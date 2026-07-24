package broker

import (
	"encoding/json"
	"strings"
	"testing"
)

func setProviderEnvironment(t *testing.T, upstream, api, key, models string) {
	t.Helper()
	t.Setenv(upstreamEnvironment, upstream)
	t.Setenv(apiEnvironment, api)
	t.Setenv(keyEnvironment, key)
	t.Setenv(modelsEnvironment, models)
}

func testCapability() string {
	return "pisafe-cap-" + strings.Repeat("ab", 32)
}

func TestFromEnvironmentUnsetMeansNoProvider(t *testing.T) {
	setProviderEnvironment(t, "", "", "", "")
	provider, err := FromEnvironment()
	if err != nil || provider != nil {
		t.Fatalf("provider = %#v, err = %v", provider, err)
	}
}

func TestFromEnvironmentRejectsPartialOrInvalidConfiguration(t *testing.T) {
	for name, environment := range map[string][4]string{
		"missing key":    {"https://api.anthropic.com", APIAnthropicMessages, "", "claude-x"},
		"missing models": {"https://api.anthropic.com", APIAnthropicMessages, "secret", ""},
		"unknown api":    {"https://api.anthropic.com", "grpc", "secret", "claude-x"},
		"plain http":     {"http://api.anthropic.com", APIAnthropicMessages, "secret", "claude-x"},
		"query upstream": {"https://api.anthropic.com?x=1", APIAnthropicMessages, "secret", "claude-x"},
		"userinfo":       {"https://user@api.anthropic.com", APIAnthropicMessages, "secret", "claude-x"},
		"bad model":      {"https://api.anthropic.com", APIAnthropicMessages, "secret", "model with space"},
		"newline in key": {"https://api.anthropic.com", APIAnthropicMessages, "se\ncret", "claude-x"},
		"only commas":    {"https://api.anthropic.com", APIAnthropicMessages, "secret", ", ,"},
	} {
		setProviderEnvironment(t, environment[0], environment[1], environment[2], environment[3])
		if _, err := FromEnvironment(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestFromEnvironmentAllowsLoopbackHTTPForTesting(t *testing.T) {
	setProviderEnvironment(
		t,
		"http://127.0.0.1:8999",
		APIOpenAICompletions,
		"secret",
		"model-a, model-b",
	)
	provider, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.Models) != 2 || provider.Models[0] != "model-a" {
		t.Fatalf("models = %#v", provider.Models)
	}
}

func TestCanonicalPathAndEndpointPerAPI(t *testing.T) {
	for api, expected := range map[string][2]string{
		APIAnthropicMessages: {"/v1/messages", "https://upstream.example/v1/messages"},
		APIOpenAICompletions: {"/v1/chat/completions", "https://upstream.example/v1/chat/completions"},
		APIOpenAIResponses:   {"/v1/responses", "https://upstream.example/v1/responses"},
	} {
		setProviderEnvironment(t, "https://upstream.example/", api, "secret", "model-a")
		provider, err := FromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		if provider.CanonicalPath() != expected[0] {
			t.Errorf("%s path = %q", api, provider.CanonicalPath())
		}
		if provider.upstreamEndpoint() != expected[1] {
			t.Errorf("%s endpoint = %q", api, provider.upstreamEndpoint())
		}
	}
}

func TestModelsJSONContainsOnlyTheRunCapability(t *testing.T) {
	setProviderEnvironment(
		t,
		"https://api.anthropic.com",
		APIAnthropicMessages,
		"upstream-secret",
		"claude-x",
	)
	provider, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ModelsJSON("garbage"); err == nil {
		t.Fatal("invalid capability was accepted")
	}
	content, err := provider.ModelsJSON(testCapability())
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
	entry, ok := parsed.Providers["pisafe"]
	if !ok {
		t.Fatalf("parsed = %#v", parsed)
	}
	if entry.BaseURL != "http://192.0.2.1:18080" ||
		entry.API != APIAnthropicMessages ||
		entry.APIKey != testCapability() ||
		len(entry.Models) != 1 || entry.Models[0].ID != "claude-x" {
		t.Fatalf("entry = %#v", entry)
	}

	// The OpenAI clients expect the /v1 prefix inside the base URL.
	setProviderEnvironment(
		t,
		"https://api.openai.com",
		APIOpenAIResponses,
		"upstream-secret",
		"gpt-x",
	)
	provider, err = FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	content, err = provider.ModelsJSON(testCapability())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"baseUrl": "http://192.0.2.1:18080/v1"`) {
		t.Fatalf("content = %s", content)
	}
}
