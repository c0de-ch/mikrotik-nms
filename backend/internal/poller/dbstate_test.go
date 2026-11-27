package poller

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Golden DB state: what the scheduled loops store in the hour the call plans
// simulate (fleetHourOf) — every table a loop writes, as it stands after the
// hour and a minute, one file per table in testdata/dbstate, and
// tables.golden naming every table of the schema with its row count and
// what writes it. The refactors of the pollers (M1: Runner + Sink +
// ConfigSource, one PR per poller) must leave these files byte for byte the
// same; a change in what the pollers store is a diff here to review, as a
// change in what the devices are sent is a diff in the call plans.
//
//	go test ./internal/poller -run TestDBState -update    # rewrite the files
//
// The normalisation, the same for every file and stated in each header:
//
//   - A time on the simulated clock is its offset from the start of the hour
//     (+00:05:00, -01:00:00 before it); the header says how the column
//     stores it (INTEGER unix seconds, or which TEXT layout).
//   - A time from the wall clock is a placeholder: <sqlite-now> where SQLite
//     stamped it (CURRENT_TIMESTAMP, in the statement or as the column's
//     default), <wall-clock> where the Go code took it from time.Now().
//     wallClockColumns lists those columns. Every time column is checked to
//     be what the catalogue says, so the list cannot go stale.
//   - A generated id (UUID) is <uuid-n>, numbered by first appearance in the
//     file. An AUTOINCREMENT id is kept where the inserts come in a
//     deterministic order, and left out where they do not (dbTable.unstable).
//   - A REAL value has three decimals. JSON text has its keys sorted and its
//     simulated times as offsets, and is printed indented under its row.
//   - Rows are ordered by the primary key — by all their columns where the
//     key is a generated id or an unstable one.
//   - A table of more than dbFullRowsMax rows is a summary — rows per device,
//     and per series the row count and each column's range — plus the full
//     rows of a few series (dbSummary).
//
// Columns only the harness's seed writes (the device rows' address, port and
// credentials, the ping targets' configuration) are left out, and checked
// unchanged after the hour; so are the tables the seed fills and no loop
// writes. dbEmptyTables gives the reason for every table the hour leaves
// empty.

// dbFullRowsMax is the most rows a golden file shows in full.
const dbFullRowsMax = 500

// dbClock is the clock a time column is set from.
type dbClock int

const (
	// simClock: the simulated clock — a loop's now, or the time of a read.
	simClock dbClock = iota + 1
	// sqliteNow: CURRENT_TIMESTAMP — the wall clock, stamped by SQLite.
	sqliteNow
	// goNow: time.Now() in the writer — the wall clock, stamped by Go.
	goNow
)

// dbTable is how one table the loops write appears in the goldens.
type dbTable struct {
	name string
	// writers: the code that writes the table in the hour, caller → query.
	writers []string
	// seedOnly: columns only the harness's seed writes — left out, and
	// checked unchanged after the hour.
	seedOnly []string
	// times: the columns holding a time, by the clock that sets them.
	times map[string]dbClock
	// json: text columns holding JSON.
	json []string
	// uuids: columns holding generated ids.
	uuids []string
	// unstable: AUTOINCREMENT id columns left out, with why their order is
	// not deterministic.
	unstable map[string]string
	// summary: how the table is shown when it has more than dbFullRowsMax
	// rows (nil: always in full).
	summary *dbSummary
}

// dbSummary shows a big table as a summary: the rows per device, and per
// series — the rows sharing the values of the series columns — the row
// count and the range of every other column; then the full rows of the
// series in full.
type dbSummary struct {
	series []string   // the columns naming a series; the first is the device
	full   [][]string // the series printed in full: their values of series
}

