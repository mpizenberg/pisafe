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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/broker"
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

type fakeStore struct {
	mu         sync.Mutex
	credential Credential
	loadErr    error
	saves      int
}

func (store *fakeStore) Load(context.Context) (Credential, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return Credential{}, store.loadErr
	}
	return store.credential, nil
}

func (store *fakeStore) Save(_ context.Context, credential Credential) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.credential = credential
	store.saves++
	return nil
}

func TestSourceServesFreshCredentialWithoutRefresh(t *testing.T) {
	store := &fakeStore{credential: Credential{
		Access:    "access-1",
		Refresh:   "refresh-1",
		Expires:   time.Now().Add(time.Hour),
		AccountID: "acct-1",
	}}
	source := NewSource(store, Endpoints{Token: "http://127.0.0.1:1/unreachable"})
	headers, err := source.UpstreamAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Authorization") != "Bearer access-1" ||
		headers.Get("chatgpt-account-id") != "acct-1" {
		t.Fatalf("headers = %#v", headers)
	}
	if store.saves != 0 {
		t.Fatalf("saves = %d", store.saves)
	}
}

func TestSourceRefreshesExpiringCredentialAndPersists(t *testing.T) {
	tokens, requests := tokenStub(t, "acct-2")
	store := &fakeStore{credential: Credential{
		Access:    "stale-access",
		Refresh:   "refresh-old",
		Expires:   time.Now().Add(time.Minute),
		AccountID: "acct-2",
	}}
	source := NewSource(store, Endpoints{Token: tokens.URL})
	headers, err := source.UpstreamAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Authorization") != "Bearer "+testAccessToken("acct-2") {
		t.Fatalf("headers = %#v", headers)
	}
	if store.saves != 1 || store.credential.Refresh != "refresh-next" {
		t.Fatalf("store = %#v", store)
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
	store := &fakeStore{loadErr: ErrNotLoggedIn}
	source := NewSource(store, Endpoints{})
	if _, err := source.UpstreamAuth(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeychainRoundTripUsesStdinForSecrets(t *testing.T) {
	saved := map[string]string{}
	keychain := Keychain{execute: func(
		_ context.Context,
		stdin string,
		args ...string,
	) (string, error) {
		if len(args) == 1 && args[0] == "-i" {
			fields := strings.Fields(stdin)
			if len(fields) == 0 || fields[0] != "add-generic-password" {
				return "", fmt.Errorf("unexpected interactive command %q", stdin)
			}
			saved["secret"] = fields[len(fields)-1]
			return "", nil
		}
		if args[0] == "find-generic-password" {
			secret, ok := saved["secret"]
			if !ok {
				return "", errors.New("The specified item could not be found in the keychain.")
			}
			return secret + "\n", nil
		}
		return "", fmt.Errorf("unexpected security invocation %v", args)
	}}

	ctx := context.Background()
	if _, err := keychain.Load(ctx); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("empty keychain err = %v", err)
	}
	credential := Credential{
		Access:    testAccessToken("acct-kc"),
		Refresh:   "refresh-kc",
		Expires:   time.Now().Add(time.Hour).UTC(),
		AccountID: "acct-kc",
	}
	if err := keychain.Save(ctx, credential); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved["secret"], "refresh-kc") {
		t.Fatal("secret was not base64-wrapped")
	}
	loaded, err := keychain.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Refresh != credential.Refresh || loaded.AccountID != credential.AccountID ||
		!loaded.Expires.Equal(credential.Expires) {
		t.Fatalf("loaded = %#v", loaded)
	}
	if err := keychain.Save(ctx, Credential{Access: "only-access"}); err == nil {
		t.Fatal("incomplete credential was saved")
	}
}

func TestProviderUsesEmbeddedCatalogWithoutRoutingOverrides(t *testing.T) {
	provider, err := Provider(nil)
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
