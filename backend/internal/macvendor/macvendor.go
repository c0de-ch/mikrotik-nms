// Package macvendor resolves a MAC address to its hardware vendor using the
// IEEE OUI (MA-L) registry, and detects locally-administered ("randomized" /
// private) MACs. It is a fallback identity for clients the NMS can't name from
// DHCP/DNS — e.g. WiFi clients on subnets with no reachable DHCP source, or
// phones using MAC randomization.
//
// The registry is embedded (gzipped, ~380 KB) and decompressed into a prefix
// map on first use, so lookups are allocation-free after warm-up and there is
// no build-time network dependency. Regenerate oui.csv.gz from
// https://standards-oui.ieee.org/oui/oui.csv (see package test for the format).
package macvendor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"strings"
	"sync"
)

//go:embed oui.csv.gz
var ouiGz []byte

var (
	once  sync.Once
	table map[string]string // 6-hex uppercase OUI -> organization name
)

func load() {
	table = make(map[string]string, 40000)
	zr, err := gzip.NewReader(bytes.NewReader(ouiGz))
	if err != nil {
		return // leave table empty; lookups degrade to ""
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		comma := strings.IndexByte(line, ',')
		if comma != 6 {
			continue
		}
		table[line[:6]] = line[comma+1:]
	}
}

// normalize strips separators and returns the first 6 hex chars (the OUI),
// upper-cased. Returns "" if there aren't 6 hex digits.
func normalize(mac string) string {
	var b strings.Builder
	b.Grow(6)
	for i := 0; i < len(mac) && b.Len() < 6; i++ {
		c := mac[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			b.WriteByte(c)
		case c >= 'a' && c <= 'f':
			b.WriteByte(c - 'a' + 'A')
		}
	}
	if b.Len() < 6 {
		return ""
	}
	return b.String()
}

// Lookup returns the IEEE-registered vendor for a MAC, or "" if unknown
// (unregistered, locally-administered, or malformed).
func Lookup(mac string) string {
	oui := normalize(mac)
	if oui == "" {
		return ""
	}
	once.Do(load)
	return table[oui]
}

// IsRandomized reports whether the MAC is locally administered (the U/L bit of
// the first octet is set) — i.e. a private/randomized address rather than a
// burned-in vendor MAC. These never resolve via the OUI registry.
func IsRandomized(mac string) bool {
	oui := normalize(mac)
	if oui == "" {
		return false
	}
	// First octet is the first two hex chars; the locally-administered bit is
	// 0x02. Hex digit 2,3,6,7,A,B,E,F in the second nibble has bit 0x02 set.
	switch oui[1] {
	case '2', '3', '6', '7', 'A', 'B', 'E', 'F':
		return true
	default:
		return false
	}
}

// Describe returns the vendor (if known) and whether the MAC is randomized.
func Describe(mac string) (vendor string, randomized bool) {
	return Lookup(mac), IsRandomized(mac)
}
