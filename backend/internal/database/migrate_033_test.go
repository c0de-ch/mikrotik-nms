package database

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 033 only adds: the previous release's binary must run on the new
// schema. Its statements against the two tables that gained columns — the
// finding upsert, the bridge port upsert and both reads, each by column name
// — work unchanged, rows written before the migration read back with NULL in
// the new columns, and nothing that existed is gone.
func TestMigration033KeepsThePreviousReleaseWorking(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upgrade.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 32); err != nil {
		t.Fatalf("migrate to the schema before 033: %v", err)
	}
	before := schemaNames(t, db)

	// The statements of the release before, as that release writes them.
	const (
		addDevice     = `INSERT INTO devices (id, address, identity, username, password_enc, api_port, status, tags) VALUES ('d1', '192.0.2.1', 'sw', 'nms', '', 8728, 'online', '[]')`
		upsertFinding = `INSERT INTO device_findings (device_id, kind, severity, detail) VALUES (?, ?, ?, ?)
			ON CONFLICT(device_id, kind, detail) DO UPDATE SET severity = excluded.severity, last_seen = CURRENT_TIMESTAMP, count = device_findings.count + 1`
		listFindings = `SELECT id, device_id, kind, severity, detail, first_seen, last_seen, count FROM device_findings WHERE device_id = ?`
		upsertPort   = `INSERT INTO bridge_port_status (id, device_id, bridge_name, port_interface, role, status,
			edge, point_to_point, path_cost, designated_bridge, topology_changes, forwarding, learning, inactive, disabled,
			hw_offload, hw_offload_group, pvid, port_state, edge_config, last_polled)
			VALUES (?, 'd1', 'bridge', ?, 'designated', 'forwarding', 0, 1, 10, '', 0, 1, 1, 0, 0, 1, 'switch1', 1, 'in-bridge', 'auto', CURRENT_TIMESTAMP)
			ON CONFLICT(device_id, bridge_name, port_interface) DO UPDATE SET hw_offload = excluded.hw_offload, last_polled = CURRENT_TIMESTAMP`
		listPorts = `SELECT id, port_interface, hw_offload, COALESCE(hw_offload_group, '') FROM bridge_port_status WHERE device_id = 'd1'`
	)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	mustExec(addDevice)
	mustExec(upsertFinding, "d1", "parallel_api_poller", "warn", "user x from 198.51.100.2 via api")
	mustExec(upsertPort, "d1:bridge:ether1", "ether1")

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Everything that existed still exists.
	after := schemaNames(t, db)
	for name := range before {
		if !after[name] {
			t.Errorf("033 removed %s", name)
		}
	}
	for _, name := range []string{"device_audit", "device_audit_runs", "flow_export_watches"} {
		if !after[name] {
			t.Errorf("033 did not create %s", name)
		}
	}

	// The old statements, on the new schema.
	mustExec(upsertFinding, "d1", "parallel_api_poller", "info", "user x from 198.51.100.2 via api") // a repeat
	mustExec(upsertFinding, "d1", "login_failure", "info", "user y")
	mustExec(upsertPort, "d1:bridge:ether1", "ether1")
	mustExec(upsertPort, "d1:bridge:ether2", "ether2")
	for _, q := range []string{listFindings, listPorts} {
		rows, err := db.Query(q, queryArgs(q)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 2 {
			t.Errorf("%d rows, want 2:\n%s", n, q)
		}
	}

	// The rows from before the migration: untouched, the new columns NULL.
	var count int
	var title, source sql.NullString
	if err := db.QueryRow(`SELECT count, title, source FROM device_findings WHERE kind = 'parallel_api_poller'`).Scan(&count, &title, &source); err != nil {
		t.Fatal(err)
	}
	if count != 2 || title.Valid || source.Valid {
		t.Errorf("the old finding: count %d, title %v, source %v; want 2 and NULLs", count, title, source)
	}
	var hw sql.NullInt64
	var horizon sql.NullString
	if err := db.QueryRow(`SELECT hw, horizon FROM bridge_port_status WHERE port_interface = 'ether1'`).Scan(&hw, &horizon); err != nil {
		t.Fatal(err)
	}
	if hw.Valid || horizon.Valid {
		t.Errorf("the old bridge port: hw %v, horizon %v; want NULLs", hw, horizon)
	}
}

func queryArgs(q string) []any {
	if strings.Contains(q, "?") {
		return []any{"d1"}
	}
	return nil
}

func schemaNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE name NOT LIKE 'goose_%' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}
