package routeros

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestScanSubnetRejectsLargeCIDR(t *testing.T) {
	_, err := ScanSubnet(context.Background(), "10.0.0.0/16", []int{8728}, 10*time.Millisecond, 8)
	if err == nil {
		t.Fatal("expected error for /16 CIDR, got nil")
	}
}

func TestScanSubnetRejectsInvalidCIDR(t *testing.T) {
	_, err := ScanSubnet(context.Background(), "not-a-cidr", []int{8728}, 10*time.Millisecond, 8)
	if err == nil {
		t.Fatal("expected error for invalid CIDR, got nil")
	}
}

func TestScanSubnetNoPorts(t *testing.T) {
	res, err := ScanSubnet(context.Background(), "192.168.1.0/24", nil, 10*time.Millisecond, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected empty result with no ports, got %d", len(res))
	}
}

func TestEnumerateHostsSkipsNetworkAndBroadcast(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("192.168.1.0/24")
	hosts := enumerateHosts(ipNet, 32)
	if len(hosts) != 254 {
		t.Fatalf("expected 254 usable hosts in /24, got %d", len(hosts))
	}
	if hosts[0] != "192.168.1.1" {
		t.Fatalf("expected first host 192.168.1.1, got %s", hosts[0])
	}
	if hosts[len(hosts)-1] != "192.168.1.254" {
		t.Fatalf("expected last host 192.168.1.254, got %s", hosts[len(hosts)-1])
	}
	for _, h := range hosts {
		if h == "192.168.1.0" || h == "192.168.1.255" {
			t.Fatalf("network/broadcast address %s should be skipped", h)
		}
	}
}

func TestEnumerateHostsSlash32(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("192.168.1.5/32")
	hosts := enumerateHosts(ipNet, 32)
	if len(hosts) != 1 || hosts[0] != "192.168.1.5" {
		t.Fatalf("expected [192.168.1.5], got %v", hosts)
	}
}

func TestScanSubnetFindsOpenPort(t *testing.T) {
	// Listen on a loopback port and scan a /32 of that address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	res, err := ScanSubnet(context.Background(), "127.0.0.1/32", []int{port}, 500*time.Millisecond, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 || res[0].Address != "127.0.0.1" {
		t.Fatalf("expected one result for 127.0.0.1, got %v", res)
	}
	if len(res[0].OpenPorts) != 1 || res[0].OpenPorts[0] != port {
		t.Fatalf("expected open port %d, got %v", port, res[0].OpenPorts)
	}
}
