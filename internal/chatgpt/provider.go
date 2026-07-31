package chatgpt

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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

// Provider is the brokered ChatGPT subscription upstream.
func Provider(source *Source) (*broker.Provider, error) {
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

// LoadProvider returns the ChatGPT provider backed by the Keychain
// credential, or nil when no login is stored.
func LoadProvider(ctx context.Context) (*broker.Provider, error) {
	keychain := NewKeychain()
	if _, err := keychain.Load(ctx); err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return nil, nil
		}
		return nil, err
	}
	return Provider(NewSource(keychain, DefaultEndpoints()))
}