// dbTables are the tables a loop writes in the hour.
var dbTables = []dbTable{
	{
		name: "bridge_port_status",
		writers: []string{"NetworkHealthPoller.readBridges → queries.UpsertBridgePortStatus " +
			"(stale rows: NetworkHealthPoller.maintain → queries.DeleteStaleBridgeRows)"},
		times: map[string]dbClock{"last_polled": sqliteNow},
	},
	{
		name: "bridge_status",
		writers: []string{"NetworkHealthPoller.readBridges → queries.UpsertBridgeStatus " +
			"(stale rows: NetworkHealthPoller.maintain → queries.DeleteStaleBridgeRows)"},
		times: map[string]dbClock{"last_polled": sqliteNow},
	},
	{
		name: "bridge_vlans",
		writers: []string{"NetworkHealthPoller.readVLANs → queries.UpsertBridgeVLAN " +
			"(stale rows: NetworkHealthPoller.maintain → queries.DeleteStaleBridgeVLANs)"},
		times: map[string]dbClock{"last_polled": sqliteNow},
	},
	{
		name:    "client_history",
		writers: []string{"ClientDiscoveryPoller.store → queries.InsertClientHistory"},
		times:   map[string]dbClock{"recorded_at": sqliteNow},
		unstable: map[string]string{"id": "ClientDiscoveryPoller.store inserts a pass's clients in the order of a Go map, " +
			"which is random"},
	},
	{
		name:    "client_signal_samples",
		writers: []string{"ConnectivityPoller.collectSignals → queries.InsertClientSignalSample"},
		times:   map[string]dbClock{"recorded_at": simClock},
	},
	{
		name:    "device_audit",
		writers: []string{"Auditor.storeSnapshot → queries.InsertDeviceAudit"},
		times:   map[string]dbClock{"taken_at": simClock},
		json:    []string{"snapshot", "findings"},
	},
	{
		name:    "device_audit_runs",
		writers: []string{"Auditor.begin → queries.InsertAuditRun", "Auditor.finish → queries.FinishAuditRun"},
		times:   map[string]dbClock{"started_at": simClock, "finished_at": simClock},
		json:    []string{"runtime", "findings", "checks"},
	},
	{
		name: "device_findings",
		writers: []string{"Auditor.storeFindings → queries.UpsertAuditFinding " +
			"(and DeleteAuditFindingsExcept, ReleaseAuditFindings for what a pass no longer sees)"},
		times: map[string]dbClock{"first_seen": simClock, "last_seen": simClock},
		json:  []string{"fix"},
	},
	{
		name: "device_hours",
		writers: []string{"PortStatsCollector.flush → queries.RollupDeviceHours (when the flush crosses an hour; " +
			"PortStatsCollector.startupRollup at the start)"},
		times: map[string]dbClock{"bucket": simClock},
	},
	{
		name: "device_minutes",
		writers: []string{"LoadRecorder.FlushMinutes (the port-stats collector's CoverageSink, from " +
			"PortStatsCollector.flush) → queries.UpsertDeviceMinutes; fed by the collector's Tier 0 coverage, " +
			"Manager.pollDevice → LoadRecorder.NoteLiveness, CPUSampler.sample / samplePost → " +
			"LoadRecorder.AddSample / AddPostSample, and the routeros observer → LoadRecorder.Command / Session"},
		times: map[string]dbClock{"bucket": simClock},
	},
	{
		name:    "device_uplinks",
		writers: []string{"Manager.collectUplinks → queries.ReplaceDeviceUplinks (gateway-host rows: Manager.resolveGatewayHosts → queries.ReplaceGatewayHosts)"},
		times:   map[string]dbClock{"last_seen": sqliteNow},
	},
	{
		name: "devices",
		writers: []string{
			"Manager.pollDevice (liveness) → queries.MarkDeviceOnline: status, last_seen, last_error",
			"Manager.refreshDeviceInfo (Tier 6) → queries.UpdateDeviceHealth: cpu_load, memory, uptime, last_seen; " +
				"queries.UpdateDeviceInfo: platform, board, ros_version, firmware_version, architecture",
			"Manager.probeClass → queries.SetDeviceClass: device_class, class_probed_at, cpu_count, cpu_mhz_max",
			"CPUSampler.sample (Tier 1) → queries.UpdateDeviceLoad: cpu_load, memory_used, uptime",
		},
		seedOnly: []string{"address", "identity", "username", "password_enc", "use_tls", "api_port", "tags", "notes",
			"created_at", "class_override", "poll_profile", "polling_paused_until", "pin_cert_sha256"},
		times: map[string]dbClock{"last_seen": sqliteNow, "updated_at": sqliteNow, "class_probed_at": simClock},
	},
	{
		name:    "firmware_status",
		writers: []string{"Manager.refreshDeviceInfo (Tier 6) → FirmwareChecker.RecordDevice → queries.UpsertDeviceFirmware"},
		times:   map[string]dbClock{"last_checked": sqliteNow, "updated_at": sqliteNow},
	},
	{
		name:    "interface_relations",
		writers: []string{"PortStatsCollector.collectDeviceRelations (Tier 3c) → queries.ReplaceInterfaceRelations"},
		times:   map[string]dbClock{"updated_at": sqliteNow},
	},
	{
		name: "interface_state",
		writers: []string{"NetworkHealthPoller.monitorPorts → queries.UpsertInterfaceState " +
			"(stale rows: NetworkHealthPoller.maintain → queries.DeleteStaleInterfaceStates)"},
		times: map[string]dbClock{"last_polled": sqliteNow},
	},
	{
		name:    "interfaces",
		writers: []string{"PortStatsCollector.syncInventory (Tier 0) → queries.SyncInterfaces"},
		times:   map[string]dbClock{"last_updated": sqliteNow},
	},
	{
		name:    "links",
		writers: []string{"Manager.pollTopology → topology.Builder.Build → queries.UpsertLink (MarkStaleLinksDown, DeleteOldLinks)"},
		times:   map[string]dbClock{"last_seen": sqliteNow},
		uuids:   []string{"id"},
	},
	{
		name:    "mac_lookup",
		writers: []string{"ClientDiscoveryPoller.store → queries.UpsertMACLookup", "WifiTracker.poll → queries.UpsertMACLookup"},
		times:   map[string]dbClock{"updated_at": sqliteNow},
	},
	{
		name:    "neighbors",
		writers: []string{"Manager.collectNeighbors (Tier 3d) → queries.UpsertNeighbor (stale rows: Manager.sweepNeighbors)"},
		times:   map[string]dbClock{"last_seen": sqliteNow},
	},
	{
		name:    "ping_samples",
		writers: []string{"ConnectivityPoller.persistAndPublish → queries.InsertPingSample"},
		times:   map[string]dbClock{"recorded_at": goNow},
	},
	{
		name:     "ping_targets",
		writers:  []string{"ConnectivityPoller.poll → queries.UpdatePingTargetAddress: a client watch's address"},
		seedOnly: []string{"kind", "mac_address", "label", "device_id", "enabled", "created_at", "src_address", "src_interface"},
	},
	{
		name:    "port_hosts",
		writers: []string{"PortStatsCollector.collectDeviceHosts (Tier 3b) → queries.UpsertPortHosts"},
		times:   map[string]dbClock{"first_seen": simClock, "last_seen": simClock},
	},
	{
		name: "port_stats_1h",
		writers: []string{"PortStatsCollector.flush → queries.RollupPortStats1h (when the flush crosses an hour; " +
			"PortStatsCollector.startupRollup at the start)"},
		times: map[string]dbClock{"bucket": simClock},
	},
	{
		name:    "port_stats_1m",
		writers: []string{"PortStatsCollector.flush → queries.InsertPortStats1m"},
		times:   map[string]dbClock{"bucket": simClock},
		summary: &dbSummary{
			series: []string{"device_id", "interface_name"},
			// A C3 router's port without drop and error counters (read every
			// 15 s), a C2 switch's access port with a bad cable (30 s), and a
			// C1 switch's uplink through a congestion episode (60 s).
			full: [][]string{{"rtr", "ether1"}, {"sw24", "ether3"}, {"sw48", "sfp-sfpplus1"}},
		},
	},
	{
		name:    "wifi_history",
		writers: []string{"WifiTracker.emitFromSnapshot / handleLogEvent / emitLeave → queries.InsertWifiHistory"},
		times:   map[string]dbClock{"recorded_at": sqliteNow},
		unstable: map[string]string{"id": "WifiTracker.poll emits the joins of one read in the order of a Go map, " +
			"which is random"},
	},
}

