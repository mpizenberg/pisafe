package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The client ID, endpoints, and authorize parameters must match what the
// pinned Pi AI OpenAI Codex flow sends; the redirect URI is registered with
// the provider and cannot vary.
const (
	clientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthScope     = "openid profile email offline_access"
	loginWait      = 10 * time.Minute
	tokenBodyLimit = 1 << 20
)

// Endpoints carries the OAuth URLs so tests can substitute a local stub; the
// zero value is never valid.
type Endpoints struct {
	Authorize   string
	Token       string
	RedirectURI string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		Authorize:   "https://auth.openai.com/oauth/authorize",
		Token:       "https://auth.openai.com/oauth/token",
		RedirectURI: "http://localhost:1455/auth/callback",
	}
}

// Login runs the browser authorization-code flow with PKCE: it serves the
// registered localhost callback, hands the authorize URL to openURL, and
// exchanges the returned code. The credential is returned, not persisted.
func Login(
	ctx context.Context,
	endpoints Endpoints,
	out io.Writer,
	openURL func(string) error,
) (Credential, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return Credential{}, err
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return Credential{}, fmt.Errorf("generate login state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	callback, err := url.Parse(endpoints.RedirectURI)
	if err != nil || callback.Port() == "" || callback.Path == "" {
		return Credential{}, fmt.Errorf("invalid OAuth redirect URI %q", endpoints.RedirectURI)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+callback.Port())
	if err != nil {
		return Credential{}, fmt.Errorf(
			"listen on the registered login callback port (close any other login attempt): %w",
			err,
		)
	}
	defer listener.Close()

	type callbackResult struct {
		code string
		err  error
	}
	results := make(chan callbackResult, 1)
	deliver := func(result callbackResult) {
		select {
		case results <- result:
		default:
		}
	}
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != callback.Path {
				http.NotFound(writer, request)
				return
			}
			query := request.URL.Query()
			if message := query.Get("error"); message != "" {
				if description := query.Get("error_description"); description != "" {
					message += ": " + description
				}
				writeLoginPage(writer, http.StatusBadRequest, "Login failed", message)
				deliver(callbackResult{err: fmt.Errorf("authorization failed: %s", message)})
				return
			}
			code := query.Get("code")
			if code == "" ||
				subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
				writeLoginPage(
					writer,
					http.StatusBadRequest,
					"Login failed",
					"The callback did not match this login attempt.",
				)
				deliver(callbackResult{err: errors.New("authorization callback state mismatch")})
				return
			}
			writeLoginPage(
				writer,
				http.StatusOK,
				"Login complete",
				"You can close this tab and return to the terminal.",
			)
			deliver(callbackResult{code: code})
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	authorize, err := url.Parse(endpoints.Authorize)
	if err != nil {
		return Credential{}, fmt.Errorf("invalid OAuth authorize URL: %w", err)
	}
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {endpoints.RedirectURI},
		"scope":                      {oauthScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"pi"},
	}
	authorize.RawQuery = query.Encode()
	fmt.Fprintf(out, "Open this URL to authorize pisafe:\n\n  %s\n\n", authorize)
	if openURL != nil {
		if err := openURL(authorize.String()); err != nil {
			fmt.Fprintf(out, "Could not open a browser automatically: %v\n", err)
		}
	}
	fmt.Fprintln(out, "Waiting for the browser login to finish...")

	timeout := time.NewTimer(loginWait)
	defer timeout.Stop()
	var code string
	select {
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	case <-timeout.C:
		return Credential{}, errors.New("browser login timed out")
	case result := <-results:
		if result.err != nil {
			return Credential{}, result.err
		}
		code = result.code
	}

	return exchangeToken(ctx, endpoints.Token, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {endpoints.RedirectURI},
	})
}

// Refresh exchanges the rotating refresh token for a fresh credential. The
// caller must persist the result: the previous refresh token may already be
// invalid once this returns.
func Refresh(ctx context.Context, endpoints Endpoints, credential Credential) (Credential, error) {
	return exchangeToken(ctx, endpoints.Token, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.Refresh},
		"client_id":     {clientID},
	})
}

func exchangeToken(ctx context.Context, tokenURL string, form url.Values) (Credential, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return Credential{}, fmt.Errorf("build token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return Credential{}, fmt.Errorf("chatgpt token request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, tokenBodyLimit))
	if err != nil {
		return Credential{}, fmt.Errorf("read token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Credential{}, fmt.Errorf(
			"chatgpt token endpoint returned %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	var parsed struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Credential{}, fmt.Errorf("parse token response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" || parsed.ExpiresIn <= 0 {
		return Credential{}, errors.New("token response is missing required fields")
	}
	accountID, err := extractAccountID(parsed.AccessToken)
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		Access:    parsed.AccessToken,
		Refresh:   parsed.RefreshToken,
		Expires:   time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second),
		AccountID: accountID,
	}, nil
}

func generatePKCE() (verifier, challenge string, err error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge, nil
}

// writeLoginPage escapes both strings because the failure branch echoes
// provider-supplied query parameters.
func writeLoginPage(writer http.ResponseWriter, status int, title, detail string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	fmt.Fprintf(
		writer,
		"<!doctype html><meta charset=\"utf-8\"><title>pisafe</title>"+
			"<body style=\"font-family: system-ui; margin: 4rem\">"+
			"<h1>%s</h1><p>%s</p></body>",
		html.EscapeString(title),
		html.EscapeString(detail),
	)
}
