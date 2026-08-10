package lima_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
)

const liveBrokerMarker = "pisafe-live-broker-ok"

// liveBrokerStub is a Mac-loopback listener that answers the way the broker
// does. It has to answer rather than merely accept, because that is the whole
// difference between a relay that carries a request and one whose owner is
// exiting.
func liveBrokerStub(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(writer, liveBrokerMarker)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go server.Serve(listener)
	t.Cleanup(func() {
		_ = server.Close()
	})
	return listener.Addr().(*net.TCPAddr).Port
}

// TestLiveBrokerReverseRelay proves the only inbound path into the boundary:
// a Mac-loopback listener published on the dedicated VM broker address, reachable
// from the VM user and from a rootless pasta container, and gone once the
// relay closes. The rest of TEST-NET stays denied.
func TestLiveBrokerReverseRelay(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	vm := lima.New()
	gateway, err := vm.SSHGateway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := lima.StartReverseForward(ctx, gateway, liveBrokerStub(t))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			forward.Close()
		}
	}()
	waitForLiveRelay(t, ctx, vm, forward)

	runLive(t, ctx, "bash", "-ec", `
exec 3<>/dev/tcp/192.0.2.1/18080
printf 'GET / HTTP/1.0\r\nHost: broker\r\n\r\n' >&3
cat <&3 | grep -q `+liveBrokerMarker+`
`)
	runLive(t, ctx, "podman", "run", "--rm",
		"--network=pasta", "--dns=1.1.1.1",
		"docker.io/library/alpine:3.22", "sh", "-ec", `
wget -qO- http://192.0.2.1:18080/ | grep -q `+liveBrokerMarker+`
if nc -z -w 2 192.0.2.2 18080; then
  echo "TEST-NET beyond the broker address is reachable" >&2
  exit 1
fi
if nc -z -w 2 192.0.2.1 443; then
  echo "broker address accepts a non-broker port" >&2
  exit 1
fi
`)

	if err := forward.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	deadline := time.Now().Add(15 * time.Second)
	for liveBrokerPortIsBound(ctx, vm) {
		if time.Now().After(deadline) {
			t.Fatal("broker listener survived relay shutdown")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// liveBrokerPortIsBound answers a different question from
// ProbeBrokerListener: not whether the relay carries a request, but whether
// sshd still holds the forwarded port at all. It unbinds a moment after the
// client owning the forward exits, so a test that returned as soon as the
// relay stopped answering would leave the port taken for the next one.
func liveBrokerPortIsBound(ctx context.Context, vm lima.VM) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := vm.Execute(probeCtx, nil, "bash", "-c", fmt.Sprintf(
		"exec 3<>/dev/tcp/%s/%d",
		lima.BrokerAddress,
		lima.BrokerPort,
	))
	return err == nil
}

func waitForLiveRelay(
	t *testing.T,
	ctx context.Context,
	vm lima.VM,
	forward *lima.ReverseForward,
) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-forward.Done():
			t.Fatal(err)
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = vm.ProbeBrokerListener(probeCtx)
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("relay did not become reachable: %v", lastErr)
}

// TestLiveSecondRelayFailsClosed documents that a second broker cannot
// silently steal the binding while one is active.
func TestLiveSecondRelayFailsClosed(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	port := liveBrokerStub(t)

	vm := lima.New()
	gateway, err := vm.SSHGateway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := lima.StartReverseForward(ctx, gateway, port)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	waitForLiveRelay(t, ctx, vm, first)

	second, err := lima.StartReverseForward(ctx, gateway, port)
	if err != nil {
		return
	}
	select {
	case err := <-second.Done():
		if !strings.Contains(err.Error(), "relay") {
			t.Fatalf("unexpected second relay failure: %v", err)
		}
	case <-time.After(30 * time.Second):
		second.Close()
		t.Fatal("second relay unexpectedly kept running")
	}
}
