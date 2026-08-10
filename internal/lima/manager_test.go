package lima

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"testing"
)

func testPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

type recordedCall struct {
	args  []string
	stdin string
}

type fakeRunner struct {
	outputs [][]byte
	// errors pairs with outputs by call index, for a path only a failing
	// command reaches. A failing call still takes its place in outputs, so the
	// two stay indexed alike.
	errors []error
	calls  []recordedCall
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
	var output []byte
	if len(runner.outputs) != 0 {
		output = runner.outputs[0]
		runner.outputs = runner.outputs[1:]
	}
	if len(runner.errors) != 0 {
		err := runner.errors[0]
		runner.errors = runner.errors[1:]
		if err != nil {
			return nil, err
		}
	}
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
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.create(context.Background(), "/tmp/pisafe.yaml"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[0], "template", "validate", "/tmp/pisafe.yaml")
	assertArgs(
		t,
		runner.calls[1],
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
	if len(runner.calls) != 9 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "disk", "list", "--json")
	assertArgs(t, runner.calls[2], "disk", "create", "pisafe-state", "--size", "64GiB")
	assertArgs(t, runner.calls[4],
		"--tty=false", "create", "--name=pisafe", runner.calls[4].args[3],
	)
	assertArgs(
		t, runner.calls[6],
		"shell", "pisafe", "cat", "/etc/pisafe/security-profile",
	)
}

