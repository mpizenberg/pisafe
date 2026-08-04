package apikey

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/keychain"
	"github.com/mpizenberg/pisafe/internal/piagent"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// fakeSecrets stands in for the Keychain and counts reads, because when a key
// is read is part of what this package is for.
type fakeSecrets struct {
	stored map[string]string
	reads  int
}

func (secrets *fakeSecrets) Save(_ context.Context, account string, secret []byte) error {
	secrets.stored[account] = string(secret)
	return nil
}

func (secrets *fakeSecrets) Load(_ context.Context, account string) ([]byte, error) {
	secrets.reads++
	secret, ok := secrets.stored[account]
	if !ok {
		return nil, keychain.ErrNotFound
	}
	return []byte(secret), nil
}

func (secrets *fakeSecrets) Delete(_ context.Context, account string) error {
	delete(secrets.stored, account)
	return nil
}

func testStore(t *testing.T) (Store, *fakeSecrets) {
	t.Helper()
	secrets := &fakeSecrets{stored: map[string]string{}}
	return Store{root: filepath.Join(t.TempDir(), "providers"), secrets: secrets}, secrets
}

// A provider pisafe knows records only its name, so an upgrade that adds a
// model or moves an endpoint reaches a key stored long before it.
func TestABuiltinLoginRecordsNothingThatCouldGoStale(t *testing.T) {
	store, secrets := testStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Record{Name: "anthropic"}, "  sk-ant-key\n"); err != nil {
		t.Fatal(err)
	}

	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].URL != "" || records[0].API != "" ||
		len(records[0].Models) != 0 {
		t.Fatalf("records = %#v", records)
	}
	content, err := os.ReadFile(filepath.Join(store.root, "anthropic.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "sk-ant-key") {
		t.Fatalf("the key was written to disk: %s", content)
	}
	if secrets.stored["anthropic"] != "sk-ant-key" {
		t.Fatalf("stored key = %q", secrets.stored["anthropic"])
	}

	catalog, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	provider := catalog[0]
	if provider.Name != "anthropic" ||
		provider.Upstream.String() != "https://api.anthropic.com" ||
		provider.API != broker.APIAnthropicMessages {
		t.Fatalf("provider = %#v", provider)
	}
	if len(provider.Models) == 0 {
		t.Fatal("the embedded catalog did not reach the provider")
	}
	// Configuring a run renders models.json and nothing else, so it must not
	// have needed the key.
	if secrets.reads != 0 {
		t.Fatalf("the key was read %d time(s) to configure a provider", secrets.reads)
	}
}

