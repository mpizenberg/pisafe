package broker

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/piagent"
)

func testCapability() string {
	return "pisafe-cap-" + strings.Repeat("ab", 32)
}

func testModels(ids ...string) []json.RawMessage {
	models := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		models = append(models, json.RawMessage(`{"id":"`+id+`"}`))
	}
	return models
}

func testProvider(t *testing.T, upstream string, api string) Provider {
	t.Helper()
	return namedProvider(t, "main", upstream, api, "upstream-secret")
}

func namedProvider(t *testing.T, name, upstream, api, secret string) Provider {
	t.Helper()
	parsed, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	header := "Authorization"
	value := "Bearer " + secret
	if api == APIAnthropicMessages {
		header, value = "X-Api-Key", secret
	}
	return Provider{
		Name:        name,
		Upstream:    parsed,
		API:         api,
		Models:      testModels("model-a"),
		Credentials: staticCredentials{header: header, value: value},
	}
}

func TestCanonicalPathAndEndpointPerAPI(t *testing.T) {
	for api, expected := range map[string][3]string{
		APIAnthropicMessages: {
			"/v1/messages", "https://upstream.example/v1/messages", "/main/v1/messages",
		},
		APIOpenAICompletions: {
			"/v1/chat/completions",
			"https://upstream.example/v1/chat/completions",
			"/main/v1/chat/completions",
		},
		APIOpenAIResponses: {
			"/v1/responses", "https://upstream.example/v1/responses", "/main/v1/responses",
		},
		APIOpenAICodexResponses: {
			"/codex/responses", "https://upstream.example/codex/responses", "/main/codex/responses",
		},
	} {
		provider := testProvider(t, "https://upstream.example", api)
		if provider.CanonicalPath() != expected[0] {
			t.Errorf("%s path = %q", api, provider.CanonicalPath())
		}
		if provider.upstreamEndpoint() != expected[1] {
			t.Errorf("%s endpoint = %q", api, provider.upstreamEndpoint())
		}
		// What the client appends is the provider's own API path, so the name
		// has to lead it for one relay to serve several upstreams.
		if provider.route() != expected[2] {
			t.Errorf("%s route = %q", api, provider.route())
		}
	}
}

// Every API pisafe names has to carry a complete row, because nothing switches
// on the name any more to notice a missing one: a constant without a row routes
// nowhere and would authenticate upstream under an empty header name.
func TestEveryNamedAPIHasAShape(t *testing.T) {
	named := []string{
		APIAnthropicMessages,
		APIOpenAICompletions,
		APIOpenAIResponses,
		APIOpenAICodexResponses,
	}
	for _, api := range named {
		shape, known := apiShapes[api]
		if !known {
			t.Errorf("%s has no shape", api)
			continue
		}
		if shape.path == "" || shape.keyHeader == "" {
			t.Errorf("%s shape = %+v", api, shape)
		}
	}
	if len(apiShapes) != len(named) {
		t.Errorf("%d shapes for %d named APIs", len(apiShapes), len(named))
	}
}

func TestUpstreamKeyAuthSpellsTheKeyTheWayEachAPIExpects(t *testing.T) {
	for api, expected := range map[string][2]string{
		APIAnthropicMessages:    {"X-Api-Key", "secret"},
		APIOpenAICompletions:    {"Authorization", "Bearer secret"},
		APIOpenAIResponses:      {"Authorization", "Bearer secret"},
		APIOpenAICodexResponses: {"Authorization", "Bearer secret"},
	} {
		headers, err := UpstreamKeyAuth(api, "secret")
		if err != nil {
			t.Fatalf("%s: %v", api, err)
		}
		if len(headers) != 1 || headers.Get(expected[0]) != expected[1] {
			t.Errorf("%s headers = %v", api, headers)
		}
	}
	// An API with no row has no convention to spell a key by, and guessing one
	// would send the Mac's secret upstream in a header nothing asked for.
	if _, err := UpstreamKeyAuth("invented", "secret"); err == nil {
		t.Error("an unknown API produced upstream credentials")
	}
}

