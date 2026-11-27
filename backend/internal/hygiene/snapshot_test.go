package hygiene

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotHoldsConfigurationOnly(t *testing.T) {
	s := cleanAudit()
	snap, complete := SnapshotOf(s)
	if !complete {
		t.Fatal("a pass without a failed read is complete")
	}
	raw := snap.JSON()
	// Counters and runtime state are not configuration …
	for _, gone := range []string{"bridge-fast-path-packets", "bridge-fast-forward-packets", "192.0.2.10"} {
		if strings.Contains(raw, gone) {
			t.Errorf("the snapshot holds %q:\n%s", gone, raw)
		}
	}
	for _, key := range []string{SecSessions, SecSniffer} {
		if _, ok := snap.Sections[key]; ok {
			t.Errorf("the snapshot holds the runtime section %s", key)
		}
	}
	// … what was read is, and so is a refusal.
	for _, kept := range []string{`"use-ip-firewall":"false"`, SecUserGroups, `"count":0`} {
		if !strings.Contains(raw, kept) {
			t.Errorf("the snapshot lacks %q:\n%s", kept, raw)
		}
	}
	// A menu with nothing in it is not there, whichever way the pass learned
	// that: an empty table, a read that was not sent, a menu the device does
	// not have. Which of the three it was must not move the hash.
	for _, key := range []string{SecGraphingInterface, SecSNMPCommunities} {
		if _, ok := snap.Sections[key]; ok {
			t.Errorf("the empty section %s is part of the snapshot", key)
		}
	}
	for name, same := range map[string]Sections{
		"an empty table that was skipped":  with(s, SecBonding, skippedSec),
		"an empty table on a missing menu": with(s, SecGraphingQueue, absentSec),
		"a skipped read that was sent":     with(s, SecSNMPCommunities, sec()),
	} {
		if got, _ := SnapshotOf(same); got.Hash() != snap.Hash() {
			t.Errorf("%s changed the hash", name)
		}
	}
	denied, _ := SnapshotOf(with(s, SecUserGroups, deniedSec))
	if denied.Sections[SecUserGroups].State != "denied" || denied.Hash() == snap.Hash() {
		t.Error("a refused read must show in the snapshot")
	}
}

// The hash changes when the configuration does, and only then.
func TestSnapshotHash(t *testing.T) {
	base, _ := SnapshotOf(cleanAudit())

	// Counters moved, another session is open, the sniffer runs: the same
	// configuration.
	busy := with(with(with(cleanAudit(), SecBridgeSettings, sec(row("use-ip-firewall", "false", "allow-fast-path", "true",
		"bridge-fast-path-packets", "999999999", "bridge-fast-forward-packets", "42"))),
		SecSessions, sec(row("name", "admin", "address", "198.51.100.3", "via", "winbox"))),
		SecSniffer, sec(row("running", "true")))
	if got, _ := SnapshotOf(busy); got.Hash() != base.Hash() {
		t.Error("the hash changed although only counters and runtime state did")
	}
	// The same answers again: the same bytes.
	if again, _ := SnapshotOf(cleanAudit()); again.JSON() != base.JSON() {
		t.Error("equal snapshots are not equal bytes")
	}
	// A setting changed.
	changed, _ := SnapshotOf(with(cleanAudit(), SecBridgeSettings, sec(row("use-ip-firewall", "true", "allow-fast-path", "true"))))
	if changed.Hash() == base.Hash() {
		t.Error("the hash did not change with use-ip-firewall")
	}
	graph, _ := SnapshotOf(with(cleanAudit(), SecGraphingInterface, sec(row("interface", "all"))))
	if graph.Hash() == base.Hash() || len(graph.Hash()) != 64 {
		t.Errorf("hash %q", graph.Hash())
	}
}

// A failed read makes the pass incomplete: what was read is not the
// device's configuration and is not stored as if it were.
func TestSnapshotIncompleteAfterAFailedRead(t *testing.T) {
	snap, complete := SnapshotOf(with(cleanAudit(), SecScheduler, failedSec))
	if complete {
		t.Fatal("a pass with a failed read counts as complete")
	}
	if _, ok := snap.Sections[SecScheduler]; ok || strings.Contains(snap.JSON(), "i/o timeout") {
		t.Fatalf("the failed read is in the snapshot: %s", snap.JSON())
	}
}

func TestRuntimeRoundTrip(t *testing.T) {
	rt := RuntimeOf(cleanAudit(), t0)
	if rt.CPUForwarded == nil || *rt.CPUForwarded != 1000 || !rt.At.Equal(t0) {
		t.Fatalf("runtime = %+v", rt)
	}
	back, ok := ParseRuntime(rt.JSON())
	if !ok || *back.CPUForwarded != 1000 || !back.At.Equal(t0) {
		t.Fatalf("round trip = %+v, %v", back, ok)
	}
	// No bridge settings read: no counter, and nothing to compare with later.
	none := RuntimeOf(with(cleanAudit(), SecBridgeSettings, skippedSec), t0.Add(time.Hour))
	if none.CPUForwarded != nil {
		t.Fatalf("runtime without the read = %+v", none)
	}
	for _, raw := range []string{"", "{}", "not json"} {
		if _, ok := ParseRuntime(raw); ok {
			t.Errorf("ParseRuntime(%q) ok", raw)
		}
	}
}
