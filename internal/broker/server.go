package broker

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

const (
	maxRequestBytes      = int64(64 * 1024 * 1024)
	maxInFlightPerRun    = 4
	upstreamHeaderWait   = 5 * time.Minute
	upstreamDialTimeout  = 30 * time.Second
	authorizationHeader  = "Authorization"
	bearerPrefix         = "Bearer "
	anthropicKeyHeader   = "X-Api-Key"
	unauthorizedMessage  = "unknown, stopped, or expired run capability"
	unconfiguredUpstream = "no upstream inference provider is configured on the Mac"
	credentialFailure    = "upstream inference credentials are unavailable on the Mac"
)

// forwardedRequestHeaders is the complete set of client headers the relay
// passes upstream; credentials are never among them because the broker sets
// its own.
var forwardedRequestHeaders = []string{
	"Content-Type",
	"Content-Encoding",
	"Accept",
	"Anthropic-Version",
	"Anthropic-Beta",
	"OpenAI-Beta",
	"Originator",
	"Session-Id",
	"X-Client-Request-Id",
	"User-Agent",
}

// RunSource yields the current durable run records. The broker reads them on
// every request so a run that has stopped or been reclaimed is rejected
// immediately.
type RunSource interface {
	List() ([]runstate.Manifest, []runstate.UnreadableRun, error)
}

type Server struct {
	runs      RunSource
	providers Catalog
	client    *http.Client
	now       func() time.Time

	mu       sync.Mutex
	inFlight map[string]int
}

func NewServer(runs RunSource, providers Catalog) *Server {
	return &Server{
		runs:      runs,
		providers: providers,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: upstreamDialTimeout}).DialContext,
				ResponseHeaderTimeout: upstreamHeaderWait,
				IdleConnTimeout:       90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("upstream redirects are not followed")
			},
		},
		now:      time.Now,
		inFlight: map[string]int{},
	}
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// The path names the upstream, so every refusal below can be written in
	// the wire shape the client that sent it knows how to read.
	provider, routed := server.providers.find(request.URL.Path)
	runID, ok := server.authorize(request)
	if !ok {
		server.writeError(writer, http.StatusUnauthorized, provider, unauthorizedMessage)
		return
	}
	if len(server.providers) == 0 {
		server.writeError(writer, http.StatusServiceUnavailable, provider, unconfiguredUpstream)
		return
	}
	if request.Method != http.MethodPost {
		server.writeError(writer, http.StatusMethodNotAllowed, provider, "only POST is relayed")
		return
	}
	if !routed {
		server.writeError(writer, http.StatusNotFound, provider, fmt.Sprintf(
			"only %s is relayed", strings.Join(server.providers.routes(), ", "),
		))
		return
	}
	if request.ContentLength > maxRequestBytes {
		server.writeError(writer, http.StatusRequestEntityTooLarge, provider, "request exceeds relay size limit")
		return
	}
	if !server.acquire(runID) {
		server.writeError(writer, http.StatusTooManyRequests, provider, "run exceeded its concurrent request limit")
		return
	}
	defer server.release(runID)

	server.relay(writer, provider, request)
}

func (server *Server) relay(writer http.ResponseWriter, provider Provider, request *http.Request) {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	upstream, err := http.NewRequestWithContext(
		request.Context(),
		http.MethodPost,
		provider.upstreamEndpoint(),
		body,
	)
	if err != nil {
		server.writeError(writer, http.StatusBadGateway, provider, "build upstream request failed")
		return
	}
	upstream.ContentLength = request.ContentLength
	for _, header := range forwardedRequestHeaders {
		if value := request.Header.Get(header); value != "" {
			upstream.Header.Set(header, value)
		}
	}
	credentials, err := provider.Credentials.UpstreamAuth(request.Context())
	if err != nil {
		server.writeError(writer, http.StatusServiceUnavailable, provider, credentialFailure)
		return
	}
	for header, values := range credentials {
		upstream.Header[header] = values
	}

	response, err := server.client.Do(upstream)
	if err != nil {
		server.writeError(writer, http.StatusBadGateway, provider, "upstream inference request failed")
		return
	}
	defer response.Body.Close()

	for _, header := range []string{"Content-Type", "Retry-After", "Request-Id"} {
		if value := response.Header.Get(header); value != "" {
			writer.Header().Set(header, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	flusher, _ := writer.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := writer.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			// Mid-stream upstream failures cannot change the already-sent
			// status; the truncated stream is the client's error signal.
			return
		}
	}
}

// authorize matches the presented capability against active, in-budget runs
// in constant time per candidate. Every failure mode is the same 401.
func (server *Server) authorize(request *http.Request) (string, bool) {
	capability := request.Header.Get(anthropicKeyHeader)
	if capability == "" {
		bearer := request.Header.Get(authorizationHeader)
		if strings.HasPrefix(bearer, bearerPrefix) {
			capability = strings.TrimPrefix(bearer, bearerPrefix)
		}
	}
	capability = presentedCapability(capability)
	if !runstate.ValidInferenceCapability(capability) {
		return "", false
	}
	// A record that cannot be read holds no capability that can be matched, so
	// leaving it out only ever denies.
	manifests, _, err := server.runs.List()
	if err != nil {
		return "", false
	}
	presented := sha256.Sum256([]byte(capability))
	now := server.now()
	matched := ""
	for _, manifest := range manifests {
		if manifest.State != runstate.StateActive || manifest.InferenceCapability == "" {
			continue
		}
		recorded := sha256.Sum256([]byte(manifest.InferenceCapability))
		if subtle.ConstantTimeCompare(presented[:], recorded[:]) != 1 {
			continue
		}
		if runstate.RemainingSeconds(manifest, now) == 0 {
			continue
		}
		matched = manifest.RunID
	}
	return matched, matched != ""
}

func (server *Server) acquire(runID string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.inFlight[runID] >= maxInFlightPerRun {
		return false
	}
	server.inFlight[runID]++
	return true
}

func (server *Server) release(runID string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.inFlight[runID]--
	if server.inFlight[runID] <= 0 {
		delete(server.inFlight, runID)
	}
}

// writeError uses the wire shape of the API the request was addressed to so
// the Pi client surfaces the message instead of a decode failure. A request
// that named no configured upstream gets the OpenAI shape, which is what every
// client that is not Anthropic's own reads.
func (server *Server) writeError(
	writer http.ResponseWriter,
	status int,
	provider Provider,
	message string,
) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(message)
	var body string
	if provider.API == APIAnthropicMessages {
		body = `{"type":"error","error":{"type":"invalid_request_error","message":"` +
			escaped + `"}}`
	} else {
		body = `{"error":{"message":"` + escaped + `","type":"invalid_request_error"}}`
	}
	_, _ = io.WriteString(writer, body+"\n")
}
