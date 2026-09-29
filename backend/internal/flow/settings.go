package flow

import (
	"database/sql"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// settingInt reads an integer app_setting clamped to lo..hi; a missing or
// non-numeric value yields def.
func settingInt(db *sql.DB, key string, def, lo, hi int) int {
	if db == nil {
		return def
	}
	v, err := queries.GetSetting(db, key)
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// settingBool reads a boolean app_setting ("true"/"false", strconv forms);
// a missing or garbage value yields def.
func settingBool(db *sql.DB, key string, def bool) bool {
	if db == nil {
		return def
	}
	v, err := queries.GetSetting(db, key)
	if err != nil {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

// TopN is flow_top_n (default 50, clamp 10..500): conversations kept per
// (in_if, out_if) pair and minute.
func TopN(db *sql.DB) int { return settingInt(db, "flow_top_n", 50, 10, 500) }

// TopNHourly is flow_top_n_hourly (default 100, clamp 20..2000): conversations
// kept per pair in the hourly rollup.
func TopNHourly(db *sql.DB) int { return settingInt(db, "flow_top_n_hourly", 100, 20, 2000) }

// MaxRowsPerMinute is flow_max_rows_per_minute (default 1500, clamp
// 200..10000): the global cap of non-other rows per exporter and minute.
func MaxRowsPerMinute(db *sql.DB) int {
	return settingInt(db, "flow_max_rows_per_minute", 1500, 200, 10000)
}

// Retention1mDays is flow_1m_days (default 3, clamp 1..14).
func Retention1mDays(db *sql.DB) int { return settingInt(db, "flow_1m_days", 3, 1, 14) }

// Retention1hDays is flow_1h_days (default 90, clamp 7..730).
func Retention1hDays(db *sql.DB) int { return settingInt(db, "flow_1h_days", 90, 7, 730) }

// RateLimitPPS is flow_rate_limit_pps (default 2000, clamp 100..50000):
// datagrams per second accepted per exporter.
func RateLimitPPS(db *sql.DB) int { return settingInt(db, "flow_rate_limit_pps", 2000, 100, 50000) }

// AutoAcceptDevices is flow_auto_accept_devices (default true): datagrams
// from a managed device's address create an exporter automatically.
func AutoAcceptDevices(db *sql.DB) bool { return settingBool(db, "flow_auto_accept_devices", true) }

// prefixListSetting parses a CIDR/IP CSV setting (bare IPs are /32 or /128;
// junk entries are skipped).
func prefixListSetting(db *sql.DB, key string) []netip.Prefix {
	if db == nil {
		return nil
	}
	v, err := queries.GetSetting(db, key)
	if err != nil {
		return nil
	}
	return parsePrefixCSV(v)
}

// parsePrefixCSV parses a CSV of CIDRs or bare IPs (masked, unmapped).
func parsePrefixCSV(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == ';' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			if p.Addr().Is4In6() {
				p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
			}
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}
