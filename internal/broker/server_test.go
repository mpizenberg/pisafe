package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

type fakeRuns struct {
	manifests []runstate.Manifest
	err       error
}

func (runs fakeRuns) List() ([]runstate.Manifest, error) {
	return runs.manifests, runs.err
}

type staticCredentials struct {
	header string
	value  string
	err    error
}

func (credentials staticCredentials) UpstreamAuth(context.Context) (http.Header, error) {
	if credentials.err != nil {
		return nil, credentials.err
	}
	headers := http.Header{}
	headers.Set(credentials.header, credentials.value)
	return headers, nil
}

func activeRun(runID, capability string) runstate.Manifest {
	started := time.Now().UTC().Add(-time.Minute)
	deadline := started.Add(8 * time.Hour)
	return runstate.Manifest{
		RunID:               runID,
		State:               runstate.StateActive,
		InferenceCapability: capability,
		ActiveLimitSeconds:  8 * 60 * 60,
		ActiveStartedAt:     &started,
		ActiveDeadline:      &deadline,
	}
}

func TestServerRejectsMissingUnknownAndInactiveCapabilities(t *testing.T) {
	stopped := activeRun("run-b", "")
	stopped.State = runstate.StateStopped
	stopped.ActiveStartedAt = nil
	stopped.ActiveDeadline = nil
	exhausted := activeRun("run-c", "pisafe-cap-"+strings.Repeat("cd", 32))
	spent := time.Now().UTC().Add(-time.Minute)
	exhausted.ActiveDeadline = &spent
	server := NewServer(fakeRuns{manifests: []runstate.Manifest{
		activeRun("run-a", testCapability()),
		stopped,
		exhausted,
	}}, testProvider(t, "https://upstream.example", APIAnthropicMessages))

	for name, request := range map[string]*http.Request{
		"no auth": httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		"unknown capability": requestWithKey(
			"pisafe-cap-" + strings.Repeat("ef", 32),
		),
		"malformed capability": requestWithKey("not-a-capability"),
		"exhausted run capability": requestWithKey(
			"pisafe-cap-" + strings.Repeat("cd", 32),
		),
	} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d", name, recorder.Code)
		}
	}
}

func requestWithKey(key string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set("x-api-key", key)
	return request
}

func TestServerFailsClosedWithoutProviderMethodOrPath(t *testing.T) {
	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}

	recorder := httptest.NewRecorder()
	NewServer(runs, nil).ServeHTTP(recorder, requestWithKey(testCapability()))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("no provider: status = %d", recorder.Code)
	}

	server := NewServer(runs, testProvider(t, "https://upstream.example", APIAnthropicMessages))
	get := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	get.Header.Set("x-api-key", testCapability())
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, get)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d", recorder.Code)
	}

	wrongPath := httptest.NewRequest(http.MethodPost, "/v1/complete", nil)
	wrongPath.Header.Set("x-api-key", testCapability())
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, wrongPath)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("wrong path: status = %d", recorder.Code)
	}

	oversize := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	oversize.Header.Set("x-api-key", testCapability())
	oversize.ContentLength = maxRequestBytes + 1
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, oversize)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: status = %d", recorder.Code)
	}
}

