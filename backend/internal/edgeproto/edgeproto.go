// Package edgeproto is the boundary between the two halves of a poller
// (design 3.5). The collect half learns what to read from a ConfigSource,
// reads its devices and reports what it saw as Observations to a Sink; the
// apply half, internal/ingest, writes them to the database.
//
// In the monolith the Sink applies each Emit through ingest in one
// transaction, with the write semantics the pollers had before the split.
// From M2 the edge's Sink writes them to its outbox instead and central
// applies the records it receives — the collect halves do not change.
//
// The package is shared by both sides, so it imports nothing but the
// standard library and internal/devclass (internal/fences). That is why the
// third seam, the device-call seam, is not here: it hands out a
// routeros.Runner, and central must not link the device package. It is
// routeros.DeviceExec.
//
// Placeholders until the pieces exist that fill them: Observation.OwnerGen
// is 0 until ownership tenure (MIKROTIK_NMS_TENURE), Observation.UID is ""
// until the edge's outbox stamps every record with its UUIDv7 (M2). Nothing
// reads either yet.
package edgeproto

import (
	"context"
	"database/sql"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
)

// Kind names what an Observation reports. Each kind has one body type and
// exactly one apply function in internal/ingest.
type Kind string

// Kinds lists every kind there is.
func Kinds() []Kind {
	return []Kind{KindClientSeen}
}

// Observation is one thing a collect half saw.
type Observation struct {
	Kind Kind
	// Subject is what the observation is about, as its kind defines it.
	Subject string
	// OwnerGen is the generation of the reading process's ownership of the
	// subject's device; 0 until tenure exists.
	OwnerGen int64
	// ObservedAt is when the collect half saw it, on its own clock. The
	// apply functions do not read it yet: the rows keep the timestamps they
	// had before the split (CURRENT_TIMESTAMP where the statement sets one).
	ObservedAt time.Time
	// UID identifies the record so that applying it twice is harmless; ""
	// until the edge's outbox stamps one.
	UID string
	// Body is the kind's body type, by value.
	Body any
}

// StateTx is a transaction the observations of an Emit are written in
// together with a collect half's own state (a detector's cursor, a
// checkpoint): both land, or neither does. In the monolith it is a
// transaction on the database, on the edge one on its local store (M2).
// *sql.Tx satisfies it, and its methods are those of queries.DBTX, so the
// apply functions write into it as they are.
type StateTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Sink is where a collect half reports what it saw.
type Sink interface {
	// Emit makes obs durable: all of them, or — when it returns an error —
	// none. With a nil tx the sink commits them itself. With a collect
	// half's tx they are written into it, and the collect half commits or
	// rolls back its own writes and them together; Emit never ends a tx it
	// is given, and after an error that tx must be rolled back.
	Emit(ctx context.Context, tx StateTx, obs ...Observation) error
	// Live hands over an observation that matters only now (a port snapshot,
	// a live sample): best effort, nothing is stored and nothing waits.
	Live(o Observation)
}

// ConfigSource is what a collect half reads and how often. In the monolith
// it is the device table, the app settings and the per-class cadence, read at
// each call; on the edge the configuration snapshot central sends (M2), which
// adds the design's Revision and Changed.
type ConfigSource interface {
	// Devices lists the devices to read, as they stand at the call. An error
	// means the list could not be read, not that there are none: the caller
	// keeps what it knows of every device and asks again later.
	Devices() ([]DeviceSpec, error)
	// Setting is the value of an app setting, "" when it is not set or
	// cannot be read; the caller applies its default and its bounds.
	Setting(key string) string
}

// DeviceSpec is one device as a ConfigSource describes it at the call.
type DeviceSpec struct {
	ID string
	// Name is what rows written from the device's reads call it: its
	// identity, else its address.
	Name string
	// Online is the liveness verdict (devices.status): only a device that
	// answers is read. It is not configuration — on the edge it will come
	// from the edge's own liveness.
	Online bool
	// Paused: an admin pause of the device's polling is in force; nothing
	// but liveness runs.
	Paused bool
	// Cadence is the interval each tier of the device's plan runs at — the
	// class cadence, floored by the fleet setting — paused or not: a
	// staleness rule needs the cadence the rows were written at. A tier
	// that is not in the plan is absent.
	Cadence map[devclass.Tier]time.Duration
}

// RunInterval is the interval a loop schedules tier at on the device: its
// cadence, or 0 — the tier does not run — while the device is paused or when
// the tier is not in its plan.
func (d DeviceSpec) RunInterval(tier devclass.Tier) time.Duration {
	if d.Paused {
		return 0
	}
	return d.Cadence[tier]
}