// dbEmptyTables are the tables the hour leaves empty, with why.
var dbEmptyTables = map[string]string{
	"alerts":                   "the alert engine is not part of the hour (Manager.Start runs it; it reads only the database)",
	"alert_silences":           "written only by the API (an admin's silence)",
	"audit_log":                "the API's admin actions, and Manager.probeClass when a class change is accepted — the hour's probes agree with the stored classes",
	"device_address_proposals": "IP auto-follow (auto_follow_ip) is off, and no device is heard at another address",
	"dns_servers":              "written only by the API (settings)",
	"map_positions":            "written only by the API (an admin's saved map layout)",
	"firmware_releases":        "the release feed (FirmwareChecker.fetch) is not part of the hour: it reads the internet, not a device",
	"flow_1h":                  "the flow collector is not part of the hour (off unless MIKROTIK_NMS_FLOW_LISTEN is set)",
	"flow_1h_meta":             "the flow collector is not part of the hour",
	"flow_1m":                  "the flow collector is not part of the hour",
	"flow_1m_meta":             "the flow collector is not part of the hour",
	"flow_export_checks":       "the flow collector is not part of the hour",
	"flow_export_watches":      "the flow collector is not part of the hour (a watch starts when it accepts an exporter)",
	"flow_exporter_ifaces":     "the flow collector is not part of the hour",
	"flow_exporters":           "the flow collector is not part of the hour, and an exporter is added by an admin",
	"flow_points":              "the flow collector is not part of the hour",
	"flow_prefixes":            "the flow collector is not part of the hour",
	"flow_sender_policy":       "the flow collector is not part of the hour",
	"flow_templates":           "the flow collector is not part of the hour",
	"loop_events":              "network health records a loop, a MAC flap, a port going down, flapping or disabled, or an address move: none happens in the hour",
	"notification_channels":    "written only by the API",
	"password_reset_tokens":    "written only by the API",
	"speed_samples":            "the speed-test loop is not part of the hour, and no test is configured",
	"speed_tests":              "written only by the API",
	"traceroute_runs":          "a traceroute is captured on demand, or when an internet target loses at least traceroute_loss_threshold (off by default): every probe of the hour is answered",
	"traffic_samples":          "the on-demand live stream (TrafficManager) is not part of the hour: nobody subscribes",
	"upgrade_job_devices":      "firmware upgrades are an admin's action",
	"upgrade_jobs":             "firmware upgrades are an admin's action",
	"users":                    "the harness logs nobody in; only the API writes users",
	"vlan_labels":              "written only by the API (an admin's labels)",
}

// dbSeededTables are the tables filled before the hour and not written in
// it: checked unchanged.
var dbSeededTables = map[string]string{
	"app_settings":     "the migrations' defaults and NewManager's AdoptIntervalSettings, before the hour; the loops only read it",
	"goose_db_version": "the migrations",
}

// dbBookkeeping are SQLite's own tables.
var dbBookkeeping = map[string]string{
	"sqlite_sequence": "SQLite's AUTOINCREMENT counters, which follow the tables above",
}

// wallClockColumns are the columns of the hour's tables that hold the wall
// clock instead of the simulated one: what v2 ingest semantics (an
// observed_at carried from the read) will change. Each is checked against
// the values the hour stored.
var wallClockColumns = map[string]dbClock{
	// SQLite stamps CURRENT_TIMESTAMP, in the statement or as the default.
	"bridge_port_status.last_polled": sqliteNow, // queries.UpsertBridgePortStatus
	"bridge_status.last_polled":      sqliteNow, // queries.UpsertBridgeStatus
	"bridge_vlans.last_polled":       sqliteNow, // queries.UpsertBridgeVLAN
	"client_history.recorded_at":     sqliteNow, // queries.InsertClientHistory (the default)
	"device_uplinks.last_seen":       sqliteNow, // queries.ReplaceDeviceUplinks
	"devices.last_seen":              sqliteNow, // queries.MarkDeviceOnline, UpdateDeviceHealth
	"devices.updated_at":             sqliteNow, // every queries.*Device* update
	"firmware_status.last_checked":   sqliteNow, // queries.UpsertDeviceFirmware
	"firmware_status.updated_at":     sqliteNow, // queries.UpsertDeviceFirmware
	"interface_relations.updated_at": sqliteNow, // queries.ReplaceInterfaceRelations
	"interface_state.last_polled":    sqliteNow, // queries.UpsertInterfaceState
	"interfaces.last_updated":        sqliteNow, // queries.SyncInterfaces
	"links.last_seen":                sqliteNow, // queries.UpsertLink
	"mac_lookup.updated_at":          sqliteNow, // queries.UpsertMACLookup
	"neighbors.last_seen":            sqliteNow, // queries.UpsertNeighbor
	"wifi_history.recorded_at":       sqliteNow, // queries.InsertWifiHistory (the default)
	// Go stamps time.Now(): the writer has no clock of its own.
	"ping_samples.recorded_at": goNow, // queries.InsertPingSample, for a sample without a time
}

func (c dbClock) String() string {
	switch c {
	case simClock:
		return "simulated clock"
	case sqliteNow:
		return "SQLite's CURRENT_TIMESTAMP (wall clock)"
	case goNow:
		return "time.Now() in Go (wall clock)"
	}
	return "?"
}

func (c dbClock) placeholder() string {
	switch c {
	case sqliteNow:
		return "<sqlite-now>"
	case goNow:
		return "<wall-clock>"
	}
	return ""
}

// dbSeed is what the harness stored before the hour: per table, its rows as
// SQLite quotes them (quote()), restricted to the seed-only columns for the
// tables the loops write.
type dbSeed map[string][]string

// dbState is what the hour stored, normalised.
type dbState struct {
	files    map[string]string // golden file name → content
	rows     map[string]int    // table → rows after the hour
	problems []string          // where the database is not what the catalogue says
	// nmsCmds is device_minutes.nms_cmds by device and minute of the hour.
	nmsCmds map[string]map[int]int64
}

