// Package hostnet discovers host-side networks that a pisafe VM must not
// reach. The result is embedded in the initial firewall and refreshed whenever
// the VM starts or resumes.
package hostnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"sort"
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

func collectIPv4(interfaces []interfaceInfo, gateway netip.Addr) ([]netip.Prefix, error) {
	unique := make(map[netip.Prefix]struct{})
	for _, networkInterface := range interfaces {
		if networkInterface.flags&net.FlagUp == 0 || networkInterface.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, address := range networkInterface.addrs {
			prefix, err := netip.ParsePrefix(address)
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			unique[prefix.Masked()] = struct{}{}
		}
	}
	if gateway.IsValid() && gateway.Is4() {
		unique[netip.PrefixFrom(gateway, gateway.BitLen())] = struct{}{}
	}
	if len(unique) == 0 {
		return nil, errors.New("no active host IPv4 network could be determined")
	}

	prefixes := make([]netip.Prefix, 0, len(unique))
	for prefix := range unique {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(left, right int) bool {
		if prefixes[left].Bits() != prefixes[right].Bits() {
			return prefixes[left].Bits() < prefixes[right].Bits()
		}
		return prefixes[left].String() < prefixes[right].String()
	})
	collapsed := make([]netip.Prefix, 0, len(prefixes))
	for _, candidate := range prefixes {
		covered := false
		for _, existing := range collapsed {
			if existing.Contains(candidate.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			collapsed = append(collapsed, candidate)
		}
	}
	prefixes = collapsed
	sort.Slice(prefixes, func(left, right int) bool {
		return prefixes[left].String() < prefixes[right].String()
	})
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
