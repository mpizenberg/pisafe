// Package broker implements the Mac-side inference relay. Provider
// credentials stay on the Mac; runs receive only a revocable run-scoped
// capability that this broker validates on every request.
package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const (
	APIAnthropicMessages    = "anthropic-messages"
	APIOpenAICompletions    = "openai-completions"
	APIOpenAIResponses      = "openai-responses"
	APIOpenAICodexResponses = "openai-codex-responses"
)

// CredentialSource supplies the headers that authenticate one relayed
// request upstream. Implementations may refresh and persist tokens; they
// must never derive anything from the run's request.
type CredentialSource interface {
	UpstreamAuth(ctx context.Context) (http.Header, error)
}

// Provider is the Mac-side upstream a broker forwards to. Models are
// complete Pi models.json model definitions so runs see curated context and
// cost metadata instead of Pi's defaults.
type Provider struct {
	Upstream    *url.URL
	API         string
	Models      []json.RawMessage
	Credentials CredentialSource
}

// CanonicalPath is the only request path the broker relays. It matches what
// the pinned Pi client emits for the configured API against the models.json
// base URL written into each run.
func (provider Provider) CanonicalPath() string {
	switch provider.API {
	case APIAnthropicMessages:
		return "/v1/messages"
	case APIOpenAICompletions:
		return "/v1/chat/completions"
	case APIOpenAICodexResponses:
		return "/codex/responses"
	default:
		return "/v1/responses"
	}
}

func (provider Provider) upstreamEndpoint() string {
	return provider.Upstream.String() + provider.CanonicalPath()
}

// runBaseURL is the models.json baseUrl inside a run. The Anthropic and
// Codex clients append their full request paths themselves while the OpenAI
// clients expect the /v1 prefix in the base URL.
func (provider Provider) runBaseURL() string {
	base := fmt.Sprintf("http://%s:%d", lima.BrokerAddress, lima.BrokerPort)
	if provider.API == APIOpenAICompletions || provider.API == APIOpenAIResponses {
		return base + "/v1"
	}
	return base
}

// runAPIKey is the models.json apiKey inside a run. The pinned Pi Codex
// client refuses an apiKey that does not parse as a JWT and derives its
// chatgpt-account-id header from one of its claims, so the capability is
// wrapped in an unsigned JWT shape: a placeholder account ID in the payload
// (the broker sets the real one upstream) and the capability riding as the
// signature segment, which nothing decodes.
func (provider Provider) runAPIKey(capability string) string {
	if provider.API != APIOpenAICodexResponses {
		return capability
	}
	header := base64.StdEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.StdEncoding.EncodeToString(
		[]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"pisafe"}}`),
	)
	return header + "." + payload + "." + capability
}

// presentedCapability undoes runAPIKey: the capability itself never contains
// a dot, so the last segment of a wrapped token is the capability.
func presentedCapability(token string) string {
	if index := strings.LastIndexByte(token, '.'); index >= 0 {
		return token[index+1:]
	}
	return token
}

// ModelsJSON renders the ~/.pi/agent/models.json content for one run. The
// only secret it contains is that run's revocable capability.
func (provider Provider) ModelsJSON(capability string) ([]byte, error) {
	if !runstate.ValidInferenceCapability(capability) {
		return nil, errors.New("models configuration requires a valid run capability")
	}
	if len(provider.Models) == 0 {
		return nil, errors.New("inference provider lists no models")
	}
	type providerEntry struct {
		Name    string            `json:"name"`
		BaseURL string            `json:"baseUrl"`
		API     string            `json:"api"`
		APIKey  string            `json:"apiKey"`
		Models  []json.RawMessage `json:"models"`
	}
	content := struct {
		Providers map[string]providerEntry `json:"providers"`
	}{
		Providers: map[string]providerEntry{
			"pisafe": {
				Name:    "pisafe brokered inference",
				BaseURL: provider.runBaseURL(),
				API:     provider.API,
				APIKey:  provider.runAPIKey(capability),
				Models:  provider.Models,
			},
		},
	}
	encoded, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode models configuration: %w", err)
	}
	return append(encoded, '\n'), nil
}

// Describe summarizes the provider without exposing credentials.
func (provider Provider) Describe() string {
	return provider.API + " via " + provider.Upstream.Redacted() +
		" (" + strconv.Itoa(len(provider.Models)) + " model(s))"
}