func TestServerRelaysWithUpstreamCredentialAndStreams(t *testing.T) {
	var seen struct {
		path          string
		auth          string
		clientAuth    string
		version       string
		body          string
		authorization string
	}
	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			seen.path = request.URL.Path
			seen.auth = request.Header.Get("x-api-key")
			seen.authorization = request.Header.Get("Authorization")
			seen.version = request.Header.Get("anthropic-version")
			seen.body = string(body)
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			flusher := writer.(http.Flusher)
			for _, event := range []string{"event: one\n\n", "event: two\n\n"} {
				_, _ = io.WriteString(writer, event)
				flusher.Flush()
			}
		},
	))
	defer upstream.Close()

	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, testProvider(t, upstream.URL, APIAnthropicMessages))
	front := httptest.NewServer(server)
	defer front.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		front.URL+"/v1/messages",
		strings.NewReader(`{"model":"model-a"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", testCapability())
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("content-type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if string(body) != "event: one\n\nevent: two\n\n" {
		t.Fatalf("body = %q", body)
	}
	if seen.path != "/v1/messages" || seen.auth != "upstream-secret" {
		t.Fatalf("upstream saw %#v", seen)
	}
	if seen.authorization != "" {
		t.Fatal("client capability header leaked upstream")
	}
	if seen.version != "2023-06-01" || seen.body != `{"model":"model-a"}` {
		t.Fatalf("upstream saw %#v", seen)
	}
}

func TestServerUsesBearerAuthForOpenAIAndAcceptsBearerCapability(t *testing.T) {
	var authorization, apiKey string
	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			authorization = request.Header.Get("Authorization")
			apiKey = request.Header.Get("x-api-key")
			writer.WriteHeader(http.StatusOK)
		},
	))
	defer upstream.Close()

	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, testProvider(t, upstream.URL, APIOpenAIResponses))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer "+testCapability())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	if authorization != "Bearer upstream-secret" || apiKey != "" {
		t.Fatalf("upstream auth = %q %q", authorization, apiKey)
	}
}

func TestServerRefusesUpstreamRedirects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "https://evil.example", http.StatusFound)
		},
	))
	defer upstream.Close()

	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, testProvider(t, upstream.URL, APIAnthropicMessages))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestWithKey(testCapability()))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestServerCapsConcurrentRequestsPerRun(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, maxInFlightPerRun)
	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			arrived <- struct{}{}
			<-release
			writer.WriteHeader(http.StatusOK)
		},
	))
	defer upstream.Close()

	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, testProvider(t, upstream.URL, APIAnthropicMessages))
	front := httptest.NewServer(server)
	defer front.Close()

	var waiting sync.WaitGroup
	for range maxInFlightPerRun {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			request, _ := http.NewRequest(
				http.MethodPost,
				front.URL+"/v1/messages",
				strings.NewReader("{}"),
			)
			request.Header.Set("x-api-key", testCapability())
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				response.Body.Close()
			}
		}()
	}
	for range maxInFlightPerRun {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("saturating requests did not reach the upstream")
		}
	}

	request, _ := http.NewRequest(
		http.MethodPost,
		front.URL+"/v1/messages",
		strings.NewReader("{}"),
	)
	request.Header.Set("x-api-key", testCapability())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	status := response.StatusCode
	response.Body.Close()
	close(release)
	waiting.Wait()
	if status != http.StatusTooManyRequests {
		t.Fatalf("saturated run status = %d", status)
	}
}

// The Codex client authenticates with the JWT-wrapped capability and relies
// on the broker to replace its placeholder account header with the real one.
func TestServerRelaysCodexWithAccountHeaderAndForwardsClientHeaders(t *testing.T) {
	var seen struct {
		path       string
		auth       string
		account    string
		beta       string
		encoding   string
		originator string
		session    string
	}
	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			seen.path = request.URL.Path
			seen.auth = request.Header.Get("Authorization")
			seen.account = request.Header.Get("chatgpt-account-id")
			seen.beta = request.Header.Get("OpenAI-Beta")
			seen.encoding = request.Header.Get("Content-Encoding")
			seen.originator = request.Header.Get("originator")
			seen.session = request.Header.Get("session-id")
			writer.WriteHeader(http.StatusOK)
		},
	))
	defer upstream.Close()

	provider := testProvider(t, upstream.URL, APIOpenAICodexResponses)
	provider.Credentials = codexCredentials{}
	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, provider)

	wrapped := provider.runAPIKey(testCapability())
	request := httptest.NewRequest(http.MethodPost, "/codex/responses", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer "+wrapped)
	request.Header.Set("chatgpt-account-id", "pisafe")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("originator", "pi")
	request.Header.Set("session-id", "session-1")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body)
	}
	if seen.path != "/codex/responses" ||
		seen.auth != "Bearer real-access-token" ||
		seen.account != "real-account-id" {
		t.Fatalf("upstream saw %#v", seen)
	}
	if seen.beta != "responses=experimental" || seen.encoding != "zstd" ||
		seen.originator != "pi" || seen.session != "session-1" {
		t.Fatalf("client headers were not forwarded: %#v", seen)
	}
}

type codexCredentials struct{}

func (codexCredentials) UpstreamAuth(context.Context) (http.Header, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer real-access-token")
	headers.Set("Chatgpt-Account-Id", "real-account-id")
	return headers, nil
}

func TestServerFailsClosedWhenCredentialsUnavailable(t *testing.T) {
	provider := testProvider(t, "https://upstream.example", APIAnthropicMessages)
	provider.Credentials = staticCredentials{err: errors.New("refresh failed")}
	runs := fakeRuns{manifests: []runstate.Manifest{activeRun("run-a", testCapability())}}
	server := NewServer(runs, provider)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestWithKey(testCapability()))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "refresh failed") {
		t.Fatalf("error detail leaked to the run: %s", recorder.Body)
	}
}

func TestServerErrorShapeMatchesConfiguredAPI(t *testing.T) {
	runs := fakeRuns{err: errors.New("unavailable")}
	server := NewServer(runs, testProvider(t, "https://upstream.example", APIAnthropicMessages))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestWithKey(testCapability()))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"error"`) {
		t.Fatalf("body = %s", recorder.Body)
	}
}
