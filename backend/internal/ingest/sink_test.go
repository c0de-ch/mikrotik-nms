package ingest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database/dbtest"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

// aPass is three clients as one pass of the client discovery reports them.
func aPass() []edgeproto.Observation {
	return []edgeproto.Observation{
		seen("02:00:5E:10:00:01", edgeproto.ClientSeen{IP: "192.0.2.101", HostName: "laptop-1", Source: "dhcp", DeviceName: "rtr"}),
		seen("02:00:5E:10:00:02", edgeproto.ClientSeen{IP: "192.0.2.102", Source: "arp", DeviceName: "sw24"}),
		seen("02:00:5E:10:00:03", edgeproto.ClientSeen{IP: "192.0.2.103", Source: "dhcp", DeviceName: "kea"}),
	}
}

// broken is an observation whose apply fails.
var broken = edgeproto.Observation{Kind: edgeproto.KindClientSeen, Subject: "02:00:5E:10:00:09", Body: "not a client"}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An Emit without a transaction of the caller's is committed whole.
func TestSinkEmitCommitsThePass(t *testing.T) {
	db := dbtest.Open(t)
	if err := NewSink(db).Emit(context.Background(), nil, aPass()...); err != nil {
		t.Fatal(err)
	}
	if got, want := lookupRows(t, db), []string{
		"02:00:5E:10:00:01|192.0.2.101|laptop-1||dhcp|rtr|||||||||||",
		"02:00:5E:10:00:02|192.0.2.102|||arp|sw24|||||||||||",
		"02:00:5E:10:00:03|192.0.2.103|||dhcp|kea|||||||||||",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mac_lookup = %q, want %q", got, want)
	}
	if n := count(t, db, "client_history"); n != 3 {
		t.Errorf("client_history has %d rows, want 3", n)
	}
}

// When one observation of an Emit fails to apply, the whole Emit is rolled
// back: the rows the others wrote — a new client, an update of a stored one,
// their history — are not there, and what was stored before is unchanged.
func TestSinkEmitRollsBackWhenAnApplyFails(t *testing.T) {
	db := dbtest.Open(t)
	sink := NewSink(db)
	stored := seen("02:00:5E:10:00:01", edgeproto.ClientSeen{IP: "192.0.2.1", HostName: "old-name", Source: "arp", DeviceName: "sw48"})
	if err := sink.Emit(context.Background(), nil, stored); err != nil {
		t.Fatal(err)
	}
	lookupBefore, historyBefore := lookupRows(t, db), historyRows(t, db)

	err := sink.Emit(context.Background(), nil, append(aPass(), broken)...)
	if err == nil || !strings.Contains(err.Error(), "want edgeproto.ClientSeen") {
		t.Fatalf("Emit with a broken observation = %v, want its apply error", err)
	}
	if got := lookupRows(t, db); strings.Join(got, "\n") != strings.Join(lookupBefore, "\n") {
		t.Errorf("mac_lookup after a failed Emit = %q, want it as before: %q", got, lookupBefore)
	}
	if got := historyRows(t, db); strings.Join(got, "\n") != strings.Join(historyBefore, "\n") {
		t.Errorf("client_history after a failed Emit = %q, want it as before: %q", got, historyBefore)
	}

	// The failure left no transaction open: the next Emit is written.
	if err := sink.Emit(context.Background(), nil, aPass()...); err != nil {
		t.Fatalf("Emit after a failed one: %v", err)
	}
	if n := count(t, db, "mac_lookup"); n != 3 {
		t.Errorf("mac_lookup has %d rows after the next Emit, want 3", n)
	}
}

// With a collect half's transaction the observations are written in it and
// stand or fall with the collect half's own writes: Emit commits nothing.
func TestSinkEmitWritesIntoTheCollectHalfsTransaction(t *testing.T) {
	for _, commit := range []bool{false, true} {
		db := dbtest.Open(t)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		// The collect half's own state, committed with its observations.
		if _, err := tx.Exec(`INSERT INTO app_settings (key, value) VALUES ('test_cursor', '7')`); err != nil {
			t.Fatal(err)
		}
		if err := NewSink(db).Emit(context.Background(), tx, aPass()...); err != nil {
			t.Fatal(err)
		}
		var inTx int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM mac_lookup`).Scan(&inTx); err != nil {
			t.Fatal(err)
		}
		if inTx != 3 {
			t.Errorf("the transaction sees %d lookup rows, want the 3 just emitted", inTx)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatalf("the caller ends its transaction: %v", err)
		}

		wantClients, wantState := 0, 0
		if commit {
			wantClients, wantState = 3, 1
		}
		if n := count(t, db, "mac_lookup"); n != wantClients {
			t.Errorf("commit=%v: mac_lookup has %d rows, want %d", commit, n, wantClients)
		}
		if n := count(t, db, "client_history"); n != wantClients {
			t.Errorf("commit=%v: client_history has %d rows, want %d", commit, n, wantClients)
		}
		var state int
		if err := db.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key = 'test_cursor'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != wantState {
			t.Errorf("commit=%v: the collect half's own row is there %d times, want %d", commit, state, wantState)
		}
	}
}

// An Emit whose context is done writes nothing; an Emit of nothing opens no
// transaction at all (a sink without a database answers it).
func TestSinkEmitWithoutWork(t *testing.T) {
	db := dbtest.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewSink(db).Emit(ctx, nil, aPass()...); err == nil {
		t.Error("Emit with a cancelled context succeeded")
	}
	if n := count(t, db, "mac_lookup") + count(t, db, "client_history"); n != 0 {
		t.Errorf("%d rows written with a cancelled context", n)
	}
	if err := NewSink(nil).Emit(context.Background(), nil); err != nil {
		t.Errorf("Emit of no observations = %v", err)
	}
}
