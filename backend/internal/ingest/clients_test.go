package ingest

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/dbtest"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

// seen is a KindClientSeen observation.
func seen(mac string, c edgeproto.ClientSeen) edgeproto.Observation {
	return edgeproto.Observation{Kind: edgeproto.KindClientSeen, Subject: mac, ObservedAt: time.Unix(1_767_614_400, 0), Body: c}
}

// tableRows returns query's rows, one line each, the columns separated by |.
func tableRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v.String
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The rows a client leaves, except the clock's columns (updated_at,
// recorded_at) and the AUTOINCREMENT id; history in insert order.
func lookupRows(t *testing.T, db *sql.DB) []string {
	return tableRows(t, db, `SELECT mac_address, ip_address, host_name, dns_name, source, device_name, interface_name,
		device_id, ap, ssid, band, channel, frequency, signal, tx_rate, rx_rate, uptime FROM mac_lookup ORDER BY mac_address`)
}

func historyRows(t *testing.T, db *sql.DB) []string {
	return tableRows(t, db, `SELECT mac_address, ip_address, host_name, source, device_name FROM client_history ORDER BY id`)
}

// storedBeforeTheSplit is what ClientDiscoveryPoller.store wrote for one
// client before its writes moved here — the code ApplyClientSeen replaces,
// with the client's fields as the collect half now reports them.
func storedBeforeTheSplit(db *sql.DB, mac string, c edgeproto.ClientSeen) {
	hostname := c.HostName
	if hostname == "" {
		hostname = c.DNSName
	}
	_ = queries.UpsertMACLookup(db, &queries.MACLookup{
		MACAddress: mac, IPAddress: c.IP, HostName: hostname, DNSName: c.DNSName, Source: c.Source, DeviceName: c.DeviceName,
	})
	_ = queries.InsertClientHistory(db, &queries.ClientHistoryEntry{
		MACAddress: mac, IPAddress: c.IP, HostName: hostname, Source: c.Source, DeviceName: c.DeviceName,
	})
}

