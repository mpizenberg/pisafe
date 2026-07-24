package chatgpt

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// refreshMargin renews the access token well before expiry so an in-flight
// inference request never races the deadline.
const refreshMargin = 5 * time.Minute

// CredentialStore abstracts the Keychain for tests.
type CredentialStore interface {
	Load(ctx context.Context) (Credential, error)
	Save(ctx context.Context, credential Credential) error
}

// Source hands the broker fresh upstream credentials. Refreshes are
// serialized, and a rotated refresh token must be persisted before use
// because the provider may invalidate its predecessor.
type Source struct {
	endpoints Endpoints
	store     CredentialStore
	now       func() time.Time

	mu         sync.Mutex
	credential Credential
	loaded     bool
}

func NewSource(store CredentialStore, endpoints Endpoints) *Source {
	return &Source{endpoints: endpoints, store: store, now: time.Now}
}

// UpstreamAuth returns the headers that authenticate one request against the
// ChatGPT backend, refreshing and persisting the credential when needed.
func (source *Source) UpstreamAuth(ctx context.Context) (http.Header, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if !source.loaded {
		credential, err := source.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		source.credential = credential
		source.loaded = true
	}
	if source.now().After(source.credential.Expires.Add(-refreshMargin)) {
		refreshed, err := Refresh(ctx, source.endpoints, source.credential)
		if err != nil {
			return nil, fmt.Errorf("refresh chatgpt credential: %w", err)
		}
		if err := source.store.Save(ctx, refreshed); err != nil {
			return nil, fmt.Errorf("persist refreshed chatgpt credential: %w", err)
		}
		source.credential = refreshed
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+source.credential.Access)
	headers.Set("Chatgpt-Account-Id", source.credential.AccountID)
	return headers, nil
}
