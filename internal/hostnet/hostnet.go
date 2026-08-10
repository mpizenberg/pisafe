// Package hostnet discovers the host-side networks a pisafe VM must not reach.
// It reports what the Mac is attached to and nothing more: masking, collapsing,
// and ordering belong to whoever builds the boundary out of them, because that
// is what the VM definition and its digest are taken over.
//
// The set is fixed into the VM definition when the instance is built. Starting
// or resuming a VM verifies it rather than rewriting it, so a Mac that has since
// joined a different network fails the check and is told to rebuild.
package hostnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
)

type interfaceInfo struct {
	flags net.Flags
	addrs []string
}

func OnLinkIPv4(ctx context.Context) ([]netip.Prefix, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list host interfaces: %w", err)
	}

	infos := make([]interfaceInfo, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list addresses for %s: %w", networkInterface.Name, err)
		}
		info := interfaceInfo{flags: networkInterface.Flags}
		for _, address := range addresses {
			info.addrs = append(info.addrs, address.String())
		}
		infos = append(infos, info)
	}

	gateway, err := defaultGatewayIPv4(ctx)
	if err != nil {
		return nil, err
	}
	return collectIPv4(infos, gateway)
}

// collectIPv4 takes the IPv4 prefix of every address on an interface that is up
// and is not the loopback, and the default gateway as a host route. An empty
// result is an error: an incomplete deny set is worse than none at all, because
// it would be built into a VM that then looks boundaried.
func collectIPv4(interfaces []interfaceInfo, gateway netip.Addr) ([]netip.Prefix, error) {
	prefixes := []netip.Prefix{}
	for _, networkInterface := range interfaces {
		if networkInterface.flags&net.FlagUp == 0 || networkInterface.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, address := range networkInterface.addrs {
			prefix, err := netip.ParsePrefix(address)
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			prefixes = append(prefixes, prefix)
		}
	}
	if gateway.IsValid() && gateway.Is4() {
		prefixes = append(prefixes, netip.PrefixFrom(gateway, gateway.BitLen()))
	}
	if len(prefixes) == 0 {
		return nil, errors.New("no active host IPv4 network could be determined")
	}
	return prefixes, nil
}

func defaultGatewayIPv4(ctx context.Context) (netip.Addr, error) {
	command := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default")
	output, err := command.Output()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("determine default route: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		if !found || key != "gateway" {
			continue
		}
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err == nil && address.Is4() {
			return address, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, fmt.Errorf("read default route: %w", err)
	}
	return netip.Addr{}, errors.New("default IPv4 gateway could not be determined")
}
