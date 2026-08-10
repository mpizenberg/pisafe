package hostnet

import (
	"net"
	"net/netip"
	"slices"
	"testing"
)

func TestCollectIPv4ReportsLiveInterfacesAndTheGateway(t *testing.T) {
	interfaces := []interfaceInfo{
		{
			flags: net.FlagUp,
			addrs: []string{
				"192.168.7.23/24",
				"2001:db8::1/64",
				"100.64.4.2/30",
			},
		},
		{
			flags: net.FlagUp | net.FlagLoopback,
			addrs: []string{"127.0.0.1/8"},
		},
		{
			flags: 0,
			addrs: []string{"203.0.113.8/24"},
		},
	}

	prefixes, err := collectIPv4(interfaces, netip.MustParseAddr("192.168.7.1"))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		got = append(got, prefix.String())
	}
	// A down interface, the loopback, and IPv6 contribute nothing. What is left
	// is reported as observed: the boundary is what masks and collapses it.
	want := []string{"192.168.7.23/24", "100.64.4.2/30", "192.168.7.1/32"}
	if !slices.Equal(got, want) {
		t.Fatalf("prefixes = %#v, want %#v", got, want)
	}
}

func TestCollectIPv4FailsClosed(t *testing.T) {
	_, err := collectIPv4([]interfaceInfo{{
		flags: net.FlagUp | net.FlagLoopback,
		addrs: []string{"127.0.0.1/8"},
	}}, netip.Addr{})
	if err == nil {
		t.Fatal("collectIPv4 unexpectedly succeeded")
	}
}
