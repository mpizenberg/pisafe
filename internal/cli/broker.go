package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// runBroker serves brokered inference to active runs until interrupted. The
// VM-side listener exists only while this process holds the reverse forward.
func runBroker(ctx context.Context, out io.Writer) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("pisafe broker requires macOS on ARM64")
	}
	provider, err := broker.FromEnvironment()
	if err != nil {
		return err
	}
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return fmt.Errorf("discover host networks: %w", err)
	}
	prefixStrings := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefixStrings = append(prefixStrings, prefix.String())
	}
	if err := lima.NewManager().Start(ctx, prefixStrings); err != nil {
		return err
	}
	transport := lima.NewTransport()
	gateway, err := transport.SSHGateway(ctx)
	if err != nil {
		return err
	}
	stateRoot, err := runstate.DefaultRoot()
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
		Handler:           broker.NewServer(runstate.NewStore(stateRoot), provider),
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

	if provider == nil {
		fmt.Fprintf(
			out,
			"Warning: no upstream provider is configured; runs will receive errors.\n"+
				"         Set PISAFE_INFERENCE_UPSTREAM, PISAFE_INFERENCE_API,\n"+
				"         PISAFE_INFERENCE_KEY, and PISAFE_INFERENCE_MODELS, then restart.\n",
		)
	} else {
		fmt.Fprintf(out, "Relaying %s\n", provider.Describe())
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
