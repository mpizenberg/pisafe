package apikey

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/keychain"
)

// Source presents one stored key the way its API expects it. The key is read
// on the first relayed request rather than when the provider is configured, so
// starting a run reads no secret at all.
type Source struct {
	name    string
	api     string
	secrets secretStore

	mu  sync.Mutex
	key string
}

func newSource(name, api string, secrets secretStore) *Source {
	return &Source{name: name, api: api, secrets: secrets}
}

func (source *Source) UpstreamAuth(ctx context.Context) (http.Header, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.key == "" {
		secret, err := source.secrets.Load(ctx, source.name)
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, fmt.Errorf(
				"no key is stored for %s; run: pisafe login %s", source.name, source.name,
			)
		}
		if err != nil {
			return nil, err
		}
		source.key = strings.TrimSpace(string(secret))
		if source.key == "" {
			return nil, fmt.Errorf("the stored key for %s is empty", source.name)
		}
	}
	return broker.UpstreamKeyAuth(source.api, source.key)
}
