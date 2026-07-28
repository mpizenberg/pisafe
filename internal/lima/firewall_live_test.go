package lima_test

import (
	"context"
	"os"
	"testing"
	"time"
)

// The ruleset gates connection initiation on destination address alone, which
// is only worth as much as the paths that reach it. These are the ways a run
// arrives at a denied address without naming one: a resolver that answers with
// it, a redirect that names it after the request was allowed, a datagram that
// expects no handshake, and the names Podman hands the container for its own
// host. Each has to fail exactly as a direct attempt does.
func TestLiveFirewallDeniesShapedTraffic(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runLive(t, ctx, "podman", "run", "--rm", "--dns=1.1.1.1", "--dns=9.9.9.9",
		"docker.io/library/alpine:3.22", "sh", "-ec", `
wget -q -T 10 -O /dev/null https://example.com

# A public wildcard resolver answers with the address encoded in the name, so
# these are genuine answers pointing where the ruleset refuses to go. Requiring
# the answer keeps a resolver failure from passing as a denial.
for address in 10.0.0.1 169.254.169.254; do
  name="${address}.nip.io"
  nslookup "${name}" | grep -qx "Address: ${address}"
  if wget -q -T 5 -O /dev/null "http://${name}/"; then
    echo "a DNS answer reached ${address}" >&2
    exit 1
  fi
done

printf 'HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/latest/meta-data/\r\nContent-Length: 0\r\n\r\n' |
  nc -l -p 8080 &
for _ in 1 2 3 4 5; do
  netstat -ltn 2>/dev/null | grep -q ':8080 ' && break
  sleep 1
done
if wget -q -T 5 -O /dev/null http://127.0.0.1:8080/ 2>/tmp/redirect.err; then
  echo "a redirect reached 169.254.169.254" >&2
  exit 1
fi
# Naming the target proves the redirect was followed and the second hop is what
# failed, rather than the allowed first one.
grep -q 169.254.169.254 /tmp/redirect.err

if nslookup example.com 10.0.0.1 >/dev/null 2>&1; then
  echo "a UDP query was answered from 10.0.0.1" >&2
  exit 1
fi

# Podman names the VM for the container and sshd listens there on a port the
# input chain accepts, so these are the reachable-looking way out of the run.
# The broker address stands in for the one destination that is excepted, to
# show the exception is the port and not the address.
targets="host.containers.internal host.docker.internal 127.0.0.1 192.0.2.1"
targets="${targets} $(ip -4 -o addr show eth0 | awk '{ print $4 }' | cut -d/ -f1)"
targets="${targets} $(ip -4 route show default | awk '{ print $3; exit }')"
for target in ${targets}; do
  if nc -z -w 3 "${target}" 22; then
    echo "the VM is reachable from a run at ${target}" >&2
    exit 1
  fi
done
`)

	runLive(t, ctx, "sh", "-ec", `
nc -z -w 10 example.com 443 >/dev/null

# A refusal only means something while something is listening behind it.
ss -ltn | grep -q '0.0.0.0:22'
ss -ltn | grep -q '127.0.0.54:53'

# Loopback is excepted for root alone, so the account a container escape lands
# on cannot reach sshd or the resolver the VM runs for itself.
for target in 127.0.0.1:22 127.0.0.54:53; do
  if nc -z -w 3 "${target%:*}" "${target#*:}"; then
    echo "the unprivileged VM user reached loopback ${target}" >&2
    exit 1
  fi
done
`)
}