// A catalog that cannot be routed must never reach a run: the name is what
// separates two upstreams, and a run configured with an ambiguous one would
// send a request to whichever provider happened to be checked first.
func TestRunConfigurationRefusesACatalogNoRunCouldUse(t *testing.T) {
	provider := testProvider(t, "https://upstream.example", APIAnthropicMessages)
	unnamed := provider
	unnamed.Name = "Not A Name"
	empty := provider
	empty.Models = nil
	unroutable := provider
	unroutable.API = "invented-api"
	for name, catalog := range map[string]Catalog{
		"nothing configured": nil,
		"unusable name":      {unnamed},
		"no models":          {empty},
		"one name twice":     {provider, provider},
		"unroutable API":     {unroutable},
	} {
		if _, err := catalog.RunConfiguration(testCapability()); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestModelsContainOnlyTheRunCapability(t *testing.T) {
	provider := testProvider(t, "https://api.anthropic.com", APIAnthropicMessages)
	if _, err := (Catalog{provider}).modelsJSON("garbage"); err == nil {
		t.Fatal("invalid capability was accepted")
	}
	provider.Models = testModels("claude-x")
	content, err := (Catalog{provider}).modelsJSON(testCapability())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "upstream-secret") {
		t.Fatal("run configuration leaks the upstream key")
	}
	var parsed struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatal(err)
	}
	entry, ok := parsed.Providers["main"]
	if !ok {
		t.Fatalf("parsed = %#v", parsed)
	}
	if entry.BaseURL != "http://192.0.2.1:18080/main" ||
		entry.API != APIAnthropicMessages ||
		entry.APIKey != testCapability() ||
		len(entry.Models) != 1 || entry.Models[0].ID != "claude-x" {
		t.Fatalf("entry = %#v", entry)
	}

	// The OpenAI clients expect the /v1 prefix inside the base URL.
	openai := namedProvider(t, "second", "https://api.openai.com", APIOpenAIResponses, "other-secret")
	content, err = (Catalog{provider, openai}).modelsJSON(testCapability())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Providers) != 2 {
		t.Fatalf("a run was told about %d provider(s)", len(parsed.Providers))
	}
	if parsed.Providers["second"].BaseURL != "http://192.0.2.1:18080/second/v1" {
		t.Fatalf("entry = %#v", parsed.Providers["second"])
	}
	if strings.Contains(string(content), "other-secret") {
		t.Fatal("run configuration leaks the upstream key")
	}
}

// The pinned Pi Codex client requires a JWT-shaped apiKey carrying a
// chatgpt_account_id claim it can decode with atob, and appends
// /codex/responses to the base URL itself.
func TestModelsWrapTheCodexCapabilityAsJWT(t *testing.T) {
	provider := testProvider(t, "https://chatgpt.com/backend-api", APIOpenAICodexResponses)
	content, err := (Catalog{provider}).modelsJSON(testCapability())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatal(err)
	}
	entry := parsed.Providers["main"]
	if entry.BaseURL != "http://192.0.2.1:18080/main" {
		t.Fatalf("baseUrl = %q", entry.BaseURL)
	}
	parts := strings.Split(entry.APIKey, ".")
	if len(parts) != 3 || parts[2] != testCapability() {
		t.Fatalf("apiKey = %q", entry.APIKey)
	}
	payload, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	var claims map[string]struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["https://api.openai.com/auth"].ChatGPTAccountID != "pisafe" {
		t.Fatalf("claims = %#v", claims)
	}
	if presentedCapability(entry.APIKey) != testCapability() {
		t.Fatalf("unwrapped = %q", presentedCapability(entry.APIKey))
	}
}

// What a run opens on is pisafe's to say, because Pi's own table of defaults is
// keyed by names pisafe does not use. Where the preferred model is offered by
// no configured upstream, the choice goes back to Pi rather than to an
// arbitrary entry of somebody's catalog.
func TestRunConfigurationOpensOnThePreferredModelWhereverItIsOffered(t *testing.T) {
	elsewhere := testProvider(t, "https://upstream.example", APIAnthropicMessages)
	subscription := namedProvider(
		t, "chatgpt", "https://chatgpt.com/backend-api", APIOpenAICodexResponses, "session",
	)
	subscription.Models = testModels("gpt-1", preferredModel, "gpt-2")
	keyed := namedProvider(t, "openai", "https://api.openai.com", APIOpenAIResponses, "key")
	keyed.Models = testModels(preferredModel)

	for name, expected := range map[string]piagent.Selection{
		"unoffered": {},
		"offered": {
			Provider: "chatgpt",
			Model:    preferredModel,
			Thinking: preferredThinking,
		},
	} {
		catalog := Catalog{elsewhere}
		if name == "offered" {
			catalog = Catalog{elsewhere, subscription, keyed}
		}
		content, err := catalog.RunConfiguration(testCapability())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var configuration piagent.Configuration
		if err := json.Unmarshal(content, &configuration); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if configuration.Default != expected {
			t.Errorf("%s: default = %#v, want %#v", name, configuration.Default, expected)
		}
		if err := configuration.Validate(); err != nil {
			t.Errorf("%s: a run was configured with %v", name, err)
		}
		var models struct {
			Providers map[string]json.RawMessage `json:"providers"`
		}
		if err := json.Unmarshal(configuration.Models, &models); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(models.Providers) != len(catalog) {
			t.Errorf("%s: a run was told about %d provider(s)", name, len(models.Providers))
		}
	}
}
