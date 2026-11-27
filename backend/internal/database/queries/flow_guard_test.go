package queries

import (
	"database/sql"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func guardDevice(t *testing.T, db *sql.DB, id, addr, name string) {
	t.Helper()
	if err := CreateDevice(db, &Device{ID: id, Address: addr, Identity: name, Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatalf("device %s: %v", id, err)
	}
}

func TestDeleteFlowExporterLeavesTombstone(t *testing.T) {
	db := testDB(t)
	guardDevice(t, db, "r1", "192.0.2.1", "core-router")
	id, err := CreateFlowExporter(db, &FlowExporter{Name: "core-router", Address: "192.0.2.1", Kind: "routeros",
		DeviceID: "r1", Enabled: true, Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := UpsertDeviceFlowIfaces(db, id, []FlowIface{{IfIndex: 1, Name: "ether1", Type: "ether"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	p, err := DeleteFlowExporterBlocking(db, id, "exporter deleted by alice", "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Address != "192.0.2.1" || p.Source != FlowBlockDeleted || p.DeviceID != "r1" || p.Name != "core-router" {
		t.Fatalf("tombstone = %+v", p)
	}
	if e, _ := GetFlowExporter(db, id); e != nil {
		t.Fatalf("exporter still there: %+v", e)
	}
	if ifs, _ := ListFlowIfaces(db, id); len(ifs) != 0 {
		t.Fatalf("interfaces did not cascade: %+v", ifs)
	}
	got, err := GetFlowSenderPolicy(db, "192.0.2.1")
	if err != nil || got == nil || got.Policy != FlowPolicyBlocked || got.Reason != "exporter deleted by alice" ||
		got.CreatedBy != "alice" || !got.CreatedAt.Equal(now) || got.Kind != "routeros" || got.Datagrams != 0 ||
		!got.LastSeen.IsZero() {
		t.Fatalf("stored tombstone = %+v, %v", got, err)
	}
	if _, err := DeleteFlowExporterBlocking(db, id, "again", "alice", now); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("second delete: %v, want ErrFlowNotFound", err)
	}

	// The address cannot be added again without saying so.
	e := &FlowExporter{Name: "core-router", Address: "192.0.2.1", Kind: "routeros", Enabled: true}
	if _, err := CreateFlowExporterChecked(db, e, false); !errors.Is(err, ErrFlowSenderBlocked) {
		t.Fatalf("re-add without unblock: %v, want ErrFlowSenderBlocked", err)
	}
	if exps, _ := ListFlowExporters(db); len(exps) != 0 {
		t.Fatalf("a refused create left a row: %+v", exps)
	}
	newID, err := CreateFlowExporterChecked(db, e, true)
	if err != nil || newID == id {
		t.Fatalf("re-add with unblock: id %d (old %d), %v — the id is never reused", newID, id, err)
	}
	if got, _ := GetFlowSenderPolicy(db, "192.0.2.1"); got != nil {
		t.Fatalf("block survived an explicit re-add: %+v", got)
	}
	if _, err := CreateFlowExporterChecked(db, e, true); !errors.Is(err, ErrFlowExporterExists) {
		t.Fatalf("duplicate address: %v", err)
	}
}

func TestFlowSenderPolicyBlockUnblockAndDrops(t *testing.T) {
	db := testDB(t)
	guardDevice(t, db, "s1", "192.0.2.10", "access-switch")
	now := time.Unix(1_800_000_000, 0).UTC()
	created, err := BlockFlowSender(db, &FlowSenderPolicy{Address: "192.0.2.10", Reason: "a switch", Source: FlowBlockAuto,
		Name: "access-switch", Kind: "routeros", DeviceID: "s1", CreatedAt: now})
	if err != nil || !created {
		t.Fatalf("block: %v %v", created, err)
	}
	// A second block keeps the first one's reason.
	created, err = BlockFlowSender(db, &FlowSenderPolicy{Address: "192.0.2.10", Reason: "other", Source: FlowBlockDeleted, CreatedAt: now})
	if err != nil || created {
		t.Fatalf("second block: created=%v err=%v", created, err)
	}
	if err := AddFlowSenderDrops(db, "192.0.2.10", 7, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := AddFlowSenderDrops(db, "192.0.2.10", 3, now); err != nil { // an older stamp never moves last_seen back
		t.Fatal(err)
	}
	if err := AddFlowSenderDrops(db, "192.0.2.99", 1, now); err != nil { // unknown address: no error, no row
		t.Fatal(err)
	}
	list, err := ListFlowSenderPolicies(db)
	if err != nil || len(list) != 1 || list[0].Reason != "a switch" || list[0].Source != FlowBlockAuto ||
		list[0].Datagrams != 10 || !list[0].LastSeen.Equal(now.Add(time.Minute)) || list[0].DeviceID != "s1" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// An exporter row wins over a block for the same address (a restore, an
	// older binary): the block is not listed.
	if _, err := CreateFlowExporter(db, &FlowExporter{Name: "x", Address: "192.0.2.10", Kind: "routeros", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if list, _ := ListFlowSenderPolicies(db); len(list) != 0 {
		t.Fatalf("block listed although the address is an exporter: %+v", list)
	}

	// Deleting the device keeps the block (it is keyed by address).
	if err := DeleteDevice(db, "s1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := GetFlowSenderPolicy(db, "192.0.2.10"); p == nil || p.DeviceID != "" {
		t.Fatalf("after device delete: %+v", p)
	}
	if err := UnblockFlowSender(db, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	if err := UnblockFlowSender(db, "192.0.2.10"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("unblock twice: %v", err)
	}
}

func TestFlowExportChecks(t *testing.T) {
	db := testDB(t)
	guardDevice(t, db, "s1", "192.0.2.10", "access-switch")
	t0 := time.Unix(1_800_000_000, 0).UTC()
	if c, err := GetFlowExportCheck(db, "s1"); err != nil || c != nil {
		t.Fatalf("no row yet: %+v %v", c, err)
	}
	if err := NoteFlowExportSighting(db, "s1", "192.0.2.10", 2055, t0); err != nil {
		t.Fatal(err)
	}
	if err := NoteFlowExportSighting(db, "s1", "192.0.2.10", 2055, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, err := GetFlowExportCheck(db, "s1")
	if err != nil || c == nil || !c.FirstSeen.Equal(t0) || !c.LastSeen.Equal(t0.Add(5*time.Minute)) ||
		!c.CheckedAt.IsZero() || c.Result != "" || c.CollectorPort != 2055 || len(c.Targets) != 0 {
		t.Fatalf("after sightings: %+v %v", c, err)
	}
	targets := []netip.AddrPort{netip.MustParseAddrPort("198.51.100.5:2055"), netip.MustParseAddrPort("[2001:db8::5]:9995")}
	if err := SetFlowExportCheckResult(db, "s1", t0.Add(6*time.Minute), FlowExportExporting, "enabled, two targets", targets); err != nil {
		t.Fatal(err)
	}
	// A later sighting keeps the result.
	if err := NoteFlowExportSighting(db, "s1", "192.0.2.10", 2055, t0.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, _ = GetFlowExportCheck(db, "s1")
	if c.Result != FlowExportExporting || !c.CheckedAt.Equal(t0.Add(6*time.Minute)) || !c.FirstSeen.Equal(t0) ||
		!c.LastSeen.Equal(t0.Add(7*time.Minute)) || c.Detail == "" || !slices.Equal(c.Targets, targets) {
		t.Fatalf("after check: %+v", c)
	}
	// A read that finds no target clears the stored ones.
	if err := SetFlowExportCheckResult(db, "s1", t0.Add(70*time.Minute), FlowExportNotExporting, "disabled", nil); err != nil {
		t.Fatal(err)
	}
	if c, _ = GetFlowExportCheck(db, "s1"); c.Result != FlowExportNotExporting || len(c.Targets) != 0 {
		t.Fatalf("after a negative check: %+v", c)
	}
	if all, _ := ListFlowExportChecks(db); len(all) != 1 {
		t.Fatalf("list = %+v", all)
	}
	if err := DeleteDevice(db, "s1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := ListFlowExportChecks(db); len(all) != 0 {
		t.Fatalf("rows did not cascade with the device: %+v", all)
	}
}

// The finding takes its severity from the device's check row in the same
// statement: warn while unconfirmed, critical once the device confirmed it.
func TestFlowExportFindingSeverityFollowsTheCheck(t *testing.T) {
	db := testDB(t)
	guardDevice(t, db, "s1", "192.0.2.10", "access-switch")
	const kind, detail = "flow_export_on_switch", "flow datagrams from 192.0.2.10"
	severity := func() (string, int) {
		t.Helper()
		rows, err := ListDeviceFindings(db, "s1", 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("findings = %+v, %v", rows, err)
		}
		return rows[0].Severity, rows[0].Count
	}
	t0 := time.Unix(1_800_000_000, 0).UTC()
	if err := UpsertFlowExportFinding(db, "s1", kind, detail, t0); err != nil { // no check row at all
		t.Fatal(err)
	}
	if s, n := severity(); s != "warn" || n != 1 {
		t.Fatalf("before any check: %s ×%d", s, n)
	}
	if err := SetFlowExportCheckResult(db, "s1", t0, FlowExportExporting, "enabled", nil); err != nil {
		t.Fatal(err)
	}
	if err := RefreshFlowExportFindingSeverity(db, "s1", kind, t0); err != nil {
		t.Fatal(err)
	}
	if s, n := severity(); s != "critical" || n != 1 {
		t.Fatalf("after a confirming check: %s ×%d (a refresh is not a sighting)", s, n)
	}
	if err := UpsertFlowExportFinding(db, "s1", kind, detail, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s, n := severity(); s != "critical" || n != 2 {
		t.Fatalf("a later sighting: %s ×%d", s, n)
	}
	// A confirmation nobody renewed expires: a datagram that arrives long
	// after the last read — it may be forged — is unconfirmed again.
	stale := t0.Add(FlowExportConfirmationValid + time.Minute)
	chk, _ := GetFlowExportCheck(db, "s1")
	if !chk.Confirmed(t0.Add(time.Hour)) || chk.Confirmed(stale) {
		t.Fatalf("Confirmed: within the window %v, after it %v", chk.Confirmed(t0.Add(time.Hour)), chk.Confirmed(stale))
	}
	if err := UpsertFlowExportFinding(db, "s1", kind, detail, stale); err != nil {
		t.Fatal(err)
	}
	if s, _ := severity(); s != "warn" {
		t.Fatalf("a sighting after the confirmation expired: severity %s, want warn", s)
	}
	for _, result := range []string{FlowExportNotExporting, FlowExportCheckFailed} {
		if err := SetFlowExportCheckResult(db, "s1", t0, result, "", nil); err != nil {
			t.Fatal(err)
		}
		if err := UpsertFlowExportFinding(db, "s1", kind, detail, t0); err != nil {
			t.Fatal(err)
		}
		if s, _ := severity(); s != "warn" {
			t.Fatalf("check result %s: severity %s, want warn", result, s)
		}
		if c, _ := GetFlowExportCheck(db, "s1"); c.Confirmed(t0) {
			t.Fatalf("check result %s counts as a confirmation", result)
		}
	}
}
