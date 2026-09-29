package flow

import (
	"database/sql"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNilAndDisabledCollector(t *testing.T) {
	var nilC *Collector
	for name, c := range map[string]*Collector{"nil": nilC, "disabled": New(nil, nil, nil, Config{})} {
		if c.Enabled() {
			t.Errorf("%s: Enabled() = true", name)
		}
		if !c.FlushedThrough().IsZero() || !c.RolledThrough().IsZero() {
			t.Errorf("%s: flushed/rolled through must be zero", name)
		}
		c.Reload()
		c.Reload() // never blocks
		if !c.Wait(time.Millisecond) {
			t.Errorf("%s: Wait must return true at once when never started", name)
		}
		b, err := json.Marshal(c.Status())
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"enabled":false`, `"listen":[]`, `"listen_errors":[]`, `"started_at":null`,
			`"flushed_through":null`, `"rolled_through":null`, `"rcvbuf_bytes":0`, `"exporters":[]`, `"unknown_senders":[]`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: status JSON %s lacks %s", name, b, want)
			}
		}
	}
	done := make(chan struct{})
	go func() { New(nil, nil, nil, Config{}).Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run of a disabled collector must return at once")
	}
}

func TestListenPort(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want int
	}{
		"empty":       {nil, 0},
		"simple":      {[]string{":2055"}, 2055},
		"first valid": {[]string{"bogus", "0.0.0.0:6343", ":2055"}, 6343},
		"v6":          {[]string{"[::]:4739"}, 4739},
		"none valid":  {[]string{":0", ":99999"}, 0},
	}
	for name, c := range cases {
		if got := ListenPort(c.in); got != c.want {
			t.Errorf("%s: ListenPort(%q) = %d, want %d", name, c.in, got, c.want)
		}
	}
}

func TestExporterStatusFromRow(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	row := queries.FlowExporter{ID: 2, Name: "firewall003", Address: "192.168.78.81", Kind: "opnsense", Enabled: true,
		NATAddresses: []netip.Addr{netip.MustParseAddr("192.168.28.81")}, LastSeen: now.Add(-time.Minute),
		Protocol: "netflow9", SamplingRate: 1, Datagrams: 5, Flows: 6, Bytes: 7, DecodeErrors: 1, TemplateMisses: 2,
		SeqLost: 3, ClockSkewMs: 40}
	s := ExporterStatusFromRow(row, now)
	if s.State != StateOK || s.DeviceID != nil || s.LastSeen == nil || s.NATAddresses[0] != "192.168.28.81" ||
		s.TemplateState != TemplateNA || s.Counters["datagrams"] != 5 || s.Counters["seq_lost"] != 3 ||
		s.Counters["replayed"] != 0 || len(s.Counters) != len(ExporterCounterKeys) || len(s.TimeSource) != 4 ||
		s.ClockSkewMs != 40 || s.LastErrorAt != nil {
		t.Fatalf("status = %+v", s)
	}
	for _, c := range []struct {
		enabled  bool
		lastSeen time.Time
		want     string
	}{
		{false, now, StateDisabled},
		{true, time.Time{}, StateNever},
		{true, now.Add(-2*time.Minute - 59*time.Second), StateOK},
		{true, now.Add(-3 * time.Minute), StateStale},
	} {
		row.Enabled, row.LastSeen = c.enabled, c.lastSeen
		if got := ExporterStatusFromRow(row, now).State; got != c.want {
			t.Errorf("state(enabled=%v, lastSeen=%v) = %s, want %s", c.enabled, c.lastSeen, got, c.want)
		}
	}
	row.DeviceID, row.NATAddresses = "dev", nil
	s = ExporterStatusFromRow(row, now)
	if s.DeviceID == nil || *s.DeviceID != "dev" || s.NATAddresses == nil {
		t.Fatalf("device/nat = %+v", s)
	}
}

func TestSettingsReadersClamp(t *testing.T) {
	db := testDB(t)
	if TopN(db) != 50 || TopNHourly(db) != 100 || MaxRowsPerMinute(db) != 1500 || Retention1mDays(db) != 3 ||
		Retention1hDays(db) != 90 || RateLimitPPS(db) != 2000 || !AutoAcceptDevices(db) {
		t.Fatal("migration defaults not read")
	}
	for k, v := range map[string]string{"flow_top_n": "5", "flow_top_n_hourly": "999999", "flow_1m_days": "junk",
		"flow_rate_limit_pps": " 300 ", "flow_auto_accept_devices": "false"} {
		if err := queries.SetSetting(db, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if TopN(db) != 10 || TopNHourly(db) != 2000 || Retention1mDays(db) != 3 || RateLimitPPS(db) != 300 || AutoAcceptDevices(db) {
		t.Fatalf("clamps: topN %d hourly %d 1m %d pps %d auto %v", TopN(db), TopNHourly(db), Retention1mDays(db),
			RateLimitPPS(db), AutoAcceptDevices(db))
	}
	if TopN(nil) != 50 || AutoAcceptDevices(nil) != true {
		t.Fatal("nil db must yield defaults")
	}
	got := parsePrefixCSV("192.168.23.0/24, 10.1.2.3 ,junk,2001:db8::1,::ffff:10.0.0.0/104\n172.16.0.1/12")
	want := []string{"192.168.23.0/24", "10.1.2.3/32", "2001:db8::1/128", "10.0.0.0/8", "172.16.0.0/12"}
	if len(got) != len(want) {
		t.Fatalf("parsePrefixCSV = %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("parsePrefixCSV[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
