package database

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// An install that ran the previous release keeps every exporter it had —
// accepted automatically or by hand — across migration 031, while the old
// auto-accept switch reads false (so the previous binary on this schema stops
// accepting too) and the new one starts off.
func TestMigration031KeepsExportersAndTurnsAutoAcceptOff(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upgrade.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	// Everything before 031 (030 does not have to exist: UpTo applies what
	// there is up to that version).
	if err := goose.UpTo(db, "migrations", 30); err != nil {
		t.Fatalf("migrate to the schema before 031: %v", err)
	}
	// The state of a running install: auto-accept on (the old default), one
	// auto-accepted exporter and one added by hand.
	var old string
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'flow_auto_accept_devices'`).Scan(&old); err != nil || old != "true" {
		t.Fatalf("before: flow_auto_accept_devices = %q, %v (want the old default true)", old, err)
	}
	for _, q := range []string{
		`INSERT INTO flow_exporters (name, address, kind, enabled, auto) VALUES ('core-router', '192.0.2.1', 'routeros', 1, 1)`,
		`INSERT INTO flow_exporters (name, address, kind, enabled, auto) VALUES ('firewall', '192.0.2.2', 'opnsense', 1, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	rows, err := db.Query(`SELECT name, enabled, auto FROM flow_exporters ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		var enabled, auto int
		if err := rows.Scan(&name, &enabled, &auto); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Errorf("exporter %s no longer enabled after the migration", name)
		}
		got = append(got, name)
	}
	if len(got) != 2 || got[0] != "core-router" || got[1] != "firewall" {
		t.Fatalf("exporters after the migration = %v, want both kept", got)
	}
	settings := map[string]string{}
	for _, key := range []string{"flow_auto_accept_devices", "flow_auto_accept_classes"} {
		var v string
		if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		settings[key] = v
	}
	if settings["flow_auto_accept_devices"] != "false" {
		t.Errorf("flow_auto_accept_devices = %q after the migration, want false (N-1 rule)", settings["flow_auto_accept_devices"])
	}
	if settings["flow_auto_accept_classes"] != "" {
		t.Errorf("flow_auto_accept_classes = %q, want '' (off)", settings["flow_auto_accept_classes"])
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM flow_sender_policy`).Scan(&n); err != nil || n != 0 {
		t.Errorf("flow_sender_policy rows = %d, %v (want an empty block list)", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM flow_export_checks`).Scan(&n); err != nil || n != 0 {
		t.Errorf("flow_export_checks rows = %d, %v", n, err)
	}
}

// The schema rule: 031 only adds. Every table, index and column of the
// schema before it is still there with its definition unchanged.
func TestMigration031IsAdditive(t *testing.T) {
	schemaAt := func(version int64) map[string]string {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		goose.SetBaseFS(migrations)
		if err := goose.SetDialect("sqlite3"); err != nil {
			t.Fatal(err)
		}
		if version > 0 {
			err = goose.UpTo(db, "migrations", version)
		} else {
			err = goose.Up(db, "migrations")
		}
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query(`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'goose_%' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, ddl string
			if err := rows.Scan(&name, &ddl); err != nil {
				t.Fatal(err)
			}
			out[name] = ddl
		}
		return out
	}
	before, after := schemaAt(30), schemaAt(31)
	for name, ddl := range before {
		if after[name] != ddl {
			t.Errorf("031 changed %s:\n before: %s\n after:  %s", name, ddl, after[name])
		}
	}
	for _, name := range []string{"flow_sender_policy", "flow_export_checks"} {
		if after[name] == "" {
			t.Errorf("031 did not create %s", name)
		}
	}
}
