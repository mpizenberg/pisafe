package lima

import (
	"context"
	"fmt"
	"io"
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

func TestManagerStartIsIdempotent(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tRunning\n"),
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), []string{"192.168.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "shell", "pisafe", "sudo", "nft", "--file", "-")
}

func TestManagerStartRefreshesAfterResume(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("pisafe\tStopped\n"),
		nil,
		nil,
	}}
	manager := Manager{instance: InstanceName, runner: runner}

	if err := manager.Start(context.Background(), []string{"192.168.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[1], "--tty=false", "start", "pisafe")
	assertArgs(t, runner.calls[2], "shell", "pisafe", "sudo", "nft", "--file", "-")
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

func TestRefreshFirewallUsesOneNftTransaction(t *testing.T) {
	runner := &fakeRunner{}
	manager := Manager{instance: InstanceName, runner: runner}

	err := manager.RefreshFirewall(
		context.Background(),
		[]string{"192.168.2.0/24", "192.168.2.1/32", "203.0.113.9/32"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgs(t, runner.calls[0], "shell", "pisafe", "sudo", "nft", "--file", "-")
	want := "flush set inet pisafe host_onlink_v4\n" +
		"add element inet pisafe host_onlink_v4 { 192.168.2.0/24, 203.0.113.9/32 }\n"
	if runner.calls[0].stdin != want {
		t.Fatalf("stdin = %q, want %q", runner.calls[0].stdin, want)
	}
}

func TestRefreshFirewallRejectsInjectedPrefix(t *testing.T) {
	manager := Manager{instance: InstanceName, runner: &fakeRunner{}}
	err := manager.RefreshFirewall(context.Background(), []string{"10.0.0.0/8 } delete table inet pisafe"})
	if err == nil || !strings.Contains(err.Error(), "invalid IPv4 prefix") {
		t.Fatalf("error = %v", err)
	}
}

func assertArgs(t *testing.T, call recordedCall, want ...string) {
	t.Helper()
	if fmt.Sprint(call.args) != fmt.Sprint(want) {
		t.Fatalf("args = %#v, want %#v", call.args, want)
	}
}
