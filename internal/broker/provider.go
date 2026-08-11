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
	"regexp"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/piagent"
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
	Name        string
	Upstream    *url.URL
	API         string
	Models      []json.RawMessage
	Credentials CredentialSource
}

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidateName bounds what an upstream may be called. One name is at once a
// path segment in the relay's own URL space, a key in every run's models.json,
// and the account its credential is filed under, so it is held to what all
// three can carry unambiguously.
func ValidateName(name string) error {
	if !providerName.MatchString(name) {
		return fmt.Errorf(
			"invalid provider name %q: use lowercase letters, digits, and hyphens", name,
		)
	}
	return nil
}

// apiShape is everything the wire format of one upstream decides. Each field
// was a switch on the API name in a different file, which is four chances for
// one API to be described four ways; adding an API is now adding a row. An API
// with no row has no path, and a route that matches nothing a client could send
// is what makes an upstream pisafe cannot relay refuse itself.
type apiShape struct {
	// path is the only request path the broker relays, matching what the pinned
	// Pi client emits against the base URL written into each run.
	path string
	// baseSuffix completes that base URL. The Anthropic and Codex clients append
	// their whole request path themselves; the OpenAI clients expect /v1 to be
	// there already.
	baseSuffix string
	// jwtAPIKey wraps the run's capability in an unsigned JWT shape. The pinned
	// Pi Codex client refuses an apiKey that does not parse as a JWT and derives
	// its chatgpt-account-id header from one of its claims, so the payload
	// carries a placeholder account ID — the broker sets the real one upstream —
	// and the capability rides as the signature segment, which nothing decodes.
	jwtAPIKey bool
	// keyHeader and keyPrefix are how the Mac's own key travels upstream.
	keyHeader string
	keyPrefix string
	// anthropicErrors renders a refusal in Anthropic's error envelope instead of
	// the OpenAI one every other client reads.
	anthropicErrors bool
}

var apiShapes = map[string]apiShape{
	APIAnthropicMessages: {
		path:            "/v1/messages",
		keyHeader:       "X-Api-Key",
		anthropicErrors: true,
	},
	APIOpenAICompletions: {
		path:       "/v1/chat/completions",
		baseSuffix: "/v1",
		keyHeader:  "Authorization",
		keyPrefix:  "Bearer ",
	},
	APIOpenAIResponses: {
		path:       "/v1/responses",
		baseSuffix: "/v1",
		keyHeader:  "Authorization",
		keyPrefix:  "Bearer ",
	},
	APIOpenAICodexResponses: {
		path:      "/codex/responses",
		jwtAPIKey: true,
		keyHeader: "Authorization",
		keyPrefix: "Bearer ",
	},
}

// CanonicalPath is the only request path the broker relays for this provider.
func (provider Provider) CanonicalPath() string {
	return apiShapes[provider.API].path
}

// UpstreamKeyAuth is how one API expects a plain key to authenticate a request
// upstream. A credential source holds the secret; the convention for spelling
// it belongs here with the rest of what the API decides.
func UpstreamKeyAuth(api, key string) (http.Header, error) {
	shape, known := apiShapes[api]
	if !known {
		return nil, fmt.Errorf("cannot authenticate upstream for unknown API %q", api)
	}
	headers := http.Header{}
	headers.Set(shape.keyHeader, shape.keyPrefix+key)
	return headers, nil
}

func (provider Provider) upstreamEndpoint() string {
	return provider.Upstream.String() + provider.CanonicalPath()
}

// route is the path a run's client requests for this provider. One relay
// serves every configured upstream over one address, so the provider's name
// leads the path its client would otherwise send on its own.
func (provider Provider) route() string {
	return "/" + provider.Name + provider.CanonicalPath()
}

// runBaseURL is the models.json baseUrl inside a run.
func (provider Provider) runBaseURL() string {
	return fmt.Sprintf(
		"http://%s:%d/%s%s",
		lima.BrokerAddress,
		lima.BrokerPort,
		provider.Name,
		apiShapes[provider.API].baseSuffix,
	)
}

