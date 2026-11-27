package routeros

import "context"

// DeviceExec is how a collect half reaches a device: the one choke point of
// device I/O (design 3.5). It hands out the device's link — the Runner every
// read helper takes — for one turn of reads. Today that is the device's
// pooled session (PoolExec); the per-device scheduler of M1 (devsched)
// implements it next, its per-device handle being the Runner.
//
// It is declared here, beside Runner, and not in internal/edgeproto with the
// other two seams: edgeproto is shared with central, and a seam that names
// Runner would link the device package into it.
type DeviceExec interface {
	// Device returns the link one turn of reads of deviceID runs on, or nil
	// when the device has no session now — not dialled yet, or a redial held
	// back by the pool's backoff: nothing was sent, and the caller tries
	// again later. Every read of the turn goes to the session the turn
	// started with. ctx is the turn's: today the pool's own deadlines bound
	// each command, devsched will bound its requests by it.
	Device(ctx context.Context, deviceID string) Runner
}

// PoolExec is DeviceExec on the connection pool: deviceID's pooled session
// (Pool.GetLive, which redials a device whose session ended, within the
// backoff) through ClientRunner — the device link the pollers have always
// had.
type PoolExec struct {
	Pool *Pool
}

// Device returns deviceID's pooled session, or nil while the pool has none.
func (e PoolExec) Device(_ context.Context, deviceID string) Runner {
	if e.Pool == nil {
		return nil
	}
	if client := e.Pool.GetLive(deviceID); client != nil {
		return ClientRunner{Client: client}
	}
	return nil
}
