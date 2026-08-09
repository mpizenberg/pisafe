package lima

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"testing"
)

type recordedCall struct {
	args  []string
	stdin string
}

type fakeRunner struct {
	outputs [][]byte
	calls   []recordedCall
}

func (runner *fakeRunner) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	call := recordedCall{args: append([]string(nil), args...)}
	if stdin != nil {
		content, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		call.stdin = string(content)
	}
	runner.calls = append(runner.calls, call)
	if len(runner.outputs) == 0 {
		return nil, nil
	}
	output := runner.outputs[0]
	runner.outputs = runner.outputs[1:]
	return output, nil
}

func (runner *fakeRunner) Stream(ctx context.Context, stdout io.Writer, args ...string) error {
	output, err := runner.Run(ctx, nil, args...)
	if err != nil {
		return err
	}
	_, err = stdout.Write(output)
	return err
}

func TestManagerCreateValidatesBeforeCreating(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		nil,
		nil,
		nil,
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Create(context.Background(), "/tmp/pisafe.yaml"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "template", "validate", "/tmp/pisafe.yaml")
	assertArgs(
		t,
		runner.calls[2],
		"--tty=false", "create", "--name=pisafe", "/tmp/pisafe.yaml",
	)
}

func TestManagerEnsureCreatesStartsAndVerifiesAbsentVM(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.2.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		nil,
		nil,
		nil,
		nil,
		[]byte("pisafe\tRunning\n"),
		[]byte(securityProfileDigest([]string{prefix.String()}) + "\n"),
		nil,
		[]byte(prefix.String() + "\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Ensure(context.Background(), []netip.Prefix{prefix}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[3],
		"--tty=false", "create", "--name=pisafe", runner.calls[3].args[3],
	)
	assertArgs(
		t, runner.calls[5],
		"shell", "pisafe", "cat", "/etc/pisafe/security-profile",
	)
}

func TestManagerStartIsIdempotent(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte(securityProfileDigest([]string{"192.168.2.0/24"}) + "\n"),
		nil,
		[]byte("192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), []string{"192.168.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(
		t, runner.calls[1],
		"shell", "pisafe", "cat", "/etc/pisafe/security-profile",
	)
	assertArgs(t, runner.calls[2], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
	assertArgs(
		t,
		runner.calls[3],
		"shell", "pisafe", "cat", "/etc/pisafe/host-prefixes",
	)
}

func TestManagerStartRefreshesAfterResume(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tStopped\n"),
		nil,
		[]byte(securityProfileDigest([]string{"192.168.2.0/24"}) + "\n"),
		nil,
		[]byte("192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), []string{"192.168.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "start", "pisafe")
	assertArgs(
		t, runner.calls[2],
		"shell", "pisafe", "cat", "/etc/pisafe/security-profile",
	)
	assertArgs(t, runner.calls[3], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
	assertArgs(
		t,
		runner.calls[4],
		"shell", "pisafe", "cat", "/etc/pisafe/host-prefixes",
	)
}

// Handing a run's work back, and letting go of the run, are what is left on a
// VM that can no longer start one, so neither boundary record may be read here:
// a drifted one would refuse exactly the commands that rescue the work.
func TestManagerStartUnverifiedSkipsBoundaryVerification(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		nil,
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.StartUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

func TestManagerStartUnverifiedStartsStoppedInstance(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tStopped\n"),
		nil,
		nil,
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.StartUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "start", "pisafe")
	assertArgs(t, runner.calls[2], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

func TestManagerStartUnverifiedRefusesAbsentInstance(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{nil}}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.StartUnverified(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has not been created") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerStartFailsClosedOnSecurityProfileDrift(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("sha256:stale\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.Start(context.Background(), []string{"192.168.2.0/24"})
	if err == nil || !strings.Contains(err.Error(), "security profile is stale") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("Start continued after detecting drift: %#v", runner.calls)
	}
}

func TestManagerStartFailsClosedWhenSecurityProfileIsMissing(t *testing.T) {
	runner := &errorRunner{
		outputs: [][]byte{[]byte("pisafe\tRunning\n")},
		errors:  []error{nil, fmt.Errorf("missing")},
	}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.Start(context.Background(), []string{"192.168.2.0/24"})
	if err == nil || !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerStartFailsBeforeLimaWhenPrefixesAreMissing(t *testing.T) {
	runner := &fakeRunner{}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), nil); err == nil {
		t.Fatal("Start unexpectedly accepted an empty firewall set")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestVerifyFirewallAcceptsCanonicalEquivalentPrefixes(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("203.0.113.9/32\n192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.VerifyFirewall(
		context.Background(),
		[]string{"192.168.2.0/24", "192.168.2.1/32", "203.0.113.9/32"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(
		t,
		runner.calls[0],
		"shell", "pisafe", "cat", "/etc/pisafe/host-prefixes",
	)
}

func TestVerifyFirewallRejectsInjectedPrefix(t *testing.T) {
	manager := Manager{instance: InstanceName, runner: &fakeRunner{}}
	err := manager.VerifyFirewall(context.Background(), []string{"10.0.0.0/8 } delete table inet pisafe"})
	if err == nil || !strings.Contains(err.Error(), "invalid IPv4 prefix") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyFirewallFailsClosedOnNetworkChange(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}
	err := manager.VerifyFirewall(context.Background(), []string{"10.20.30.0/24"})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v", err)
	}
}

func assertArgs(t *testing.T, call recordedCall, want ...string) {
	t.Helper()
	if fmt.Sprint(call.args) != fmt.Sprint(want) {
		t.Fatalf("args = %#v, want %#v", call.args, want)
	}
}

type errorRunner struct {
	outputs [][]byte
	errors  []error
}

func (runner *errorRunner) Run(
	_ context.Context,
	_ io.Reader,
	_ ...string,
) ([]byte, error) {
	var output []byte
	if len(runner.outputs) > 0 {
		output = runner.outputs[0]
		runner.outputs = runner.outputs[1:]
	}
	var err error
	if len(runner.errors) > 0 {
		err = runner.errors[0]
		runner.errors = runner.errors[1:]
	}
	return output, err
}

func (runner *errorRunner) Stream(ctx context.Context, stdout io.Writer, args ...string) error {
	output, err := runner.Run(ctx, nil, args...)
	if err != nil {
		return err
	}
	_, err = stdout.Write(output)
	return err
}
