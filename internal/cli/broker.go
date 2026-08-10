package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/providers"
)

// runBroker serves brokered inference to active runs until interrupted. The
// VM-side listener exists only while this process holds the reverse forward.
func runBroker(ctx context.Context, out io.Writer) error {
	if err := requireSupportedHost("broker"); err != nil {
		return err
	}
	catalog, err := providers.Load(ctx)
	if err != nil {
		return err
	}
	// Force a credential check (and any pending refresh) now, so a dead login
	// is reported here rather than discovered by a run. One dead login does
	// not withhold the others: the relay already fails closed per request, and
	// refusing to start would make one stale token cost every upstream.
	var unusable []string
	for _, provider := range catalog {
		if _, err := provider.Credentials.UpstreamAuth(ctx); err != nil {
			unusable = append(unusable, fmt.Sprintf("  %s: %v", provider.Name, err))
		}
	}
	if err := startBoundary(ctx); err != nil {
		return err
	}
	transport := lima.NewTransport()
	gateway, err := transport.SSHGateway(ctx)
	if err != nil {
		return err
	}
	store, err := runStore()
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for broker relay: %w", err)
	}
	defer listener.Close()
	localPort := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{
		Handler:           broker.NewServer(store, catalog),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverFailed := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			serverFailed <- fmt.Errorf("broker HTTP server failed: %w", err)
		}
	}()
	defer server.Close()

	forward, err := lima.StartReverseForward(ctx, gateway, localPort)
	if err != nil {
		return err
	}
	defer forward.Close()
	if err := waitForRelay(ctx, transport, forward); err != nil {
		return err
	}

	if len(catalog) == 0 {
		fmt.Fprintf(
			out,
			"Warning: no upstream provider is configured; runs will receive errors.\n"+
				"         Run pisafe login, then restart the broker.\n",
		)
	}
	for _, line := range catalog.Describe() {
		fmt.Fprintf(out, "Relaying %s\n", line)
	}
	if len(unusable) > 0 {
		fmt.Fprintf(
			out,
			"Warning: %d stored login(s) could not be used; runs asking for them get errors:\n%s\n",
			len(unusable),
			strings.Join(unusable, "\n"),
		)
	}
	fmt.Fprintf(
		out,
		"Broker:    http://%s:%d (inside runs)\n"+
			"Local:     127.0.0.1:%d\n"+
			"Stop with Ctrl-C; active runs then lose inference until restarted.\n",
		lima.BrokerAddress,
		lima.BrokerPort,
		localPort,
	)

	select {
	case <-ctx.Done():
		fmt.Fprintln(out, "Stopping broker.")
		return nil
	case err := <-forward.Done():
		return err
	case err := <-serverFailed:
		return err
	}
}

// waitForRelay confirms from inside the VM that the reverse forward is
// accepting connections before reporting the broker as available.
func waitForRelay(
	ctx context.Context,
	transport lima.Transport,
	forward *lima.ReverseForward,
) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-forward.Done():
			return err
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = transport.ProbeBrokerListener(probeCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("broker relay did not become reachable: %w", lastErr)
}
