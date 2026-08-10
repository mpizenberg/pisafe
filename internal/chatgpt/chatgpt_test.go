package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/keychain"
	"github.com/mpizenberg/pisafe/internal/piagent"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func testAccessToken(accountID string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestExtractAccountID(t *testing.T) {
	accountID, err := extractAccountID(testAccessToken("acct-1"))
	if err != nil || accountID != "acct-1" {
		t.Fatalf("accountID = %q, err = %v", accountID, err)
	}
	for name, token := range map[string]string{
		"not a jwt":     "opaque-token",
		"empty claim":   testAccessToken(""),
		"bad payload":   "a.!!!.c",
		"wrong payload": "a." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".c",
	} {
		if _, err := extractAccountID(token); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// tokenStub answers the OAuth token endpoint for both grant types and
// records the request bodies it saw.
func tokenStub(t *testing.T, accountID string) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var requests []url.Values
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			form, err := url.ParseQuery(string(body))
			if err != nil {
				t.Errorf("token request body: %v", err)
			}
			mu.Lock()
			requests = append(requests, form)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(
				writer,
				`{"access_token":%q,"refresh_token":"refresh-next","expires_in":3600}`,
				testAccessToken(accountID),
			)
		},
	))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestLoginExchangesCallbackCode(t *testing.T) {
	tokens, requests := tokenStub(t, "acct-login")
	endpoints := Endpoints{
		Authorize:   "https://auth.example/oauth/authorize",
		Token:       tokens.URL,
		RedirectURI: "http://localhost:18455/auth/callback",
	}
	browser := func(authorizeURL string) error {
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		for _, required := range []string{
			"code_challenge", "state", "redirect_uri", "client_id",
		} {
			if query.Get(required) == "" {
				return fmt.Errorf("authorize URL misses %s", required)
			}
		}
		if query.Get("code_challenge_method") != "S256" {
			return fmt.Errorf("challenge method = %q", query.Get("code_challenge_method"))
		}
		// Simulate the provider redirecting the browser to the callback.
		go func() {
			callback := "http://127.0.0.1:18455/auth/callback?code=auth-code&state=" +
				url.QueryEscape(query.Get("state"))
			for range 50 {
				response, err := http.Get(callback)
				if err == nil {
					response.Body.Close()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		return nil
	}
	credential, err := Login(context.Background(), endpoints, io.Discard, browser)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountID != "acct-login" || credential.Refresh != "refresh-next" {
		t.Fatalf("credential = %#v", credential)
	}
	if time.Until(credential.Expires) < 55*time.Minute {
		t.Fatalf("expires = %v", credential.Expires)
	}
	if len(*requests) != 1 {
		t.Fatalf("token requests = %d", len(*requests))
	}
	form := (*requests)[0]
	if form.Get("grant_type") != "authorization_code" ||
		form.Get("code") != "auth-code" ||
		form.Get("code_verifier") == "" ||
		form.Get("redirect_uri") != endpoints.RedirectURI {
		t.Fatalf("exchange form = %v", form)
	}
}

func TestLoginRejectsStateMismatch(t *testing.T) {
	tokens, _ := tokenStub(t, "acct-x")
	endpoints := Endpoints{
		Authorize:   "https://auth.example/oauth/authorize",
		Token:       tokens.URL,
		RedirectURI: "http://localhost:18456/auth/callback",
	}
	browser := func(string) error {
		go func() {
			for range 50 {
				response, err := http.Get(
					"http://127.0.0.1:18456/auth/callback?code=auth-code&state=forged",
				)
				if err == nil {
					response.Body.Close()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		return nil
	}
	if _, err := Login(context.Background(), endpoints, io.Discard, browser); err == nil {
		t.Fatal("forged state was accepted")
	}
}

// storedLogin is what a login looks like once it is stored: the real Keychain,
// over a fake of where secrets are kept rather than of what they mean.
func storedLogin(t *testing.T, credential Credential) Keychain {
	t.Helper()
	store := Keychain{secrets: fakeSecrets{}}
	if err := store.Save(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSourceServesFreshCredentialWithoutRefresh(t *testing.T) {
	store := storedLogin(t, Credential{
		Access:    "access-1",
		Refresh:   "refresh-1",
		Expires:   time.Now().Add(time.Hour),
		AccountID: "acct-1",
	})
	// An unreachable token endpoint: a refresh of a fresh credential fails the
	// call rather than merely being counted.
	source := NewSource(store, Endpoints{Token: "http://127.0.0.1:1/unreachable"})
	headers, err := source.UpstreamAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Authorization") != "Bearer access-1" ||
		headers.Get("chatgpt-account-id") != "acct-1" {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestSourceRefreshesExpiringCredentialAndPersists(t *testing.T) {
	tokens, requests := tokenStub(t, "acct-2")
	store := storedLogin(t, Credential{
		Access:    "stale-access",
		Refresh:   "refresh-old",
		Expires:   time.Now().Add(time.Minute),
		AccountID: "acct-2",
	})
	source := NewSource(store, Endpoints{Token: tokens.URL})
	headers, err := source.UpstreamAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Authorization") != "Bearer "+testAccessToken("acct-2") {
		t.Fatalf("headers = %#v", headers)
	}
	rotated, err := store.Load(context.Background())
	if err != nil || rotated.Refresh != "refresh-next" {
		t.Fatalf("stored credential = %#v, err = %v", rotated, err)
	}
	form := (*requests)[0]
	if form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != "refresh-old" {
		t.Fatalf("refresh form = %v", form)
	}
	// The refreshed credential now serves without another round trip.
	if _, err := source.UpstreamAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 {
		t.Fatalf("token requests = %d", len(*requests))
	}
}

func TestSourcePropagatesMissingLogin(t *testing.T) {
	source := NewSource(Keychain{secrets: fakeSecrets{}}, Endpoints{})
	if _, err := source.UpstreamAuth(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
}

// fakeSecrets stands in for the Keychain. What the OAuth credential is stored
// as belongs to this package; where it is kept does not.
type fakeSecrets map[string][]byte

func (secrets fakeSecrets) Save(_ context.Context, account string, secret []byte) error {
	secrets[account] = secret
	return nil
}

func (secrets fakeSecrets) Delete(_ context.Context, account string) error {
	delete(secrets, account)
	return nil
}

func (secrets fakeSecrets) Load(_ context.Context, account string) ([]byte, error) {
	secret, ok := secrets[account]
	if !ok {
		return nil, keychain.ErrNotFound
	}
	return secret, nil
}

func TestAStoredLoginIsCompleteOrIsNotALogin(t *testing.T) {
	secrets := fakeSecrets{}
	store := Keychain{secrets: secrets}

	ctx := context.Background()
	if _, err := store.Load(ctx); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("empty keychain err = %v", err)
	}
	credential := Credential{
		Access:    testAccessToken("acct-kc"),
		Refresh:   "refresh-kc",
		Expires:   time.Now().Add(time.Hour).UTC(),
		AccountID: "acct-kc",
	}
	if err := store.Save(ctx, credential); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Refresh != credential.Refresh || loaded.AccountID != credential.AccountID ||
		!loaded.Expires.Equal(credential.Expires) {
		t.Fatalf("loaded = %#v", loaded)
	}
	if err := store.Save(ctx, Credential{Access: "only-access"}); err == nil {
		t.Fatal("incomplete credential was saved")
	}
	// A credential that lost a field in storage cannot be refreshed, so it is
	// not a login however it got that way — but it is still something stored,
	// which is what keeps it removable.
	secrets[Name] = []byte(`{"access":"a","refresh":"b"}`)
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("incomplete credential was loaded")
	}
	if stored, err := store.Has(ctx); err != nil || !stored {
		t.Fatalf("unusable credential reported as absent: %v %v", stored, err)
	}
	if err := store.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("forgotten login err = %v", err)
	}
	if stored, err := store.Has(ctx); err != nil || stored {
		t.Fatalf("forgotten login reported as stored: %v %v", stored, err)
	}
}

func TestProviderUsesEmbeddedCatalogWithoutRoutingOverrides(t *testing.T) {
	provider, err := provider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider.API != broker.APIOpenAICodexResponses ||
		provider.Upstream.String() != "https://chatgpt.com/backend-api" {
		t.Fatalf("provider = %#v", provider)
	}
	if len(provider.Models) == 0 {
		t.Fatal("catalog is empty")
	}
	for _, raw := range provider.Models {
		var model map[string]any
		if err := json.Unmarshal(raw, &model); err != nil {
			t.Fatal(err)
		}
		if model["id"] == "" || model["id"] == nil {
			t.Fatalf("model without id: %s", raw)
		}
		for _, forbidden := range []string{"api", "provider", "baseUrl", "headers"} {
			if _, ok := model[forbidden]; ok {
				t.Errorf("model %v carries %q, which could route around the broker", model["id"], forbidden)
			}
		}
	}
}

// pisafe names the model a run opens on, which holds only while the embedded
// catalog still offers it: a re-sync that dropped the id would hand the choice
// back to Pi, whose own table does not know this provider by pisafe's name.
func TestASubscriptionRunOpensOnAModelPisafeNames(t *testing.T) {
	provider, err := provider(nil)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := runstate.NewInferenceCapability()
	if err != nil {
		t.Fatal(err)
	}
	content, err := (broker.Catalog{*provider}).RunConfiguration(capability)
	if err != nil {
		t.Fatal(err)
	}
	var configuration piagent.Configuration
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Default.Provider != Name || !configuration.Default.Named() {
		t.Fatalf("a run opens on %#v", configuration.Default)
	}
}
