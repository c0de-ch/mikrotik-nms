// Package ingest is the apply half of the pollers (design 3.5): the database
// writes moved out of the collect halves, one apply function per
// edgeproto.Kind, each writing within the transaction it is given (a
// queries.DBTX). In M1 they keep the pollers' write semantics exactly: the
// same query functions, the same statements in the same order, and the same
// timestamps — CURRENT_TIMESTAMP where a statement sets one; nothing reads
// Observation.ObservedAt yet.
//
// Sink is the monolith's edgeproto.Sink: each Emit applied in one
// transaction, all of it or none (what that asks of a collect half is in
// edgeproto.Sink; what it asks of an apply function — write before you read —
// in Sink.Emit).
//
// The package reaches no device and no network: of this module it imports
// only edgeproto and database/queries, and nothing they link reaches
// internal/routeros, a library or a socket (internal/fences).
package ingest

import (
	"fmt"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

// applyFunc writes one observation of its kind within tx.
type applyFunc func(tx queries.DBTX, o edgeproto.Observation) error

// applyFuncs holds every kind's one apply function.
var applyFuncs = map[edgeproto.Kind]applyFunc{
	edgeproto.KindClientSeen: ApplyClientSeen,
}

// Apply writes o within tx through its kind's apply function.
func Apply(tx queries.DBTX, o edgeproto.Observation) error {
	apply, ok := applyFuncs[o.Kind]
	if !ok {
		return fmt.Errorf("ingest: no apply function for kind %q", o.Kind)
	}
	return apply(tx, o)
}
