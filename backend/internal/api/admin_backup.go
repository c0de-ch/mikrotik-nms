package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// exportableTables lists every persisted table that the backup/restore
// endpoints can touch, in foreign-key-safe insert order (parents first).
// Anything not in this list is intentionally NOT exposed for export/import.
var exportableTables = []string{
	// Config / parents
	"users",
	"devices",
	"upgrade_jobs",
	"dns_servers",
	"app_settings",
	// Child / dependent tables
	"interfaces",
	"neighbors",
	"links",
	"firmware_status",
	"upgrade_job_devices",
	"traffic_samples",
	"wifi_history",
	"client_history",
	"mac_lookup",
	"bridge_status",
	"bridge_port_status",
	"interface_state",
	"loop_events",
}

// tableAllowed is the security gate for any caller-supplied table name.
// Returns true only if the name is on the exportableTables list.
func tableAllowed(name string) bool {
	for _, t := range exportableTables {
		if t == name {
			return true
		}
	}
	return false
}

type backupBundle struct {
	Version    int                        `json:"version"`
	ExportedAt time.Time                  `json:"exported_at"`
	Tables     map[string][]map[string]any `json:"tables"`
}

const backupBundleVersion = 1

// ---------- per-table export ----------

func (s *Server) handleExportTable(w http.ResponseWriter, r *http.Request) {
	table := chi.URLParam(r, "table")
	if !tableAllowed(table) {
		writeError(w, http.StatusBadRequest, "unknown table")
		return
	}
	rows, err := dumpTable(s.db, table)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("export %s: %v", table, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="mikrotik-nms-%s-%s.json"`,
			table, time.Now().UTC().Format("2006-01-02")))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rows)
}

// ---------- per-table import ----------

type importResponse struct {
	Inserted int64 `json:"inserted"`
	Skipped  int64 `json:"skipped"`
}

func (s *Server) handleImportTable(w http.ResponseWriter, r *http.Request) {
	table := chi.URLParam(r, "table")
	if !tableAllowed(table) {
		writeError(w, http.StatusBadRequest, "unknown table")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 200*1024*1024)) // 200MB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		writeError(w, http.StatusBadRequest, "decode JSON: "+err.Error())
		return
	}
	ins, skip, err := importTableRows(s.db, table, rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("import %s: %v", table, err))
		return
	}
	writeJSON(w, http.StatusOK, importResponse{Inserted: ins, Skipped: skip})
}

// ---------- full backup ----------

func (s *Server) handleFullBackup(w http.ResponseWriter, r *http.Request) {
	bundle := backupBundle{
		Version:    backupBundleVersion,
		ExportedAt: time.Now().UTC(),
		Tables:     make(map[string][]map[string]any, len(exportableTables)),
	}
	for _, table := range exportableTables {
		rows, err := dumpTable(s.db, table)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("export %s: %v", table, err))
			return
		}
		bundle.Tables[table] = rows
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="mikrotik-nms-backup-%s.json"`,
			time.Now().UTC().Format("2006-01-02-150405")))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bundle)
}

// ---------- full restore ----------

type restoreResponse struct {
	Tables map[string]importResponse `json:"tables"`
}

func (s *Server) handleFullRestore(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 500*1024*1024)) // 500MB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var bundle backupBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		writeError(w, http.StatusBadRequest, "decode JSON: "+err.Error())
		return
	}
	if bundle.Version != backupBundleVersion {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unsupported bundle version %d (expected %d)",
				bundle.Version, backupBundleVersion))
		return
	}

	resp := restoreResponse{Tables: make(map[string]importResponse, len(bundle.Tables))}
	// Honour the dependency order from exportableTables, not the (random)
	// map iteration order in the bundle.
	for _, table := range exportableTables {
		rows, ok := bundle.Tables[table]
		if !ok || len(rows) == 0 {
			continue
		}
		ins, skip, err := importTableRows(s.db, table, rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("restore %s: %v", table, err))
			return
		}
		resp.Tables[table] = importResponse{Inserted: ins, Skipped: skip}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- generic helpers ----------

// dumpTable reads every row of `table` and returns them as JSON-friendly maps.
// Column names come from the driver, so the result survives schema changes —
// missing columns just don't appear in newer dumps.
func dumpTable(db *sql.DB, table string) ([]map[string]any, error) {
	if !tableAllowed(table) {
		return nil, fmt.Errorf("table %q not allowed", table)
	}
	rows, err := db.Query("SELECT * FROM " + table) //nolint:gosec // table is validated against allowlist
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, 64)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			// modernc.org/sqlite returns []byte for TEXT columns sometimes;
			// normalise to string so JSON round-trips cleanly.
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// importTableRows inserts each row with INSERT OR IGNORE so duplicates on
// PRIMARY KEY / UNIQUE constraints are silently skipped. Column names come
// from each JSON object's keys, so schema-evolved backups still load.
//
// Unknown columns in the source data will cause SQLite to error on the
// INSERT — that's intentional: silently dropping fields on an older server
// would corrupt the restore.
func importTableRows(db *sql.DB, table string, rows []map[string]any) (inserted, skipped int64, err error) {
	if !tableAllowed(table) {
		return 0, 0, fmt.Errorf("table %q not allowed", table)
	}
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		cols := make([]string, 0, len(row))
		vals := make([]any, 0, len(row))
		for k, v := range row {
			cols = append(cols, k)
			vals = append(vals, v)
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(cols)), ",")
		stmt := fmt.Sprintf("INSERT OR IGNORE INTO %s(%s) VALUES(%s)", //nolint:gosec // table validated; columns come from JSON keys but if they don't match schema SQLite errors safely
			table, strings.Join(cols, ","), placeholders)
		res, e := db.Exec(stmt, vals...)
		if e != nil {
			return inserted, skipped, fmt.Errorf("row insert: %w", e)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			inserted += n
		} else {
			skipped++
		}
	}
	return inserted, skipped, nil
}
