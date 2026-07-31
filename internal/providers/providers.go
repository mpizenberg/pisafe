// Package providers assembles every inference upstream configured on this Mac
// into the one catalog the broker relays and every run is told about. Each kind
// of upstream is logged in to and stored its own way; all they have in common
// is the interface the broker forwards through, which is why the knowledge of
// what is configured lives here rather than in any one of them.
package providers

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/mpizenberg/pisafe/internal/apikey"
	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/chatgpt"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// Load returns the configured upstreams. An empty catalog is not an error: a
// Mac with no login is a Mac whose runs have no inference, which pisafe reports
// rather than refuses.
func Load(ctx context.Context) (broker.Catalog, error) {
	var catalog broker.Catalog
	subscription, err := chatgpt.LoadProvider(ctx)
	if err != nil {
		return nil, err
	}
	if subscription != nil {
		catalog = append(catalog, *subscription)
	}
	store, err := keyStore()
	if err != nil {
		return nil, err
	}
	keyed, err := store.Providers()
	if err != nil {
		return nil, err
	}
	return append(catalog, keyed...), nil
}

// Add stores one API-key login.
func Add(ctx context.Context, record apikey.Record, key string) error {
	if record.Name == chatgpt.Name {
		return fmt.Errorf("%s is logged in to with a browser, not a key", chatgpt.Name)
	}
	store, err := keyStore()
	if err != nil {
		return err
	}
	return store.Save(ctx, record, key)
}

// Remove takes one login away, whichever kind it is. A name nothing is stored
// under is an error rather than a silent success, because the user asking for
// it believes something is there.
func Remove(ctx context.Context, name string) error {
	if name == chatgpt.Name {
		subscription, err := chatgpt.LoadProvider(ctx)
		if err != nil {
			return err
		}
		if subscription == nil {
			return unknownLogin(name)
		}
		return chatgpt.NewKeychain().Forget(ctx)
	}
	store, err := keyStore()
	if err != nil {
		return err
	}
	// Whether the record still describes a reachable upstream is deliberately
	// not asked: the reason to remove one is often that it does not.
	stored, err := store.Has(name)
	if err != nil {
		return err
	}
	if !stored {
		return unknownLogin(name)
	}
	return store.Remove(ctx, name)
}

func unknownLogin(name string) error {
	return fmt.Errorf("no login named %q is stored", name)
}

// keyStore files the API-key records beside the run records, under the state
// directory that already holds everything else pisafe keeps about this Mac.
func keyStore() (apikey.Store, error) {
	root, err := runstate.DefaultRoot()
	if err != nil {
		return apikey.Store{}, err
	}
	return apikey.NewStore(filepath.Join(root, "providers")), nil
}
