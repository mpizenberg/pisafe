package lima

import (
	"context"
	"errors"
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
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.create(context.Background(), "/tmp/pisafe.yaml"); err != nil {
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
	prefix := netip.MustParsePrefix("198.51.100.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		nil,
		nil,
		nil,
		nil,
		nil,
		[]byte("pisafe\tRunning\n"),
		readyOutput(prefix.String()),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Ensure(context.Background(), []netip.Prefix{prefix}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "disk", "list", "--json")
	assertArgs(t, runner.calls[2], "disk", "create", "pisafe-state", "--size", "64GiB")
	assertArgs(t, runner.calls[4],
		"--tty=false", "create", "--name=pisafe", runner.calls[4].args[3],
	)
	assertSetupRead(t, runner.calls[6])
}

// The disk outlives the instance, so a VM being recreated has to find the one
// already holding every run's storage rather than ask for a second, empty one.
func TestManagerEnsureAdoptsAnExistingStateDisk(t *testing.T) {
	prefix := netip.MustParsePrefix("198.51.100.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		nil,
		[]byte(`{"name":"other","size":1}` + "\n" +
			`{"name":"pisafe-state","size":68719476736}` + "\n"),
		nil,
		nil,
		[]byte("pisafe\tRunning\n"),
		readyOutput(prefix.String()),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Ensure(context.Background(), []netip.Prefix{prefix}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if len(call.args) > 1 && call.args[0] == "disk" && call.args[1] == "create" {
			t.Fatalf("Ensure recreated a state disk that already exists: %#v", call)
		}
	}
	if len(runner.calls) != 7 {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

// A VM that is already there keeps the disk it was created with, so nothing
// asks Lima about disks on the path every run takes.
func TestManagerEnsureLeavesDisksAloneWhenTheVMExists(t *testing.T) {
	prefix := netip.MustParsePrefix("198.51.100.0/24")
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("pisafe\tRunning\n"),
		readyOutput(prefix.String()),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Ensure(context.Background(), []netip.Prefix{prefix}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if call.args[0] == "disk" {
			t.Fatalf("Ensure inspected Lima disks for an existing VM: %#v", call)
		}
	}
}

// The setup state and the record it vouches for come back together, so holding
// a ready VM to its record costs no more round trips than reading the record.
func TestManagerStartIsIdempotent(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		readyOutput("198.51.100.0/24"),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Start(context.Background(), testPrefixes("198.51.100.0/24")); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertSetupRead(t, runner.calls[1])
	assertArgs(t, runner.calls[2], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

func TestManagerStartRefreshesAfterResume(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tStopped\n"),
		nil,
		readyOutput("198.51.100.0/24"),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Start(context.Background(), testPrefixes("198.51.100.0/24")); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "start", "--timeout=2h0m0s", "pisafe")
	assertSetupRead(t, runner.calls[2])
	assertArgs(t, runner.calls[3], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

// A Mac that moved from one private network to another is denied the same
// addresses on both, so the VM built on the first stays current on the second.
func TestManagerStartKeepsTheVMAcrossPrivateNetworks(t *testing.T) {
	built, err := CanonicalIPv4Prefixes(testPrefixes("172.20.10.2/28"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		readyOutput(built...),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Start(context.Background(), testPrefixes("10.16.3.7/21")); err != nil {
		t.Fatal(err)
	}
}

// Handing a run's work back, and letting go of the run, are what is left on a
// VM that can no longer start one, so the security profile is not held against
// it here: a drifted one would refuse exactly the commands that rescue the work.
func TestManagerStartUnverifiedSkipsBoundaryVerification(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("ready\nsha256:stale\n"),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.StartUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertSetupRead(t, runner.calls[1])
	assertArgs(t, runner.calls[2], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

// A VM whose setup ended without completing is the one whose work most needs
// rescuing, so the exempt commands still reach it.
func TestManagerStartUnverifiedStartsStoppedInstance(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tStopped\n"),
		nil,
		[]byte("incomplete\n"),
		nil,
	}}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.StartUnverified(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "start", "--timeout=2h0m0s", "pisafe")
	assertSetupRead(t, runner.calls[2])
	assertArgs(t, runner.calls[3], "shell", "pisafe", "sudo", "/usr/local/sbin/pisafe-clock-step")
}

// Setup still running is waited on by every command, verified or not: it is
// what mounts the state disk and narrows sudo, and a rebuild would only start
// it over.
func TestManagerRefusesAVMStillSettingUpWithoutNamingRebuild(t *testing.T) {
	for name, start := range map[string]func(VM) error{
		"Start": func(vm VM) error {
			return vm.Start(context.Background(), testPrefixes("198.51.100.0/24"))
		},
		"StartUnverified": func(vm VM) error {
			return vm.StartUnverified(context.Background())
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{outputs: [][]byte{
				[]byte("pisafe\tRunning\n"),
				[]byte("setting-up\n"),
			}}
			err := start(VM{instance: InstanceName, runner: runner})
			if !errors.Is(err, ErrSettingUp) || strings.Contains(err.Error(), "rebuild") {
				t.Fatalf("error = %v", err)
			}
			if len(runner.calls) != 2 {
				t.Fatalf("continued on a VM still setting up: %#v", runner.calls)
			}
		})
	}
}

// Lima gives up on a slow first setup while the guest carries on, so its
// failure is reported as the wait it is when the setup is still going.
func TestManagerStartReportsAnAbandonedStartThatIsStillSettingUp(t *testing.T) {
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte("pisafe\tStopped\n"),
			nil,
			[]byte("setting-up\n"),
		},
		errors: []error{nil, fmt.Errorf("did not receive an event with the running status")},
	}
	vm := VM{instance: InstanceName, runner: runner}

	err := vm.Start(context.Background(), testPrefixes("198.51.100.0/24"))
	if !errors.Is(err, ErrSettingUp) {
		t.Fatalf("error = %v", err)
	}
	assertSetupRead(t, runner.calls[2])
}

func TestManagerStartReportsLimaWhenAFailedStartIsNotSettingUp(t *testing.T) {
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte("pisafe\tStopped\n"),
			nil,
			nil,
		},
		errors: []error{
			nil,
			fmt.Errorf("did not receive an event with the running status"),
			fmt.Errorf("instance is not running"),
		},
	}
	vm := VM{instance: InstanceName, runner: runner}

	err := vm.Start(context.Background(), testPrefixes("198.51.100.0/24"))
	if err == nil || errors.Is(err, ErrSettingUp) ||
		!strings.Contains(err.Error(), "start Lima instance: did not receive") {
		t.Fatalf("error = %v", err)
	}
}

// A VM that cannot be asked has told nothing about its setup, so it must not
// be reported as waiting on one or as needing a rebuild.
func TestManagerSetupReadFailureIsNeitherUnfinishedState(t *testing.T) {
	for _, output := range []struct {
		name   string
		answer []byte
		err    error
	}{
		{name: "unreachable", err: fmt.Errorf("ssh: connection reset")},
		{name: "unrecognised", answer: []byte("sha256:old-record\n")},
	} {
		t.Run(output.name, func(t *testing.T) {
			runner := &fakeRunner{
				outputs: [][]byte{[]byte("pisafe\tRunning\n"), output.answer},
				errors:  []error{nil, output.err},
			}
			vm := VM{instance: InstanceName, runner: runner}

			err := vm.Start(context.Background(), testPrefixes("198.51.100.0/24"))
			if err == nil || errors.Is(err, ErrSettingUp) ||
				!strings.Contains(err.Error(), "read VM setup state") ||
				strings.Contains(err.Error(), "rebuild") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestManagerStartUnverifiedRefusesAbsentInstance(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{nil}}
	vm := VM{instance: InstanceName, runner: runner}

	err := vm.StartUnverified(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has not been created") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerStartFailsClosedOnSecurityProfileDrift(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("ready\nsha256:stale\n"),
	}}
	vm := VM{instance: InstanceName, runner: runner}

	err := vm.Start(context.Background(), testPrefixes("192.168.2.0/24"))
	if err == nil || !strings.Contains(err.Error(), "security profile is stale") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("Start continued after detecting drift: %#v", runner.calls)
	}
}

// Setup that ended this boot without writing the record either failed or
// belongs to a VM built before the record moved, and only a rebuild settles
// either.
func TestManagerStartFailsClosedWhenSetupDidNotComplete(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
		[]byte("incomplete\n"),
	}}
	vm := VM{instance: InstanceName, runner: runner}

	err := vm.Start(context.Background(), testPrefixes("192.168.2.0/24"))
	if err == nil || errors.Is(err, ErrSettingUp) ||
		!strings.Contains(err.Error(), "pisafe vm rebuild") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("Start continued on an incomplete setup: %#v", runner.calls)
	}
}

func TestManagerStartFailsBeforeLimaWhenPrefixesAreMissing(t *testing.T) {
	runner := &fakeRunner{}
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Start(context.Background(), nil); err == nil {
		t.Fatal("Start unexpectedly accepted an empty firewall set")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v", runner.calls)
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
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Delete(context.Background()); err != nil {
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
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Delete(context.Background()); err != nil {
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
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArgs(t, runner.calls[2], "--tty=false", "stop", "--force", "pisafe")
	assertArgs(t, runner.calls[5], "disk", "unlock", "pisafe-state")

	broken := VM{
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
	vm := VM{instance: InstanceName, runner: runner}

	if err := vm.Delete(context.Background()); err != nil {
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
			vm := VM{instance: InstanceName, runner: runner}

			has, err := vm.HasStateDisk(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if has != testCase.want {
				t.Fatalf("HasStateDisk = %v, want %v", has, testCase.want)
			}
		})
	}
}

func readyOutput(prefixes ...string) []byte {
	return []byte("ready\n" + securityProfileDigest(prefixes) + "\n")
}

func assertSetupRead(t *testing.T, call recordedCall) {
	t.Helper()
	assertArgs(t, call, "shell", "pisafe", "sh", "-ceu", setupStateScript, "pisafe-remote")
}

func assertArgs(t *testing.T, call recordedCall, want ...string) {
	t.Helper()
	if fmt.Sprint(call.args) != fmt.Sprint(want) {
		t.Fatalf("args = %#v, want %#v", call.args, want)
	}
}
