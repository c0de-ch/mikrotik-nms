package flow

import (
	"database/sql"
	"log"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Retention of the flow side tables.
const (
	templateRetention     = 7 * 24 * time.Hour  // flow_templates.updated_at
	learnedIfaceRetention = 30 * 24 * time.Hour // flow_exporter_ifaces source=learned last_seen
	stalePointRetention   = 7 * 24 * time.Hour  // auto native points none of whose ifaces was seen
)

// Sweep applies flow retention; the poller's retention loop calls it every
// sweep. Settings are re-read each call: flow_1m + flow_1m_meta older than
// flow_1m_days, flow_1h + flow_1h_meta older than flow_1h_days, fact rows of
// deleted exporters, templates not refreshed for 7 days, learned interfaces
// not seen for 30 days and auto native points none of whose interfaces was
// seen for 7 days. Deletes of fact rows run in bounded chunks.
func Sweep(db *sql.DB, now time.Time) {
	logDeleted := func(what string, n int64, err error) {
		if err != nil {
			log.Printf("poller retention: %s: %v", what, err)
		} else if n > 0 {
			log.Printf("poller retention: deleted %d %s", n, what)
		}
	}

	n, err := queries.DeleteOldFlows(db, queries.Flows1m, now.AddDate(0, 0, -Retention1mDays(db)))
	logDeleted("old 1-minute flow rows", n, err)

	n, err = queries.DeleteOldFlows(db, queries.Flows1h, now.AddDate(0, 0, -Retention1hDays(db)))
	logDeleted("old hourly flow rows", n, err)

	n, err = queries.DeleteOrphanFlows(db)
	logDeleted("flow rows of deleted exporters", n, err)

	n, err = queries.DeleteOldFlowTemplates(db, now.Add(-templateRetention))
	logDeleted("old flow templates", n, err)

	n, err = queries.DeleteStaleAutoFlowPoints(db, now.Add(-stalePointRetention))
	logDeleted("stale auto flow points", n, err)

	n, err = queries.DeleteOldLearnedFlowIfaces(db, now.Add(-learnedIfaceRetention))
	logDeleted("stale learned flow interfaces", n, err)
}
