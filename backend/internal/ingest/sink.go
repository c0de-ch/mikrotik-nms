package ingest

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

// Sink is the monolith's edgeproto.Sink: the collect halves of this process
// report to it, and it applies what they report through Apply. Safe for
// concurrent use.
type Sink struct {
	db *sql.DB
}

// NewSink returns the in-process sink writing to db.
func NewSink(db *sql.DB) *Sink {
	return &Sink{db: db}
}

var _ edgeproto.Sink = (*Sink)(nil)

// Emit applies obs in order. With a nil tx they go into one transaction of
// their own, committed when every one applied and rolled back at the first
// that did not — nothing of obs is then written. With the caller's tx they
// are applied in it and the caller commits or rolls back.
//
// Its own transaction begins DEFERRED: SQLite takes the write lock at the
// first write and waits out busy_timeout for it, as the pollers' single
// writes did. An apply function therefore writes before it reads — a read
// first would make the transaction a reader that fails at once with
// SQLITE_BUSY when another writer got in between. One that must read first
// needs this Emit to begin IMMEDIATE, not a workaround of its own.
//
// ctx bounds the wait for the connection; a flush at shutdown passes
// context.WithoutCancel. A panic in an apply function rolls the transaction
// back before it travels on.
func (s *Sink) Emit(ctx context.Context, tx edgeproto.StateTx, obs ...edgeproto.Observation) error {
	if len(obs) == 0 {
		return nil
	}
	if tx != nil {
		return applyAll(tx, obs)
	}
	own, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ingest: begin: %w", err)
	}
	// After a Commit this returns sql.ErrTxDone and does nothing.
	defer func() { _ = own.Rollback() }()
	if err := applyAll(own, obs); err != nil {
		return err
	}
	if err := own.Commit(); err != nil {
		return fmt.Errorf("ingest: commit: %w", err)
	}
	return nil
}

func applyAll(tx queries.DBTX, obs []edgeproto.Observation) error {
	for _, o := range obs {
		if err := Apply(tx, o); err != nil {
			return err
		}
	}
	return nil
}

// Live delivers nothing yet: no kind is live — every kind there is is stored
// through Emit. The first collect half with a live observation (the port
// snapshot) adds its delivery here.
func (s *Sink) Live(edgeproto.Observation) {}
