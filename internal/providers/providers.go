// Package providers assembles every inference upstream configured on this Mac
// into the one catalog the broker relays and every run is told about. Each kind
// of upstream is logged in to and stored its own way; all they have in common
// is the interface the broker forwards through, which is why the knowledge of
// what is configured lives here rather than in any one of them.
package providers

import (
	"context"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/chatgpt"
)

// Load returns the configured upstreams. An empty catalog is not an error: a
// Mac with no login is a Mac whose runs have no inference, which pisafe reports
// rather than refuses.
func Load(ctx context.Context) (broker.Catalog, error) {
	subscription, err := chatgpt.LoadProvider(ctx)
	if err != nil {
		return nil, err
	}
	var catalog broker.Catalog
	if subscription != nil {
		catalog = append(catalog, *subscription)
	}
	return catalog, nil
}