// The record names an upstream a sweep of the release cannot describe, so it
// has to carry one — but a record naming something pisafe does know may not
// redirect it.
func TestACustomEndpointCarriesItsOwnAndNeverRedirectsAKnownOne(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	models, _, err := ParseModels([]byte(`[{"id":"local-a","contextWindow":8192}]`))
	if err != nil {
		t.Fatal(err)
	}
	custom := Record{
		Name:   "local",
		URL:    "http://127.0.0.1:11434",
		API:    broker.APIOpenAICompletions,
		Models: models,
	}
	if err := store.Save(ctx, custom, "sk-local"); err != nil {
		t.Fatal(err)
	}
	// A record hand-edited to point a name pisafe knows somewhere else must be
	// ignored, or a stored key would follow the edit to another host.
	redirected := Record{
		Version: recordVersion,
		Name:    "openai",
		URL:     "https://elsewhere.example",
		API:     broker.APIOpenAICompletions,
	}
	content, err := json.Marshal(redirected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "openai.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]broker.Provider{}
	for _, provider := range catalog {
		byName[provider.Name] = provider
	}
	if got := byName["local"]; got.Upstream.String() != "http://127.0.0.1:11434" ||
		got.API != broker.APIOpenAICompletions || len(got.Models) != 1 {
		t.Fatalf("custom provider = %#v", got)
	}
	if got := byName["openai"]; got.Upstream.String() != "https://api.openai.com" ||
		got.API != broker.APIOpenAIResponses {
		t.Fatalf("a known provider followed the record: %#v", got)
	}
}

func TestALoginIsRemovableBecauseARecordNamesItsKey(t *testing.T) {
	store, secrets := testStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Record{Name: "openai"}, "sk-openai"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(ctx, "openai"); err != nil {
		t.Fatal(err)
	}
	if len(secrets.stored) != 0 {
		t.Fatalf("the key outlived the login: %#v", secrets.stored)
	}
	records, err := store.List()
	if err != nil || len(records) != 0 {
		t.Fatalf("records = %#v err = %v", records, err)
	}
	// A removal interrupted halfway is repeated, so it may not object to
	// having nothing left to do.
	if err := store.Remove(ctx, "openai"); err != nil {
		t.Fatal(err)
	}

	// A record the rules no longer accept is exactly the one a user wants
	// gone, so finding it must not depend on it still being usable.
	if err := os.WriteFile(
		filepath.Join(store.root, "stale.json"),
		[]byte(`{"version":1,"name":"stale","url":"nonsense"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("an unusable record was listed")
	}
	stored, err := store.Has("stale")
	if err != nil || !stored {
		t.Fatalf("stored = %v err = %v", stored, err)
	}
	if err := store.Remove(ctx, "stale"); err != nil {
		t.Fatal(err)
	}
	if stored, err := store.Has("stale"); err != nil || stored {
		t.Fatalf("stored = %v err = %v", stored, err)
	}
}

func TestAKeyIsReadOnlyWhenARequestIsRelayed(t *testing.T) {
	store, secrets := testStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Record{Name: "anthropic"}, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Record{Name: "openai"}, "sk-openai"); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	// Each API presents its key its own way, and never the other's.
	want := map[string][2]string{
		"anthropic": {"X-Api-Key", "sk-ant"},
		"openai":    {"Authorization", "Bearer sk-openai"},
	}
	for _, provider := range catalog {
		headers, err := provider.Credentials.UpstreamAuth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expected := want[provider.Name]
		if headers.Get(expected[0]) != expected[1] || len(headers) != 1 {
			t.Fatalf("%s headers = %#v", provider.Name, headers)
		}
	}
	if secrets.reads != 2 {
		t.Fatalf("reads = %d", secrets.reads)
	}
	// A relayed request must not cost a Keychain read every time.
	if _, err := catalog[0].Credentials.UpstreamAuth(ctx); err != nil {
		t.Fatal(err)
	}
	if secrets.reads != 2 {
		t.Fatalf("reads after a repeat = %d", secrets.reads)
	}

	// A record whose key is missing is what an interrupted login leaves, and it
	// has to say so rather than relay without one.
	delete(secrets.stored, "anthropic")
	fresh, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh[0].Credentials.UpstreamAuth(ctx); err == nil ||
		!strings.Contains(err.Error(), "pisafe login anthropic") {
		t.Fatalf("err = %v", err)
	}
}

// pisafe appends the client's own API path, so an endpoint pasted from a
// provider's documentation — which always ends in /v1 — would be requested
// twice over and answered only by the upstream's own 404.
func TestAnEndpointThatWouldBeRequestedTwiceOverIsRefused(t *testing.T) {
	models, _, err := ParseModels([]byte(`[{"id":"a"}]`))
	if err != nil {
		t.Fatal(err)
	}
	for endpoint, allowed := range map[string]bool{
		"https://gateway.example":              true,
		"https://gateway.example/openai":       true,
		"https://gateway.example/v1":           false,
		"https://gateway.example/v1/":          false,
		"https://gateway.example/openai/v1":    false,
		"https://gateway.example/v1/something": true,
	} {
		record := Record{
			Name: "gateway", URL: endpoint, API: broker.APIOpenAIResponses, Models: models,
		}
		if allowed != (record.Validate() == nil) {
			t.Errorf("%q: err = %v", endpoint, record.Validate())
		}
	}
	// The segment that would double depends on the API, so anthropic-messages
	// rejects the same /v1 and openai-codex-responses does not.
	codex := Record{
		Name: "gateway", URL: "https://gateway.example/v1",
		API: broker.APIAnthropicMessages, Models: models,
	}
	if codex.Validate() == nil {
		t.Error("a doubled /v1 was accepted for anthropic-messages")
	}
}

func TestPlaintextEndpointsAreRefusedUnlessTheyStayOnThisMac(t *testing.T) {
	for endpoint, allowed := range map[string]bool{
		"https://api.example.com":        true,
		"https://api.example.com/v1":     true,
		"http://localhost:11434/v1":      true,
		"http://127.0.0.1:11434/v1":      true,
		"http://[::1]:11434/v1":          true,
		"http://192.168.1.5:11434/v1":    false,
		"http://api.example.com":         false,
		"ftp://api.example.com":          false,
		"https://user:pass@example.com":  false,
		"https://api.example.com?key=hi": false,
		"not a url at all":               false,
		"":                               false,
	} {
		err := validateUpstream(endpoint)
		if allowed != (err == nil) {
			t.Errorf("%q: err = %v", endpoint, err)
		}
	}
}

// A model entry naming an endpoint of its own would reach the provider
// directly, so the one thing a declared list may not do is route.
func TestDeclaredModelsCannotRouteAroundTheBroker(t *testing.T) {
	models, stripped, err := ParseModels([]byte(`[
		{"id":"a","api":"openai-responses","provider":"openai","baseUrl":"https://api.openai.com"},
		{"id":"b","headers":{"Authorization":"Bearer leaked"}},
		{"id":"c","contextWindow":1000}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if stripped != 2 {
		t.Fatalf("stripped = %d", stripped)
	}
	for _, model := range models {
		var fields map[string]any
		if err := json.Unmarshal(model, &fields); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range routingFields {
			if _, present := fields[forbidden]; present {
				t.Errorf("model %v kept %q", fields["id"], forbidden)
			}
		}
	}
	if !strings.Contains(string(models[2]), "1000") {
		t.Fatalf("a model lost what it did say: %s", models[2])
	}

	for name, content := range map[string]string{
		"not an array":        `{"id":"a"}`,
		"empty":               `[]`,
		"model without an id": `[{"name":"a"}]`,
		"id is not text":      `[{"id":7}]`,
	} {
		if _, _, err := ParseModels([]byte(content)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestARecordThatCannotDescribeItsUpstreamIsRefused(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	models, _, err := ParseModels([]byte(`[{"id":"a"}]`))
	if err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]Record{
		"no name":       {Name: ""},
		"unusable name": {Name: "Not A Name"},
		"custom without endpoint": {Name: "local", API: broker.APIOpenAIResponses,
			Models: models},
		"custom without models": {
			Name: "local", URL: "https://x.example", API: broker.APIOpenAIResponses,
		},
		"custom with unknown api": {
			Name: "local", URL: "https://x.example", API: "grpc", Models: models,
		},
		"custom over plain http": {
			Name: "local", URL: "http://x.example", API: broker.APIOpenAIResponses,
			Models: models,
		},
	} {
		if err := store.Save(ctx, record, "sk-x"); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	if err := store.Save(ctx, Record{Name: "openai"}, "   "); err == nil {
		t.Error("an empty key was stored")
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v", records)
	}
}

// One unreadable record stops the listing rather than quietly shortening it,
// and says which, because the alternative is a run silently losing a provider.
func TestAnUnreadableRecordNamesItself(t *testing.T) {
	store, _ := testStore(t)
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"openai":    `{"version":99,"name":"openai"}`,
		"anthropic": `{"version":1,"name":"elsewhere"}`,
		"local":     `{"version":1,"name":"local"}`,
	} {
		path := filepath.Join(store.root, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.List()
		if err == nil {
			t.Errorf("%s was listed", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error does not say which record: %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
}

// The model pisafe opens runs on is offered by the keyed OpenAI catalog too, so
// which login a Mac holds does not decide what its runs start on.
func TestAKeyedOpenAIRunOpensOnAModelPisafeNames(t *testing.T) {
	store, _ := testStore(t)
	provider, err := store.provider(Record{Version: recordVersion, Name: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := runstate.NewInferenceCapability()
	if err != nil {
		t.Fatal(err)
	}
	content, err := (broker.Catalog{provider}).RunConfiguration(capability)
	if err != nil {
		t.Fatal(err)
	}
	var configuration piagent.Configuration
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Default.Provider != "openai" || !configuration.Default.Named() {
		t.Fatalf("a run opens on %#v", configuration.Default)
	}
}