// The disk outlives the instance, so a VM being recreated has to find the one
// already holding every run's storage rather than ask for a second, empty one.
func TestManagerEnsureAdoptsAnExistingStateDisk(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.2.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		nil,
		[]byte(`{"name":"other","size":1}` + "\n" +
			`{"name":"pisafe-state","size":68719476736}` + "\n"),
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
	for _, call := range runner.calls {
		if len(call.args) > 1 && call.args[0] == "disk" && call.args[1] == "create" {
			t.Fatalf("Ensure recreated a state disk that already exists: %#v", call)
		}
	}
	if len(runner.calls) != 8 {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

// A VM that is already there keeps the disk it was created with, so nothing
// asks Lima about disks on the path every run takes.
func TestManagerEnsureLeavesDisksAloneWhenTheVMExists(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.2.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("pisafe\tRunning\n"),
		[]byte(securityProfileDigest([]string{prefix.String()}) + "\n"),
		nil,
		[]byte(prefix.String() + "\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Ensure(context.Background(), []netip.Prefix{prefix}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if call.args[0] == "disk" {
			t.Fatalf("Ensure inspected Lima disks for an existing VM: %#v", call)
		}
	}
}

func TestManagerStartIsIdempotent(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte(securityProfileDigest([]string{"192.168.2.0/24"}) + "\n"),
		nil,
		[]byte("192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), testPrefixes("192.168.2.0/24")); err != nil {
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

	if err := manager.Start(context.Background(), testPrefixes("192.168.2.0/24")); err != nil {
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

	err := manager.Start(context.Background(), testPrefixes("192.168.2.0/24"))
	if err == nil || !strings.Contains(err.Error(), "security profile is stale") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("Start continued after detecting drift: %#v", runner.calls)
	}
}

func TestManagerStartFailsClosedWhenSecurityProfileIsMissing(t *testing.T) {
	runner := &fakeRunner{
		outputs: [][]byte{[]byte("pisafe\tRunning\n")},
		errors:  []error{nil, fmt.Errorf("missing")},
	}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.Start(context.Background(), testPrefixes("192.168.2.0/24"))
	if err == nil || !strings.Contains(err.Error(), "pisafe vm rebuild") {
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

	prefixes, err := CanonicalIPv4Prefixes(
		testPrefixes("192.168.2.0/24", "192.168.2.1/32", "203.0.113.9/32"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.verifyFirewall(context.Background(), prefixes); err != nil {
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

// The deny set the VM holds is the one pisafe did not compose, so a line of it
// that is not an IPv4 prefix fails the check rather than being skipped past.
func TestVerifyFirewallRejectsInjectedPrefix(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("10.0.0.0/8 } delete table inet pisafe\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}
	err := manager.verifyFirewall(context.Background(), []string{"10.0.0.0/8"})
	if err == nil || !strings.Contains(err.Error(), "invalid IPv4 prefix") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyFirewallFailsClosedOnNetworkChange(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("192.168.2.0/24\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}
	err := manager.verifyFirewall(context.Background(), []string{"10.20.30.0/24"})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v", err)
	}
}

// The disk carries every run's filesystem, so a delete that reached it through
// a kill would leave writes unflushed on the one thing the rebuild exists to
// keep.
func TestManagerDeleteShutsTheInstanceDownBeforeRemovingIt(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		nil,
		nil,
		[]byte(`{"name":"pisafe-state","instance":""}` + "\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "stop", "pisafe")
	assertArgs(t, runner.calls[2], "--tty=false", "delete", "--force", "pisafe")
	assertArgs(t, runner.calls[3], "disk", "list", "--json")
}

// An instance too broken to shut down is the one a rebuild was asked for, so
// killing it is the only way through — and Lima then leaves the disk locked to
// an instance that no longer exists, which would refuse it to the replacement.
func TestManagerDeleteKillsAnUnstoppableInstanceAndFreesItsDisk(t *testing.T) {
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte("pisafe\tRunning\n"),
			nil,
			nil,
			nil,
			[]byte(`{"name":"pisafe-state","instance":"pisafe"}` + "\n"),
			nil,
		},
		errors: []error{nil, fmt.Errorf("shutdown timed out")},
	}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 6 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[2], "--tty=false", "stop", "--force", "pisafe")
	assertArgs(t, runner.calls[3], "--tty=false", "delete", "--force", "pisafe")
	assertArgs(t, runner.calls[5], "disk", "unlock", "pisafe-state")
}

// An instance Lima will not classify is the one a rebuild is most often wanted
// for, so it has to be replaceable — while nothing may start a run on it.
func TestManagerDeleteReplacesAnInstanceLimaCannotClassify(t *testing.T) {
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte("pisafe\tBroken\n"),
			nil,
			nil,
			nil,
			[]byte(`{"name":"pisafe-state","instance":"pisafe"}` + "\n"),
			nil,
		},
		errors: []error{nil, fmt.Errorf("no such process")},
	}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArgs(t, runner.calls[2], "--tty=false", "stop", "--force", "pisafe")
	assertArgs(t, runner.calls[5], "disk", "unlock", "pisafe-state")

	broken := Manager{
		instance: InstanceName,
		runner:   &fakeRunner{outputs: [][]byte{[]byte("pisafe\tBroken\n")}},
	}
	err := broken.Start(context.Background(), testPrefixes("192.168.2.0/24"))
	if err == nil || !strings.Contains(err.Error(), "pisafe vm rebuild") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerDeleteLeavesAnAbsentInstanceAlone(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{[]byte("other\tRunning\n")}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("Delete acted on an instance that does not exist: %#v", runner.calls)
	}
}

// Whether the work survives the rebuild is the whole difference the plan has to
// report, and a VM provisioned before the disk existed is the case that loses
// it.
func TestManagerHasStateDiskDistinguishesTheDiskFromAnyOther(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		disks string
		want  bool
	}{
		{name: "present", disks: `{"name":"pisafe-state","instance":"pisafe"}`, want: true},
		{name: "other disks only", disks: `{"name":"pisafe-probe","instance":""}`},
		{name: "none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &fakeRunner{outputs: [][]byte{[]byte(testCase.disks + "\n")}}
			manager := Manager{instance: InstanceName, runner: runner}

			has, err := manager.HasStateDisk(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if has != testCase.want {
				t.Fatalf("HasStateDisk = %v, want %v", has, testCase.want)
			}
		})
	}
}

func assertArgs(t *testing.T, call recordedCall, want ...string) {
	t.Helper()
	if fmt.Sprint(call.args) != fmt.Sprint(want) {
		t.Fatalf("args = %#v, want %#v", call.args, want)
	}
}
