// Package broker implements the Mac-side inference relay. Provider
// credentials stay on the Mac; runs receive only a revocable run-scoped
// capability that this broker validates on every request.
package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const (
	APIAnthropicMessages = "anthropic-messages"
	APIOpenAICompletions = "openai-completions"
	APIOpenAIResponses   = "openai-responses"

	upstreamEnvironment = "PISAFE_INFERENCE_UPSTREAM"
	apiEnvironment      = "PISAFE_INFERENCE_API"
	keyEnvironment      = "PISAFE_INFERENCE_KEY"
	modelsEnvironment   = "PISAFE_INFERENCE_MODELS"
)

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// Provider is the Mac-side upstream a broker forwards to. Until the pisafe
// login flow exists, it is configured through PISAFE_INFERENCE_* environment
// variables; the key never enters the VM or a run.
type Provider struct {
	Upstream *url.URL
	API      string
	Key      string
	Models   []string
}

// FromEnvironment returns nil when no provider is configured, and an error
// when a partial or invalid configuration would otherwise fail silently.
func FromEnvironment() (*Provider, error) {
	upstream := strings.TrimSpace(os.Getenv(upstreamEnvironment))
	api := strings.TrimSpace(os.Getenv(apiEnvironment))
	key := os.Getenv(keyEnvironment)
	models := strings.TrimSpace(os.Getenv(modelsEnvironment))
	if upstream == "" && api == "" && key == "" && models == "" {
		return nil, nil
	}
	if upstream == "" || api == "" || key == "" || models == "" {
		return nil, fmt.Errorf(
			"incomplete inference provider: set all of %s, %s, %s, and %s",
			upstreamEnvironment, apiEnvironment, keyEnvironment, modelsEnvironment,
		)
	}
	parsed, err := parseUpstreamURL(upstream)
	if err != nil {
		return nil, err
	}
	if api != APIAnthropicMessages && api != APIOpenAICompletions && api != APIOpenAIResponses {
		return nil, fmt.Errorf(
			"unsupported inference API %q; expected %s, %s, or %s",
			api, APIAnthropicMessages, APIOpenAICompletions, APIOpenAIResponses,
		)
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("inference provider key contains control characters")
	}
	var modelIDs []string
	for _, model := range strings.Split(models, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if !modelIDPattern.MatchString(model) {
			return nil, fmt.Errorf("invalid inference model ID %q", model)
		}
		modelIDs = append(modelIDs, model)
	}
	if len(modelIDs) == 0 {
		return nil, fmt.Errorf("%s must list at least one model ID", modelsEnvironment)
	}
	return &Provider{Upstream: parsed, API: api, Key: key, Models: modelIDs}, nil
}

func parseUpstreamURL(upstream string) (*url.URL, error) {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("invalid inference upstream URL: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("inference upstream must be scheme://host[/prefix]")
	}
	loopback := false
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	} else {
		loopback = parsed.Hostname() == "localhost"
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, fmt.Errorf("inference upstream must use https")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed, nil
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
	default:
		return "/v1/responses"
	}
}

func (provider Provider) upstreamEndpoint() string {
	return provider.Upstream.String() + provider.CanonicalPath()
}

// runBaseURL is the models.json baseUrl inside a run. The Anthropic client
// appends /v1/messages itself while the OpenAI clients expect the /v1 prefix
// in the base URL.
func (provider Provider) runBaseURL() string {
	base := fmt.Sprintf("http://%s:%d", lima.BrokerAddress, lima.BrokerPort)
	if provider.API == APIAnthropicMessages {
		return base
	}
	return base + "/v1"
}

// ModelsJSON renders the ~/.pi/agent/models.json content for one run. The
// only secret it contains is that run's revocable capability.
func (provider Provider) ModelsJSON(capability string) ([]byte, error) {
	if !runstate.ValidInferenceCapability(capability) {
		return nil, errors.New("models configuration requires a valid run capability")
	}
	type model struct {
		ID string `json:"id"`
	}
	type providerEntry struct {
		Name    string  `json:"name"`
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Models  []model `json:"models"`
	}
	models := make([]model, 0, len(provider.Models))
	for _, id := range provider.Models {
		models = append(models, model{ID: id})
	}
	content := struct {
		Providers map[string]providerEntry `json:"providers"`
	}{
		Providers: map[string]providerEntry{
			"pisafe": {
				Name:    "pisafe brokered inference",
				BaseURL: provider.runBaseURL(),
				API:     provider.API,
				APIKey:  capability,
				Models:  models,
			},
		},
	}
	encoded, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode models configuration: %w", err)
	}
	return append(encoded, '\n'), nil
}

// Describe summarizes the provider without exposing its key.
func (provider Provider) Describe() string {
	return provider.API + " via " + provider.Upstream.Redacted() +
		" (" + strconv.Itoa(len(provider.Models)) + " model(s))"
}
