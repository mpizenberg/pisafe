package chatgpt

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/mpizenberg/pisafe/internal/broker"
)

// The catalog mirrors the pinned Pi AI Codex model list, including context
// windows and subscription cost rates, with the per-model api, provider, and
// baseUrl fields stripped so nothing can route a run around the broker.
//
//go:embed models.json
var modelCatalog []byte

// Name is what this upstream is called on the command line, in a run's
// models.json, and in the relay path that reaches it.
const Name = "chatgpt"

// provider is the brokered ChatGPT subscription upstream.
func provider(source *Source) (*broker.Provider, error) {
	var models []json.RawMessage
	if err := json.Unmarshal(modelCatalog, &models); err != nil {
		return nil, fmt.Errorf("parse embedded chatgpt model catalog: %w", err)
	}
	return &broker.Provider{
		Name:        Name,
		Upstream:    &url.URL{Scheme: "https", Host: "chatgpt.com", Path: "/backend-api"},
		API:         broker.APIOpenAICodexResponses,
		Models:      models,
		Credentials: source,
	}, nil
}

// LoadProvider returns the ChatGPT provider backed by the Keychain credential,
// or nil when no login is stored. Whether a login exists is all it asks: the
// credential itself is read by the Source, the first time a relayed request
// needs it, which is the only thing pisafe reads a stored secret for.
func LoadProvider(ctx context.Context) (*broker.Provider, error) {
	keychain := NewKeychain()
	stored, err := keychain.Has(ctx)
	if err != nil {
		return nil, err
	}
	if !stored {
		return nil, nil
	}
	return provider(NewSource(keychain, DefaultEndpoints()))
}
