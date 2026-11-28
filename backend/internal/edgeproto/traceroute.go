package edgeproto

// TracerouteHop is one row of a traceroute's hop table, as
// routeros.Traceroute parses it from /tool/traceroute. The ms fields are nil
// for hops that never answered (RouterOS shows "timeout"). The json tags are
// the wire contract: traceroute_runs.hops stores the table marshalled, and
// the API serves it as is.
//
// It lives here, not in internal/routeros, because internal/database/queries
// stores it: the apply half (internal/ingest → queries) must not link the
// device package (internal/fences). routeros.TracerouteHop is an alias of it.
type TracerouteHop struct {
	Hop     int      `json:"hop"`
	Address string   `json:"address"`
	LossPct float64  `json:"loss_pct"`
	Sent    int      `json:"sent"`
	LastMs  *float64 `json:"last_ms"`
	AvgMs   *float64 `json:"avg_ms"`
	BestMs  *float64 `json:"best_ms"`
	WorstMs *float64 `json:"worst_ms"`
	Status  string   `json:"status"`
}
