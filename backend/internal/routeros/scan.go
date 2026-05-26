package routeros

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// PortScanResult holds the open TCP ports found for a single host during a
// subnet sweep.
type PortScanResult struct {
	Address   string `json:"address"`
	OpenPorts []int  `json:"open_ports"`
}

// maxScanHosts caps the total number of host addresses a single sweep may probe
// so a careless CIDR can't fan out into an unbounded port-scan.
const maxScanHosts = 1024

// ScanSubnet sweeps every usable host in cidr, TCP-dialing each port in ports
// with a bounded worker pool. A host is included in the result only if at least
// one of its ports accepted a connection within timeout.
//
// Guardrails: the prefix must be /22 or smaller (i.e. mask bits >= 22) and the
// total usable host count must not exceed maxScanHosts; either violation returns
// an error before any dialing happens.
func ScanSubnet(ctx context.Context, cidr string, ports []int, timeout time.Duration, concurrency int) ([]PortScanResult, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}

	// Only IPv4 sweeps are supported (the managed devices speak IPv4 API).
	if ipNet.IP.To4() == nil {
		return nil, fmt.Errorf("only IPv4 CIDRs are supported")
	}

	ones, bits := ipNet.Mask.Size()
	if ones < 22 {
		return nil, fmt.Errorf("CIDR /%d is too large; use /22 or smaller", ones)
	}

	hosts := enumerateHosts(ipNet, bits)
	if len(hosts) > maxScanHosts {
		return nil, fmt.Errorf("CIDR expands to %d hosts; limit is %d", len(hosts), maxScanHosts)
	}
	if len(ports) == 0 {
		return []PortScanResult{}, nil
	}

	if concurrency <= 0 {
		concurrency = 64
	}

	type job struct{ host string }

	var (
		mu      sync.Mutex
		results = make(map[string][]int)
		wg      sync.WaitGroup
		jobs    = make(chan job)
	)

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			if ctx.Err() != nil {
				return
			}
			var open []int
			for _, p := range ports {
				if ctx.Err() != nil {
					return
				}
				if Ping(j.host, p, timeout) == nil {
					open = append(open, p)
				}
			}
			if len(open) > 0 {
				mu.Lock()
				results[j.host] = open
				mu.Unlock()
			}
		}
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker()
	}

	go func() {
		defer close(jobs)
		for _, h := range hosts {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{host: h}:
			}
		}
	}()

	wg.Wait()

	out := make([]PortScanResult, 0, len(results))
	for addr, open := range results {
		sort.Ints(open)
		out = append(out, PortScanResult{Address: addr, OpenPorts: open})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, ctx.Err()
}

// enumerateHosts returns every usable host address in ipNet, skipping the
// network and broadcast addresses for prefixes that have them (/31 and /32 have
// no broadcast/network split, so all their addresses are usable).
func enumerateHosts(ipNet *net.IPNet, bits int) []string {
	ones, _ := ipNet.Mask.Size()
	base := ipNet.IP.Mask(ipNet.Mask).To4()
	if base == nil {
		return nil
	}

	total := 1 << uint(bits-ones) // host count including network/broadcast

	skipEnds := ones <= 30 // /31 and /32 have no network/broadcast reservation

	hosts := make([]string, 0, total)
	cur := make(net.IP, len(base))
	copy(cur, base)
	for i := 0; i < total; i++ {
		if !(skipEnds && (i == 0 || i == total-1)) {
			ip := make(net.IP, len(cur))
			copy(ip, cur)
			hosts = append(hosts, ip.String())
		}
		incIP(cur)
	}
	return hosts
}

func incIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}