// ApplyClientSeen leaves exactly the rows the poller's own writes left:
// the same lookup row (every merge rule of the upsert: names, an IPv4 kept
// over an IPv6, a missing device name keeping the stored one) and one
// history row per observation, in the same order.
func TestApplyClientSeenStoresWhatThePollerStored(t *testing.T) {
	const (
		laptop = "02:00:5E:10:00:01"
		phone  = "02:00:5E:10:00:02"
		camera = "02:00:5E:10:00:03"
	)
	passes := [][]edgeproto.Observation{
		{
			seen(laptop, edgeproto.ClientSeen{IP: "192.0.2.101", HostName: "laptop-1", DNSName: "laptop.lan", Source: "dhcp", DeviceName: "rtr"}),
			seen(phone, edgeproto.ClientSeen{IP: "2001:db8::2", DNSName: "phone.lan", Source: "arp", DeviceName: "sw24"}),
			seen(camera, edgeproto.ClientSeen{IP: "192.0.2.103", Source: "dhcp", DeviceName: "kea"}),
		},
		{
			// An IPv6 address does not replace the stored IPv4 one; no DHCP
			// name: the reverse-DNS name is the host name.
			seen(laptop, edgeproto.ClientSeen{IP: "2001:db8::1", DNSName: "laptop.lan", Source: "arp", DeviceName: "sw48"}),
			// An IPv4 address replaces the stored IPv6 one.
			seen(phone, edgeproto.ClientSeen{IP: "192.0.2.102", HostName: "phone-1", Source: "dhcp", DeviceName: "rtr"}),
			// No address and no device name: both keep the stored value.
			seen(camera, edgeproto.ClientSeen{Source: "arp"}),
		},
	}

	before, after := dbtest.Open(t), dbtest.Open(t)
	for _, pass := range passes {
		for _, o := range pass {
			storedBeforeTheSplit(before, o.Subject, o.Body.(edgeproto.ClientSeen))
			if err := ApplyClientSeen(after, o); err != nil {
				t.Fatalf("ApplyClientSeen(%s): %v", o.Subject, err)
			}
		}
	}

	for name, rows := range map[string]func(*testing.T, *sql.DB) []string{"mac_lookup": lookupRows, "client_history": historyRows} {
		want, got := rows(t, before), rows(t, after)
		if !slices.Equal(got, want) {
			t.Errorf("%s after Apply:\n  %s\nwant (the poller's writes):\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
	if got := len(historyRows(t, after)); got != 6 {
		t.Errorf("client_history has %d rows, want one per observation (6)", got)
	}
	wantLaptop := laptop + "|192.0.2.101|laptop.lan|laptop.lan|arp|sw48|||||||||||"
	if got := lookupRows(t, after)[0]; got != wantLaptop {
		t.Errorf("the laptop's lookup row = %s, want %s", got, wantLaptop)
	}
}

// The rows keep the timestamps the poller's writes had: SQLite's
// CURRENT_TIMESTAMP, not the observation's time.
func TestApplyClientSeenStampsTheRowsAsBefore(t *testing.T) {
	db := dbtest.Open(t)
	o := seen("02:00:5E:10:00:01", edgeproto.ClientSeen{IP: "192.0.2.101", Source: "arp", DeviceName: "rtr"})
	o.ObservedAt = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := ApplyClientSeen(db, o); err != nil {
		t.Fatal(err)
	}
	// CURRENT_TIMESTAMP's layout, and within the hour of the write (the
	// observation is 25 years older).
	const stampedNow = `SELECT COUNT(*) FROM %[1]s
		WHERE %[2]s GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]'
		  AND %[2]s >= datetime('now', '-1 hour')`
	for table, column := range map[string]string{"mac_lookup": "updated_at", "client_history": "recorded_at"} {
		var n int
		q := fmt.Sprintf(stampedNow, table, column)
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 1 {
			t.Errorf("%s.%s is not SQLite's CURRENT_TIMESTAMP of the write", table, column)
		}
	}
}

// An observation that is not a client, or names no MAC, is refused and
// writes nothing.
func TestApplyClientSeenRefusesWhatIsNotAClient(t *testing.T) {
	db := dbtest.Open(t)
	for name, o := range map[string]edgeproto.Observation{
		"no MAC":         seen("", edgeproto.ClientSeen{IP: "192.0.2.1", Source: "arp"}),
		"a pointer body": {Kind: edgeproto.KindClientSeen, Subject: "02:00:5E:10:00:01", Body: &edgeproto.ClientSeen{IP: "192.0.2.1"}},
		"another body":   {Kind: edgeproto.KindClientSeen, Subject: "02:00:5E:10:00:01", Body: "192.0.2.1"},
		"no body":        {Kind: edgeproto.KindClientSeen, Subject: "02:00:5E:10:00:01"},
	} {
		if err := ApplyClientSeen(db, o); err == nil {
			t.Errorf("%s: applied, want an error", name)
		}
	}
	if n := len(lookupRows(t, db)) + len(historyRows(t, db)); n != 0 {
		t.Errorf("%d rows written by refused observations", n)
	}
}

// Every kind there is has its one apply function, and Apply refuses a kind
// without one.
func TestEveryKindHasItsApplyFunction(t *testing.T) {
	kinds := edgeproto.Kinds()
	seenKinds := map[edgeproto.Kind]bool{}
	for _, k := range kinds {
		if seenKinds[k] {
			t.Errorf("edgeproto.Kinds lists %q twice", k)
		}
		seenKinds[k] = true
		if applyFuncs[k] == nil {
			t.Errorf("kind %q has no apply function", k)
		}
	}
	for k := range applyFuncs {
		if !seenKinds[k] {
			t.Errorf("apply function for %q, a kind edgeproto.Kinds does not list", k)
		}
	}
	err := Apply(dbtest.Open(t), edgeproto.Observation{Kind: "no.such.kind", Subject: "x"})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", "no.such.kind")) {
		t.Errorf("Apply(unknown kind) = %v, want an error naming the kind", err)
	}
}