func (s *dbState) fail(format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

// seedColumns is what captureSeed keeps of a table: the key and the
// seed-only columns of a table the loops write, every column of a seeded one
// (nil: not kept).
func seedColumns(db *sql.DB, table string) ([]string, error) {
	if _, ok := dbSeededTables[table]; ok {
		cols, err := tableColumns(db, table)
		if err != nil {
			return nil, err
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.name
		}
		return names, nil
	}
	for _, spec := range dbTables {
		if spec.name == table && len(spec.seedOnly) > 0 {
			cols, err := tableColumns(db, table)
			if err != nil {
				return nil, err
			}
			var keep []string
			for _, c := range cols {
				if c.pk > 0 || slices.Contains(spec.seedOnly, c.name) {
					keep = append(keep, c.name)
				}
			}
			return keep, nil
		}
	}
	return nil, nil
}

// quotedRows returns the rows of table's cols as SQLite quotes them, ordered
// by all of them.
func quotedRows(db *sql.DB, table string, cols []string) ([]string, error) {
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = `quote("` + c + `")`
	}
	query := `SELECT ` + strings.Join(sel, ` || ' | ' || `) + ` FROM "` + table + `"`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// captureSeed keeps what the harness stored before the hour (dbSeed).
func (h *callPlan) captureSeed() dbSeed {
	seed := dbSeed{}
	names, err := tableNames(h.db)
	if err != nil {
		h.t.Fatalf("db state: %v", err)
	}
	for _, table := range names {
		cols, err := seedColumns(h.db, table)
		if err != nil {
			h.t.Fatalf("db state: %v", err)
		}
		if cols == nil {
			continue
		}
		if seed[table], err = quotedRows(h.db, table, cols); err != nil {
			h.t.Fatalf("db state: %v", err)
		}
	}
	return seed
}

func tableNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// dbColumn is one column of a table, as PRAGMA table_info gives it.
type dbColumn struct {
	name, dflt string
	pk         int // position in the primary key, 0 = not part of it
}

func tableColumns(db *sql.DB, table string) ([]dbColumn, error) {
	rows, err := db.Query(`SELECT name, COALESCE(dflt_value, ''), pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dbColumn
	for rows.Next() {
		var c dbColumn
		if err := rows.Scan(&c.name, &c.dflt, &c.pk); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// dumpState reads what the hour stored and renders the golden files; seed
// is what was stored before the hour, wallFrom and wallTo bound the wall
// clock of the run.
func (h *callPlan) dumpState(seed dbSeed, wallFrom, wallTo time.Time) *dbState {
	st := &dbState{files: map[string]string{}, rows: map[string]int{}, nmsCmds: map[string]map[int]int64{}}
	d := &dumper{
		st: st, db: h.db,
		simFrom: callPlanStart.Add(-24 * time.Hour), simTo: callPlanStart.Add(24 * time.Hour),
		wallFrom: wallFrom.UTC().Truncate(time.Second), wallTo: wallTo.UTC().Add(time.Second),
	}
	names, err := tableNames(h.db)
	if err != nil {
		st.fail("db state: list the tables: %v", err)
		return st
	}

	// Every table of the schema is accounted for, once.
	specs := map[string]dbTable{}
	for _, spec := range dbTables {
		specs[spec.name] = spec
	}
	inSchema := map[string]bool{}
	for _, table := range names {
		inSchema[table] = true
		var n int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n); err != nil {
			st.fail("db state: count %s: %v", table, err)
			continue
		}
		st.rows[table] = n
		_, written := specs[table]
		_, empty := dbEmptyTables[table]
		_, seeded := dbSeededTables[table]
		_, kept := dbBookkeeping[table]
		switch places := btoi(written) + btoi(empty) + btoi(seeded) + btoi(kept); {
		case places == 0:
			st.fail("table %s (%d rows after the hour) is in none of dbTables, dbEmptyTables, dbSeededTables, dbBookkeeping: "+
				"say what writes it in the hour, or why the hour leaves it empty", table, n)
		case places > 1:
			st.fail("table %s is listed in more than one of dbTables, dbEmptyTables, dbSeededTables, dbBookkeeping", table)
		case empty && n > 0:
			st.fail("table %s holds %d rows after the hour, but dbEmptyTables says the hour leaves it empty (%q): "+
				"move it to dbTables with the code that writes it", table, n, dbEmptyTables[table])
		case written && n == 0:
			st.fail("table %s is empty after the hour, but dbTables says %s writes it: "+
				"find out why it no longer does, or move it to dbEmptyTables with the reason", table, strings.Join(specs[table].writers, "; "))
		}
	}
	for _, list := range []map[string]string{dbEmptyTables, dbSeededTables, dbBookkeeping} {
		for table := range list {
			if !inSchema[table] {
				st.fail("table %s is listed, but the schema has no such table any more: remove the entry", table)
			}
		}
	}
	for table := range specs {
		if !inSchema[table] {
			st.fail("dbTables lists %s, but the schema has no such table any more: remove the entry", table)
		}
	}

	// What the seed stored stands.
	for _, table := range slices.Sorted(maps.Keys(seed)) {
		before := seed[table]
		cols, err := seedColumns(h.db, table)
		if err != nil {
			st.fail("db state: %v", err)
			continue
		}
		after, err := quotedRows(h.db, table, cols)
		if err != nil {
			st.fail("db state: %v", err)
			continue
		}
		if !slices.Equal(before, after) {
			what := "the table"
			if _, ok := dbSeededTables[table]; !ok {
				what = "a seed-only column (" + strings.Join(specs[table].seedOnly, ", ") + ")"
			}
			st.fail("%s of %s changed during the hour, but the catalogue says only the harness's seed writes it: "+
				"dump what the loops write there.\n  before: %s\n  after:  %s",
				what, table, strings.Join(before, "\n          "), strings.Join(after, "\n          "))
		}
	}

	// The wall-clock list is the catalogue's.
	declared := map[string]dbClock{}
	for _, spec := range dbTables {
		for col, clock := range spec.times {
			if clock != simClock {
				declared[spec.name+"."+col] = clock
			}
		}
	}
	for col, clock := range declared {
		if wallClockColumns[col] != clock {
			st.fail("dbTables says %s holds %s; wallClockColumns does not list it so", col, clock)
		}
	}
	for col := range wallClockColumns {
		if _, ok := declared[col]; !ok {
			st.fail("wallClockColumns lists %s, but dbTables does not declare it a wall-clock column", col)
		}
	}

	for _, spec := range dbTables {
		if !inSchema[spec.name] {
			continue
		}
		body, err := d.table(spec)
		if err != nil {
			st.fail("db state: %s: %v", spec.name, err)
			continue
		}
		st.files[spec.name+".golden"] = body
	}
	st.files["tables.golden"] = d.index(names, specs)

	// device_minutes.nms_cmds, for the cross-check with the call plans.
	rows, err := h.db.Query(`SELECT device_id, bucket, nms_cmds FROM device_minutes`)
	if err != nil {
		st.fail("db state: device_minutes: %v", err)
		return st
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var bucket, cmds int64
		if err := rows.Scan(&id, &bucket, &cmds); err != nil {
			st.fail("db state: device_minutes: %v", err)
			return st
		}
		if st.nmsCmds[id] == nil {
			st.nmsCmds[id] = map[int]int64{}
		}
		st.nmsCmds[id][int((bucket-callPlanStart.Unix())/60)] = cmds
	}
	return st
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// dumper renders the tables.
type dumper struct {
	st               *dbState
	db               *sql.DB
	simFrom, simTo   time.Time // the window a simulated time must lie in
	wallFrom, wallTo time.Time // the wall clock of the run
	uuids            map[string]string
	timeFormats      map[string]map[string]bool // table.column → storage formats seen
}

// dbCell is one normalised value: text on the row's line, or — for JSON —
// an indented block under it.
type dbCell struct {
	text  string
	block string
	uuid  string // a generated id, numbered once the rows are in order
}

// dbRow is one normalised row.
type dbRow struct {
	cells []dbCell
	raw   []any
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The text layouts a stored time comes in.
var timeLayouts = []struct{ name, layout string }{
	{`TEXT "2006-01-02 15:04:05" (as CURRENT_TIMESTAMP writes it)`, "2006-01-02 15:04:05"},
	{`TEXT time.Time.String() ("2006-01-02 15:04:05 +0000 UTC", as the driver binds a time.Time)`, "2006-01-02 15:04:05.999999999 -0700 MST"},
	{`TEXT RFC 3339`, time.RFC3339Nano},
	{`TEXT "2006-01-02T15:04:05"`, "2006-01-02T15:04:05"},
}

// parseStoredTime reads a stored text time, with the name of its layout.
func parseStoredTime(s string) (time.Time, string, bool) {
	if i := strings.Index(s, " m="); i > 0 {
		s = s[:i] // a monotonic clock reading
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l.layout, s); err == nil {
			return t.UTC(), l.name, true
		}
	}
	return time.Time{}, "", false
}

// looksLikeTime reports a text that starts with a date and time, and when.
func looksLikeTime(s string) (time.Time, bool) {
	if len(s) < 19 {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s[:19]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// offset writes a simulated time as its offset from the start of the hour.
func offset(t time.Time) string {
	d := t.Sub(callPlanStart)
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	h, m, s := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	out := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, s)
	if ms := int(d % time.Second / time.Millisecond); ms != 0 || d%time.Millisecond != 0 {
		out += fmt.Sprintf(".%03d", ms)
	}
	return out
}

func (d *dumper) noteFormat(table, col, format string) {
	if d.timeFormats == nil {
		d.timeFormats = map[string]map[string]bool{}
	}
	key := table + "." + col
	if d.timeFormats[key] == nil {
		d.timeFormats[key] = map[string]bool{}
	}
	d.timeFormats[key][format] = true
}

// timeCell normalises a value of a time column.
func (d *dumper) timeCell(table, col string, clock dbClock, v any) dbCell {
	where := table + "." + col
	switch x := v.(type) {
	case nil:
		d.noteFormat(table, col, "NULL")
		return dbCell{text: "NULL"}
	case int64:
		if x == 0 {
			d.noteFormat(table, col, "INTEGER 0 (none)")
			return dbCell{text: "0"}
		}
		d.noteFormat(table, col, "INTEGER unix seconds")
		return d.clockCell(where, clock, time.Unix(x, 0).UTC(), strconv.FormatInt(x, 10), false)
	case string:
		t, layout, ok := parseStoredTime(x)
		if !ok {
			d.st.fail("%s is a time column, but holds %q, which is no time layout the dump knows", where, x)
			return dbCell{text: strconv.Quote(x)}
		}
		d.noteFormat(table, col, layout)
		return d.clockCell(where, clock, t, x, layout == timeLayouts[0].name)
	}
	d.st.fail("%s is a time column, but holds %T %v", where, v, v)
	return dbCell{text: fmt.Sprint(v)}
}

// clockCell checks that a time is from the clock the catalogue says, and
// renders it.
func (d *dumper) clockCell(where string, clock dbClock, t time.Time, raw string, sqliteLayout bool) dbCell {
	inSim := !t.Before(d.simFrom) && !t.After(d.simTo)
	inWall := !t.Before(d.wallFrom) && !t.After(d.wallTo)
	switch clock {
	case simClock:
		if !inSim {
			d.st.fail("%s should hold the simulated clock, but holds %s (%s)", where, raw, wallOrNot(inWall))
		}
		return dbCell{text: offset(t)}
	case sqliteNow:
		if !inWall || !sqliteLayout {
			d.st.fail("%s should hold SQLite's CURRENT_TIMESTAMP of the run, but holds %s (%s)", where, raw, wallOrNot(inWall))
		}
	case goNow:
		if !inWall {
			d.st.fail("%s should hold the wall clock of the run, but holds %s", where, raw)
		}
	}
	return dbCell{text: clock.placeholder()}
}

func wallOrNot(inWall bool) string {
	if inWall {
		return "the wall clock of the run"
	}
	return "neither the simulated nor the wall clock of the run"
}

// plainCell normalises a value of a column that is neither a time, nor JSON,
// nor a generated id — and fails a value that looks like one of them, which
// the catalogue has to declare.
func (d *dumper) plainCell(where string, v any) dbCell {
	switch x := v.(type) {
	case nil:
		return dbCell{text: "NULL"}
	case int64:
		return dbCell{text: strconv.FormatInt(x, 10)}
	case float64:
		return dbCell{text: strconv.FormatFloat(x, 'f', 3, 64)}
	case []byte:
		return dbCell{text: "x'" + hex.EncodeToString(x) + "'"}
	case string:
		if t, ok := looksLikeTime(x); ok {
			if !t.Before(d.wallFrom.Add(-24*time.Hour)) && !t.After(d.wallTo.Add(24*time.Hour)) {
				d.st.fail("%s holds the wall clock (%q): declare it in the table's times, and in wallClockColumns", where, x)
			} else if !t.Before(callPlanStart.Add(-2*time.Hour)) && !t.After(callPlanStart.Add(2*time.Hour)) {
				d.st.fail("%s holds a time of the simulated hour (%q): declare it in the table's times", where, x)
			}
		}
		if uuidPattern.MatchString(x) {
			d.st.fail("%s holds a generated id (%q): declare it in the table's uuids", where, x)
		}
		return dbCell{text: strconv.Quote(x)}
	}
	return dbCell{text: fmt.Sprint(v)}
}

// jsonCell canonicalises a JSON text: keys sorted, simulated times as
// offsets, indented.
func (d *dumper) jsonCell(where string, v any) dbCell {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return dbCell{text: "NULL"}
		}
		d.st.fail("%s holds JSON text, but holds %T", where, v)
		return dbCell{text: fmt.Sprint(v)}
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		d.st.fail("%s should hold JSON, but holds %q (%v)", where, s, err)
		return dbCell{text: strconv.Quote(s)}
	}
	doc = d.jsonTimes(where, doc)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		d.st.fail("%s: re-encode JSON: %v", where, err)
	}
	return dbCell{text: "(below)", block: strings.TrimRight(buf.String(), "\n")}
}

// jsonTimes replaces the simulated times in a decoded JSON document by
// their offsets; a wall-clock time there is a problem.
func (d *dumper) jsonTimes(where string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = d.jsonTimes(where, e)
		}
	case []any:
		for i, e := range x {
			x[i] = d.jsonTimes(where, e)
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			t = t.UTC()
			switch {
			case !t.Before(d.simFrom) && !t.After(d.simTo):
				return offset(t)
			case !t.Before(d.wallFrom) && !t.After(d.wallTo):
				d.st.fail("the JSON in %s holds the wall clock (%q)", where, x)
				return "<wall-clock>"
			}
		}
	}
	return v
}

// table renders one table's golden file.
func (d *dumper) table(spec dbTable) (string, error) {
	cols, err := tableColumns(d.db, spec.name)
	if err != nil {
		return "", err
	}
	has := map[string]bool{}
	for _, c := range cols {
		has[c.name] = true
	}
	var declaredCols []string
	declaredCols = append(declaredCols, spec.seedOnly...)
	declaredCols = append(declaredCols, spec.json...)
	declaredCols = append(declaredCols, spec.uuids...)
	for col := range spec.times {
		declaredCols = append(declaredCols, col)
	}
	for col := range spec.unstable {
		declaredCols = append(declaredCols, col)
	}
	for _, col := range declaredCols {
		if !has[col] {
			d.st.fail("dbTables declares %s.%s, but the table has no such column", spec.name, col)
		}
	}

	// The columns shown, and the order of the rows.
	var shown []dbColumn
	var pk []dbColumn
	for _, c := range cols {
		if slices.Contains(spec.seedOnly, c.name) || spec.unstable[c.name] != "" {
			continue
		}
		shown = append(shown, c)
	}
	for _, c := range cols {
		if c.pk > 0 {
			pk = append(pk, c)
		}
	}
	sort.Slice(pk, func(i, j int) bool { return pk[i].pk < pk[j].pk })
	byKey := len(pk) > 0
	for _, c := range pk {
		if slices.Contains(spec.uuids, c.name) || spec.unstable[c.name] != "" || slices.Contains(spec.seedOnly, c.name) {
			byKey = false
		}
	}

	sel := make([]string, len(shown))
	for i, c := range shown {
		sel[i] = `+"` + c.name + `"` // an expression: no declared type, so the driver returns what is stored
	}
	query := `SELECT ` + strings.Join(sel, ", ") + ` FROM "` + spec.name + `"`
	if byKey {
		keys := make([]string, len(pk))
		for i, c := range pk {
			keys[i] = `"` + c.name + `"`
		}
		query += ` ORDER BY ` + strings.Join(keys, ", ")
	}
	rows, err := d.db.Query(query)
	if err != nil {
		return "", fmt.Errorf("%s: %w", query, err)
	}
	var out []dbRow
	for rows.Next() {
		raw := make([]any, len(shown))
		ptrs := make([]any, len(shown))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return "", err
		}
		row := dbRow{raw: raw, cells: make([]dbCell, len(shown))}
		for i, c := range shown {
			where := spec.name + "." + c.name
			switch {
			case spec.times[c.name] != 0:
				row.cells[i] = d.timeCell(spec.name, c.name, spec.times[c.name], raw[i])
			case slices.Contains(spec.json, c.name):
				row.cells[i] = d.jsonCell(where, raw[i])
			case slices.Contains(spec.uuids, c.name):
				s, _ := raw[i].(string)
				if !uuidPattern.MatchString(s) {
					d.st.fail("%s should hold a generated id, but holds %v", where, raw[i])
				}
				row.cells[i] = dbCell{uuid: s}
			default:
				row.cells[i] = d.plainCell(where, raw[i])
			}
		}
		out = append(out, row)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if !byKey {
		sortKey := func(r dbRow) string {
			var b strings.Builder
			for _, c := range r.cells {
				b.WriteString(c.text)
				b.WriteString(c.block)
				b.WriteByte(0)
			}
			return b.String()
		}
		sort.SliceStable(out, func(i, j int) bool { return sortKey(out[i]) < sortKey(out[j]) })
	}
	// Generated ids, numbered by first appearance.
	for _, r := range out {
		for i := range r.cells {
			if id := r.cells[i].uuid; id != "" {
				if d.uuids == nil {
					d.uuids = map[string]string{}
				}
				if d.uuids[id] == "" {
					d.uuids[id] = fmt.Sprintf("<uuid-%d>", len(d.uuids)+1)
				}
				r.cells[i].text = d.uuids[id]
			}
		}
	}

	var b strings.Builder
	d.header(&b, spec, shown, pk, byKey, len(out))
	if len(out) > dbFullRowsMax {
		if spec.summary == nil {
			d.st.fail("%s has %d rows, more than the %d a golden file shows in full: give it a dbSummary", spec.name, len(out), dbFullRowsMax)
		} else {
			d.summary(&b, spec, shown, out)
			return b.String(), nil
		}
	}
	for _, r := range out {
		writeRow(&b, shown, r)
	}
	return b.String(), nil
}

// writeRow writes one row: name=value pairs, JSON blocks under it.
func writeRow(b *strings.Builder, shown []dbColumn, r dbRow) {
	var blocks []string
	for i, c := range shown {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(c.name)
		b.WriteByte('=')
		b.WriteString(r.cells[i].text)
		if r.cells[i].block != "" {
			blocks = append(blocks, "    "+c.name+":\n      "+strings.ReplaceAll(r.cells[i].block, "\n", "\n      "))
		}
	}
	b.WriteByte('\n')
	for _, block := range blocks {
		b.WriteString(block)
		b.WriteByte('\n')
	}
}

// fleetDescription is the hour every golden file describes.
func fleetDescription(b *strings.Builder) {
	fmt.Fprintf(b, "# The hour:   the call-plan fleet of %d fake devices (callplan_harness_test.go), every scheduled loop,\n", len(planFleet))
	fmt.Fprintf(b, "#             one simulated hour and a minute from %s; the devices' counters, CPU,\n", callPlanStart.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(b, "#             memory and uptime move with the clock (callplan_motion_test.go)\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "# A change in this file is a change in what the pollers store.\n")
	fmt.Fprintf(b, "# Rewrite it with: go test ./internal/poller -run TestDBState -update\n")
}

func (d *dumper) header(b *strings.Builder, spec dbTable, shown, pk []dbColumn, byKey bool, n int) {
	fmt.Fprintf(b, "# DB state after the fleet's hour: table %s, %d rows.\n", spec.name, n)
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "# Written by: %s\n", spec.writers[0])
	for _, w := range spec.writers[1:] {
		fmt.Fprintf(b, "#             %s\n", w)
	}
	fleetDescription(b)
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "# Normalised (dbstate_test.go):\n")
	var timeCols []string
	for _, c := range shown {
		if spec.times[c.name] != 0 {
			timeCols = append(timeCols, c.name)
		}
	}
	for _, col := range timeCols {
		clock := spec.times[col]
		var formats []string
		for f := range d.timeFormats[spec.name+"."+col] {
			formats = append(formats, f)
		}
		sort.Strings(formats)
		stored := strings.Join(formats, ", ")
		if stored == "" {
			stored = "no value"
		}
		if clock == simClock {
			fmt.Fprintf(b, "#   %s: the simulated clock, as the offset from the start of the hour; stored as %s\n", col, stored)
		} else {
			fmt.Fprintf(b, "#   %s: %s → %s; stored as %s\n", col, clock, clock.placeholder(), stored)
		}
	}
	for _, col := range spec.uuids {
		fmt.Fprintf(b, "#   %s: a generated id → <uuid-n>, numbered by first appearance\n", col)
	}
	if len(spec.json) > 0 {
		fmt.Fprintf(b, "#   %s: JSON, keys sorted, simulated times as offsets, printed under the row\n", strings.Join(spec.json, ", "))
	}
	fmt.Fprintf(b, "#   REAL values with three decimals, strings quoted, NULL as NULL\n")
	if byKey {
		names := make([]string, len(pk))
		for i, c := range pk {
			names[i] = c.name
		}
		fmt.Fprintf(b, "#   rows ordered by the primary key (%s)\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintf(b, "#   rows ordered by all their columns as shown\n")
	}
	if len(spec.seedOnly) > 0 {
		fmt.Fprintf(b, "#   left out, as only the harness's seed writes them (checked unchanged): %s\n", strings.Join(spec.seedOnly, ", "))
	}
	var unstable []string
	for col := range spec.unstable {
		unstable = append(unstable, col)
	}
	sort.Strings(unstable)
	for _, col := range unstable {
		fmt.Fprintf(b, "#   left out: %s, an AUTOINCREMENT id in no deterministic order — %s\n", col, spec.unstable[col])
	}
	if n > dbFullRowsMax && spec.summary != nil {
		var full []string
		for _, key := range spec.summary.full {
			full = append(full, strings.Join(key, " "))
		}
		fmt.Fprintf(b, "#\n")
		fmt.Fprintf(b, "# More than %d rows, so a summary: the rows per %s; per series (%s) its rows and, for each\n",
			dbFullRowsMax, spec.summary.series[0], strings.Join(spec.summary.series, ", "))
		fmt.Fprintf(b, "# other column, the first and last time, or min / max / sum (\"+n NULL\" when some are NULL); then the\n")
		fmt.Fprintf(b, "# full rows of the series %s.\n", strings.Join(full, ", "))
	}
	fmt.Fprintf(b, "\n")
}

// summary writes the summary of a big table (dbSummary).
func (d *dumper) summary(b *strings.Builder, spec dbTable, shown []dbColumn, rows []dbRow) {
	idx := map[string]int{}
	for i, c := range shown {
		idx[c.name] = i
	}
	seriesOf := func(r dbRow) []string {
		out := make([]string, len(spec.summary.series))
		for i, col := range spec.summary.series {
			out[i] = r.cells[idx[col]].text
		}
		return out
	}

	fmt.Fprintf(b, "## Rows per %s\n\n", spec.summary.series[0])
	var devices []string
	perDevice := map[string]int{}
	for _, r := range rows {
		dev := r.cells[idx[spec.summary.series[0]]].text
		if perDevice[dev] == 0 {
			devices = append(devices, dev)
		}
		perDevice[dev]++
	}
	for _, dev := range devices {
		fmt.Fprintf(b, "%s=%s rows=%d\n", spec.summary.series[0], dev, perDevice[dev])
	}

	fmt.Fprintf(b, "\n## Per series\n\n")
	type agg struct {
		key  []string
		rows []dbRow
	}
	var groups []*agg
	byKey := map[string]*agg{}
	for _, r := range rows {
		key := seriesOf(r)
		k := strings.Join(key, "\x00")
		g := byKey[k]
		if g == nil {
			g = &agg{key: key}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	for _, g := range groups {
		var parts []string
		for i, col := range spec.summary.series {
			parts = append(parts, col+"="+g.key[i])
		}
		parts = append(parts, fmt.Sprintf("rows=%d", len(g.rows)))
		for i, c := range shown {
			if slices.Contains(spec.summary.series, c.name) {
				continue
			}
			parts = append(parts, c.name+"="+columnRange(spec, c.name, g.rows, i))
		}
		fmt.Fprintf(b, "%s\n", strings.Join(parts, " "))
	}

	for _, want := range spec.summary.full {
		quoted := make([]string, len(want))
		for i, v := range want {
			quoted[i] = strconv.Quote(v)
		}
		k := strings.Join(quoted, "\x00")
		var label []string
		for i, col := range spec.summary.series {
			label = append(label, col+"="+quoted[i])
		}
		fmt.Fprintf(b, "\n## Full rows of %s\n\n", strings.Join(label, " "))
		g := byKey[k]
		if g == nil {
			d.st.fail("%s: the series %s shown in full has no rows", spec.name, strings.Join(label, " "))
			continue
		}
		for _, r := range g.rows {
			writeRow(b, shown, r)
		}
	}
}

// columnRange is one column's range over a series: first..last for a time,
// min/max/sum for a number, the distinct values otherwise.
func columnRange(spec dbTable, col string, rows []dbRow, i int) string {
	if spec.times[col] != 0 {
		return rows[0].cells[i].text + ".." + rows[len(rows)-1].cells[i].text
	}
	var nulls int
	var ints []int64
	var floats []float64
	distinct := map[string]bool{}
	for _, r := range rows {
		switch x := r.raw[i].(type) {
		case nil:
			nulls++
		case int64:
			ints = append(ints, x)
		case float64:
			floats = append(floats, x)
		default:
			distinct[r.cells[i].text] = true
		}
	}
	var out string
	switch {
	case len(ints) > 0:
		lo, hi, sum := ints[0], ints[0], int64(0)
		for _, v := range ints {
			lo, hi, sum = min(lo, v), max(hi, v), sum+v
		}
		out = fmt.Sprintf("%d/%d/%d", lo, hi, sum)
	case len(floats) > 0:
		lo, hi, sum := floats[0], floats[0], 0.0
		for _, v := range floats {
			lo, hi, sum = math.Min(lo, v), math.Max(hi, v), sum+v
		}
		out = fmt.Sprintf("%.3f/%.3f/%.3f", lo, hi, sum)
	case len(distinct) > 0:
		out = fmt.Sprintf("%d-distinct", len(distinct))
	}
	switch {
	case nulls == len(rows):
		return "NULL"
	case nulls > 0:
		return fmt.Sprintf("%s(+%d NULL)", out, nulls)
	}
	return out
}

// index renders tables.golden: every table of the schema, what writes it
// and how many rows it has, and the wall-clock columns.
func (d *dumper) index(names []string, specs map[string]dbTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# DB state after the fleet's hour: every table of the schema.\n")
	fmt.Fprintf(&b, "#\n")
	fleetDescription(&b)
	fmt.Fprintf(&b, "\n## Written in the hour: one golden file each\n\n")
	for _, table := range names {
		if spec, ok := specs[table]; ok {
			fmt.Fprintf(&b, "%-24s %5d rows  %s\n", table, d.st.rows[table], spec.writers[0])
			for _, w := range spec.writers[1:] {
				fmt.Fprintf(&b, "%-24s %5s       %s\n", "", "", w)
			}
		}
	}
	fmt.Fprintf(&b, "\n## Filled before the hour, not written in it (checked unchanged)\n\n")
	for _, table := range names {
		if why, ok := dbSeededTables[table]; ok {
			fmt.Fprintf(&b, "%-24s %5d rows  %s\n", table, d.st.rows[table], why)
		}
	}
	fmt.Fprintf(&b, "\n## Empty after the hour\n\n")
	for _, table := range names {
		if why, ok := dbEmptyTables[table]; ok {
			fmt.Fprintf(&b, "%-24s %s\n", table, why)
		}
	}
	fmt.Fprintf(&b, "\n## SQLite's own\n\n")
	for _, table := range names {
		if why, ok := dbBookkeeping[table]; ok {
			fmt.Fprintf(&b, "%-24s %s\n", table, why)
		}
	}
	fmt.Fprintf(&b, "\n## Times from the wall clock (wallClockColumns), checked against the values: what v2 ingest\n")
	fmt.Fprintf(&b, "## semantics (an observed_at carried from the read) changes\n\n")
	var wall []string
	for col := range wallClockColumns {
		wall = append(wall, col)
	}
	sort.Strings(wall)
	for _, col := range wall {
		fmt.Fprintf(&b, "%-34s %s\n", col, wallClockColumns[col])
	}
	fmt.Fprintf(&b, "\n## Columns of the tables the hour leaves empty whose default is the wall clock (the schema's)\n\n")
	for _, table := range names {
		if _, ok := dbEmptyTables[table]; !ok {
			continue
		}
		cols, err := tableColumns(d.db, table)
		if err != nil {
			d.st.fail("db state: %s: %v", table, err)
			continue
		}
		for _, c := range cols {
			if dflt := strings.ToLower(c.dflt); strings.Contains(dflt, "current_timestamp") || strings.Contains(dflt, "'now'") {
				fmt.Fprintf(&b, "%-34s DEFAULT %s\n", table+"."+c.name, c.dflt)
			}
		}
	}
	return b.String()
}

func goldenDBStateDir() string { return filepath.Join("testdata", "dbstate") }

// The DB state of the fleet's hour: every table the loops write, compared
// with its golden file; every table of the schema accounted for; the
// device-minutes' command counts the call plans' own.
func TestDBState(t *testing.T) {
	h := fleetHourOf(t)
	// The state is only worth comparing if the hour behind it worked.
	for _, p := range h.problems {
		t.Error(p)
	}
	st := h.state
	for _, p := range st.problems {
		t.Error(p)
	}

	dir := goldenDBStateDir()
	if updateGolden() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		old, _ := filepath.Glob(filepath.Join(dir, "*.golden"))
		for _, path := range old {
			if _, ok := st.files[filepath.Base(path)]; !ok {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
		for name, body := range st.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		var names []string
		for name := range st.files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s has no golden file (%v). Write it with: go test ./internal/poller -run TestDBState -update", name, err)
				continue
			}
			if got := st.files[name]; got != string(want) {
				t.Errorf("the hour stored something else in %s than %s says.\n%s"+
					"  If the change is meant: go test ./internal/poller -run TestDBState -update, and commit the diff — it is what the change does to the database.",
					strings.TrimSuffix(name, ".golden"), path, stateDiff(got, string(want)))
			}
		}
		old, _ := filepath.Glob(filepath.Join(dir, "*.golden"))
		for _, path := range old {
			if _, ok := st.files[filepath.Base(path)]; !ok {
				t.Errorf("%s is a golden file of no table the hour writes: delete it, or rewrite the files with -update", path)
			}
		}
	}

	// Every command a device was sent is counted in its device-minute: the
	// call plan and device_minutes.nms_cmds tell the same story.
	for _, d := range h.devices {
		perMinute := map[int]int64{}
		for _, c := range h.calls[d.id] {
			perMinute[int(c.at/time.Minute)]++
		}
		got := st.nmsCmds[d.id]
		if len(got) == 0 {
			t.Errorf("%s has no device-minute rows", d.id)
			continue
		}
		var minutes []int
		for m := range got {
			minutes = append(minutes, m)
		}
		sort.Ints(minutes)
		for _, m := range minutes {
			if got[m] != perMinute[m] {
				t.Errorf("%s, minute %d: device_minutes.nms_cmds is %d, the call plan has %d commands in it", d.id, m, got[m], perMinute[m])
			}
		}
	}
}

// stateDiff says where a golden file and the state now differ: how many
// lines, and the first lines that differ — as the columns that changed when
// both are rows (name=value …), whole otherwise.
func stateDiff(got, want string) string {
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	var b strings.Builder
	var differ []int
	for i := 0; i < max(len(gl), len(wl)); i++ {
		if i >= len(gl) || i >= len(wl) || gl[i] != wl[i] {
			differ = append(differ, i)
		}
	}
	if len(differ) == 1 {
		fmt.Fprintf(&b, "  1 line differs")
	} else {
		fmt.Fprintf(&b, "  %d lines differ", len(differ))
	}
	if len(gl) != len(wl) {
		fmt.Fprintf(&b, " (%d lines in the golden file, %d now)", len(wl), len(gl))
	}
	fmt.Fprintf(&b, "; the first:\n")
	for _, i := range differ[:min(len(differ), 3)] {
		var a, z string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			z = gl[i]
		}
		if changed, row, ok := changedColumns(a, z); ok {
			fmt.Fprintf(&b, "    line %d, %s: %s\n", i+1, row, strings.Join(changed, ", "))
			continue
		}
		fmt.Fprintf(&b, "    line %d\n      golden: %s\n      now:    %s\n", i+1, a, z)
	}
	return b.String()
}

// rowColumns splits a row line into its name=value pairs, in order; ok is
// false for a line that is not a row.
func rowColumns(line string) (names, values []string, ok bool) {
	for line != "" {
		eq := strings.IndexByte(line, '=')
		if eq <= 0 || strings.ContainsAny(line[:eq], " \"") {
			return nil, nil, false
		}
		name, rest := line[:eq], line[eq+1:]
		value := rest
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return nil, nil, false
			}
			value = q
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			value = rest[:sp]
		}
		names, values = append(names, name), append(values, value)
		line = strings.TrimPrefix(rest[len(value):], " ")
	}
	return names, values, len(names) > 0
}

// changedColumns compares two row lines with the same columns: the columns
// whose value changed ("cpu_post 10 → NULL"), and the row named by its first
// two columns.
func changedColumns(golden, now string) (changed []string, row string, ok bool) {
	gn, gv, ok1 := rowColumns(golden)
	nn, nv, ok2 := rowColumns(now)
	if !ok1 || !ok2 || !slices.Equal(gn, nn) {
		return nil, "", false
	}
	var name []string
	for i := range gn {
		if gv[i] != nv[i] {
			changed = append(changed, fmt.Sprintf("%s %s → %s", gn[i], gv[i], nv[i]))
		} else if len(name) < 2 && len(changed) == 0 {
			name = append(name, gn[i]+"="+gv[i])
		}
	}
	return changed, strings.Join(name, " "), len(changed) > 0
}