// runAPIKey is the models.json apiKey inside a run.
func (provider Provider) runAPIKey(capability string) string {
	if !apiShapes[provider.API].jwtAPIKey {
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

// Describe summarizes the provider without exposing credentials.
func (provider Provider) Describe() string {
	return provider.Name + ": " + provider.API + " via " + provider.Upstream.Redacted() +
		" (" + strconv.Itoa(len(provider.Models)) + " model(s))"
}

// Catalog is every upstream configured on the Mac. A run reaches all of them
// through one relay and one capability, because a capability names the run
// rather than a provider: what separates two upstreams is the path, and the
// credential each is answered with never leaves the Mac either way.
type Catalog []Provider

// preferredModel is the model runs open on, at preferredThinking effort,
// wherever it is offered. Pi names a default of its own otherwise, from a table
// keyed by its own provider names — which are not pisafe's, so a subscription
// run would open on whatever its catalog happens to list first.
const (
	preferredModel    = "gpt-5.6-sol"
	preferredThinking = "high"
)

// RunConfiguration renders everything one run is told about inference: the
// providers it may reach and the model it opens on. The only secret it
// contains is that run's revocable capability.
func (catalog Catalog) RunConfiguration(capability string) ([]byte, error) {
	models, err := catalog.modelsJSON(capability)
	if err != nil {
		return nil, err
	}
	configuration := piagent.Configuration{Models: models, Default: catalog.defaultSelection()}
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, fmt.Errorf("encode run inference configuration: %w", err)
	}
	return encoded, nil
}

// defaultSelection is what a run opens on. The first configured upstream
// offering the preferred model decides, because a Mac with several logins has
// no better order to prefer them in than the one it lists them in.
func (catalog Catalog) defaultSelection() piagent.Selection {
	for _, provider := range catalog {
		if !provider.offers(preferredModel) {
			continue
		}
		return piagent.Selection{
			Provider: provider.Name,
			Model:    preferredModel,
			Thinking: preferredThinking,
		}
	}
	return piagent.Selection{}
}

// offers reports whether this upstream serves a model under that id.
func (provider Provider) offers(id string) bool {
	for _, model := range provider.Models {
		var definition struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(model, &definition) == nil && definition.ID == id {
			return true
		}
	}
	return false
}

// modelsJSON renders the ~/.pi/agent/models.json content for one run, one
// entry per configured upstream.
func (catalog Catalog) modelsJSON(capability string) ([]byte, error) {
	if !runstate.ValidInferenceCapability(capability) {
		return nil, errors.New("models configuration requires a valid run capability")
	}
	if len(catalog) == 0 {
		return nil, errors.New("no inference provider is configured")
	}
	type providerEntry struct {
		Name    string            `json:"name"`
		BaseURL string            `json:"baseUrl"`
		API     string            `json:"api"`
		APIKey  string            `json:"apiKey"`
		Models  []json.RawMessage `json:"models"`
	}
	entries := make(map[string]providerEntry, len(catalog))
	for _, provider := range catalog {
		if err := ValidateName(provider.Name); err != nil {
			return nil, err
		}
		if _, taken := entries[provider.Name]; taken {
			// Two upstreams under one name would share a relay path, so
			// whichever answered would be a matter of ordering.
			return nil, fmt.Errorf("two inference providers are named %q", provider.Name)
		}
		if len(provider.Models) == 0 {
			return nil, fmt.Errorf("inference provider %q lists no models", provider.Name)
		}
		if provider.CanonicalPath() == "" {
			return nil, fmt.Errorf(
				"inference provider %q speaks %q, which pisafe cannot route",
				provider.Name,
				provider.API,
			)
		}
		entries[provider.Name] = providerEntry{
			Name:    "pisafe: " + provider.Name,
			BaseURL: provider.runBaseURL(),
			API:     provider.API,
			APIKey:  provider.runAPIKey(capability),
			Models:  provider.Models,
		}
	}
	content := struct {
		Providers map[string]providerEntry `json:"providers"`
	}{Providers: entries}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("encode models configuration: %w", err)
	}
	return encoded, nil
}

// Describe summarizes each configured upstream without exposing credentials.
func (catalog Catalog) Describe() []string {
	lines := make([]string, 0, len(catalog))
	for _, provider := range catalog {
		lines = append(lines, provider.Describe())
	}
	return lines
}

// find resolves the upstream a request path names. The match is exact, so no
// provider's route can be reached by a path meant for another.
func (catalog Catalog) find(path string) (Provider, bool) {
	for _, provider := range catalog {
		if provider.route() == path {
			return provider, true
		}
	}
	return Provider{}, false
}

func (catalog Catalog) routes() []string {
	routes := make([]string, 0, len(catalog))
	for _, provider := range catalog {
		routes = append(routes, provider.route())
	}
	return routes
}
