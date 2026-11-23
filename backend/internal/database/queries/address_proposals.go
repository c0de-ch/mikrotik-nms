package queries

import (
	"database/sql"
	"time"
)

// Address-proposal statuses. A proposal starts pending; an admin approves it
// (verified and committed → approved, verification refused → failed) or
// dismisses it; the auto-follow reconcile supersedes one whose device address
// changed meanwhile and expires one that has not been sighted for a day.
const (
	ProposalPending    = "pending"
	ProposalApproved   = "approved"
	ProposalDismissed  = "dismissed"
	ProposalSuperseded = "superseded"
	ProposalExpired    = "expired"
	ProposalFailed     = "failed"
)

// AddressProposal is one device_address_proposals row: a managed device seen
// by neighbor discovery at an address other than the one the NMS polls.
type AddressProposal struct {
	ID             int64      `json:"id"`
	DeviceID       string     `json:"device_id"`
	DeviceIdentity string     `json:"device_identity"`
	OldAddress     string     `json:"old_address"`
	NewAddress     string     `json:"new_address"`
	MAC            string     `json:"mac"`
	FirstSeen      time.Time  `json:"first_seen"`
	LastSeen       time.Time  `json:"last_seen"`
	Status         string     `json:"status"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	Result         string     `json:"result,omitempty"`
}

// UpsertAddressProposal records a sighting of deviceID at newAddr. A pending
// proposal for the same (device, new address) has its last_seen bumped (and
// its old address / MAC refreshed); otherwise a new pending row is inserted —
// created reports that, so the caller announces a proposal once, not every
// cycle. A new proposal supersedes the device's other pending ones: the
// newest sighting is the only move worth approving.
func UpsertAddressProposal(db *sql.DB, deviceID, oldAddr, newAddr, mac string, now time.Time) (created bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	ts := now.Unix()
	res, err := tx.Exec(
		`UPDATE device_address_proposals
		    SET last_seen = ?, old_address = ?, mac = ?
		  WHERE device_id = ? AND new_address = ? AND status = 'pending'`,
		ts, oldAddr, mac, deviceID, newAddr)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, tx.Commit()
	}
	if _, err := tx.Exec(
		`UPDATE device_address_proposals
		    SET status = 'superseded', decided_at = ?, result = 'a newer sighting proposes ' || ?
		  WHERE device_id = ? AND status = 'pending'`,
		ts, newAddr, deviceID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO device_address_proposals
		   (device_id, old_address, new_address, mac, first_seen, last_seen, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending')`,
		deviceID, oldAddr, newAddr, mac, ts, ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SettleAddressProposals closes the pending proposals nobody needs to decide
// any more: those whose device no longer polls old_address (an edit or an
// approved move changed it) become superseded, and those not sighted since
// now - ttl expire. It returns how many rows it closed.
func SettleAddressProposals(db *sql.DB, now time.Time, ttl time.Duration) (int64, error) {
	ts := now.Unix()
	res, err := db.Exec(
		`UPDATE device_address_proposals
		    SET status = 'superseded', decided_at = ?, result = 'the device address changed'
		  WHERE status = 'pending'
		    AND old_address <> COALESCE((SELECT address FROM devices d WHERE d.id = device_address_proposals.device_id), '')`,
		ts)
	if err != nil {
		return 0, err
	}
	superseded, _ := res.RowsAffected()
	res, err = db.Exec(
		`UPDATE device_address_proposals
		    SET status = 'expired', decided_at = ?, result = 'not sighted again'
		  WHERE status = 'pending' AND last_seen < ?`,
		ts, now.Add(-ttl).Unix())
	if err != nil {
		return superseded, err
	}
	expired, _ := res.RowsAffected()
	return superseded + expired, nil
}

const addressProposalCols = `p.id, p.device_id, COALESCE(d.identity, ''), p.old_address, p.new_address, p.mac,
	p.first_seen, p.last_seen, p.status, COALESCE(p.decided_by, ''), p.decided_at, p.result`

func scanAddressProposal(sc interface{ Scan(...any) error }) (AddressProposal, error) {
	var p AddressProposal
	var first, last int64
	var decided sql.NullInt64
	if err := sc.Scan(&p.ID, &p.DeviceID, &p.DeviceIdentity, &p.OldAddress, &p.NewAddress, &p.MAC,
		&first, &last, &p.Status, &p.DecidedBy, &decided, &p.Result); err != nil {
		return p, err
	}
	p.FirstSeen = time.Unix(first, 0).UTC()
	p.LastSeen = time.Unix(last, 0).UTC()
	if decided.Valid {
		t := time.Unix(decided.Int64, 0).UTC()
		p.DecidedAt = &t
	}
	return p, nil
}

// ListAddressProposals returns proposals, newest sighting first. status ""
// lists every status; limit is clamped to 1..1000 (default 200).
func ListAddressProposals(db *sql.DB, status string, limit int) ([]AddressProposal, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + addressProposalCols + `
	        FROM device_address_proposals p LEFT JOIN devices d ON d.id = p.device_id`
	args := []any{}
	if status != "" {
		q += ` WHERE p.status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY p.last_seen DESC, p.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AddressProposal, 0, 8)
	for rows.Next() {
		p, err := scanAddressProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetAddressProposal returns one proposal (sql.ErrNoRows when absent).
func GetAddressProposal(db *sql.DB, id int64) (*AddressProposal, error) {
	p, err := scanAddressProposal(db.QueryRow(
		`SELECT `+addressProposalCols+`
		   FROM device_address_proposals p LEFT JOIN devices d ON d.id = p.device_id
		  WHERE p.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DecideAddressProposal closes a pending proposal with status, recording who
// decided, when and the outcome. It reports false when the proposal was no
// longer pending (someone else decided it, or the reconcile settled it).
func DecideAddressProposal(db *sql.DB, id int64, status, decidedBy, result string, now time.Time) (bool, error) {
	res, err := db.Exec(
		`UPDATE device_address_proposals
		    SET status = ?, decided_by = ?, decided_at = ?, result = ?
		  WHERE id = ? AND status = 'pending'`,
		status, decidedBy, now.Unix(), result, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
