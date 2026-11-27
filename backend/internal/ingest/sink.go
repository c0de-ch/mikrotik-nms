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
	if err := applyAll(own, obs); err != nil {
		_ = own.Rollback()
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
