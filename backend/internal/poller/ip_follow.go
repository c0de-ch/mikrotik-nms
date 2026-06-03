package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Auto-follow IP changes.
//
// When a managed device changes its IP, the poller keeps dialing the old static
// devices.address and the device looks offline. MikroTik neighbor discovery
// (MNDP/CDP) still surfaces the device at its new IP — keyed by the same
// interface MAC we already cache. This reconciler matches a discovered neighbor
// MAC back to a managed device, and — only after an authenticated, identity-
// confirmed dial to the new IP — re-points devices.address.
//
// It is OPT-IN (app_setting "auto_follow_ip", default OFF) and fail-closed:
// any doubt (ambiguous MAC, conflicting sightings, dial/identity mismatch,
// missing credentials) leaves the address untouched and records a rejected
// audit event. MNDP is unauthenticated and forgeable, so a discovery packet is
// never trusted as proof — only a live, credentialed session to the new IP
// that reports the same board (and version/serial when known) commits a move.

// defaultAutoFollowInterval is used to size the "recent sighting" window when
// the configured topology interval is unavailable (e.g. in tests).
const defaultAutoFollowInterval = 60 * time.Second

// addressMove is one verified-pending re-point. It is the output of the pure
// matcher (planMoves) and carries no I/O — the unit-test seam.
type addressMove struct {
	DeviceID string
	OldAddr  string
	NewAddr  string
	MAC      string
}

// autoFollowEnabled reports whether the opt-in feature flag is on. It is read
// once per topology cycle so a Settings toggle applies on the next tick without
// a restart. A missing row, any error, or any value other than "true"/"1"
// (whitespace-trimmed) means OFF — mirrors the port-monitor boolean parsing.
func autoFollowEnabled(db *sql.DB) bool {
	v, err := queries.GetSetting(db, "auto_follow_ip")
	if err != nil {
		return false
	}
	switch strings.TrimSpace(v) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// buildDeviceMACIndex maps every managed device's interface MACs (uppercased)
// back to its device ID. A MAC owned by two different managed devices is flagged
// ambiguous and excluded from matching, so it can never drive a move.
// addrToDeviceID maps each device's current address for the UNIQUE / IP-swap
// guard.
func buildDeviceMACIndex(db *sql.DB, devices []queries.Device) (macToDeviceID map[string]string, ambiguous map[string]bool, addrToDeviceID map[string]string) {
	macToDeviceID = make(map[string]string)
	ambiguous = make(map[string]bool)
	addrToDeviceID = make(map[string]string)

	for _, dev := range devices {
		if dev.Address != "" {
			addrToDeviceID[dev.Address] = dev.ID
		}
		ifaces, err := queries.ListInterfacesByDevice(db, dev.ID)
		if err != nil {
			log.Printf("poller: auto-follow: list interfaces for %s: %v", dev.Identity, err)
			continue
		}
		for _, iface := range ifaces {
			mac := strings.ToUpper(strings.TrimSpace(iface.MACAddress))
			if mac == "" || mac == "00:00:00:00:00:00" {
				continue
			}
			if owner, ok := macToDeviceID[mac]; ok {
				if owner != dev.ID {
					ambiguous[mac] = true
				}
				continue
			}
			macToDeviceID[mac] = dev.ID
		}
	}
	return macToDeviceID, ambiguous, addrToDeviceID
}

// planMoves is the pure, DB/network-free core. Given the MAC index and the
// freshly-upserted neighbor rows, it returns at most one proposed address move
// per device. It applies: MAC normalization, staleness filtering, idempotency,
// per-device collapse of duplicate sightings, multi-IP conflict rejection, and
// the target-IP-already-managed (UNIQUE) guard. Deterministic and fully
// unit-testable.
func planMoves(devices []queries.Device, macToDeviceID map[string]string, ambiguous map[string]bool, addrToDeviceID map[string]string, neighbors []queries.Neighbor, recentCutoff time.Time) []addressMove {
	deviceByID := make(map[string]queries.Device, len(devices))
	for _, d := range devices {
		deviceByID[d.ID] = d
	}

	// Collect the set of distinct proposed new addresses per device.
	proposed := make(map[string]map[string]bool)
	for _, n := range neighbors {
		if n.NeighborMAC == "" || n.NeighborAddress == "" {
			continue
		}
		if n.LastSeen.Before(recentCutoff) {
			continue // stale sighting — ignore
		}
		mac := strings.ToUpper(strings.TrimSpace(n.NeighborMAC))
		if mac == "" || ambiguous[mac] {
			continue
		}
		deviceID, ok := macToDeviceID[mac]
		if !ok {
			continue // unmanaged neighbor
		}
		dev, ok := deviceByID[deviceID]
		if !ok {
			continue
		}
		if n.NeighborAddress == dev.Address {
			continue // idempotent — already correct
		}
		set := proposed[deviceID]
		if set == nil {
			set = make(map[string]bool)
			proposed[deviceID] = set
		}
		set[n.NeighborAddress] = true
	}

	// Deterministic output: iterate devices in their input order.
	var moves []addressMove
	for _, dev := range devices {
		set := proposed[dev.ID]
		if len(set) != 1 {
			if len(set) > 1 {
				log.Printf("poller: auto-follow: ambiguous new address for %s: %v — skipping", dev.Identity, keysOf(set))
			}
			continue // 0 = nothing to do, >1 = conflicting sightings (anti-flap)
		}
		newAddr := onlyKey(set)
		if owner, ok := addrToDeviceID[newAddr]; ok && owner != dev.ID {
			// Target IP already owned by another managed device — skip to avoid
			// a UNIQUE violation / accidental two-device IP swap.
			log.Printf("poller: auto-follow: new address %s for %s already owned by another device — skipping", newAddr, dev.Identity)
			continue
		}
		// Recover the matching MAC for the audit trail.
		mac := ""
		for m, id := range macToDeviceID {
			if id == dev.ID {
				mac = m
				break
			}
		}
		moves = append(moves, addressMove{DeviceID: dev.ID, OldAddr: dev.Address, NewAddr: newAddr, MAC: mac})
	}
	return moves
}

// verifyIdentity is the pure anti-spoof comparator. It confirms the host that
// answered the authenticated dial at the new IP is really the same device:
// board must be present and match (case-insensitive), and the RouterOS version
// must match when both sides report one. Identity/hostname is intentionally NOT
// required so an admin rename never blocks a legitimate move. (devices has no
// stored serial, so serial is not compared; the credentialed dial plus a board
// match is the anchor.)
func verifyIdentity(dev queries.Device, res *routeros.SystemResource) bool {
	if res == nil {
		return false
	}
	liveBoard := strings.TrimSpace(res.Board)
	storedBoard := strings.TrimSpace(dev.Board)
	if liveBoard == "" || storedBoard == "" {
		return false // cannot anchor without a board on both sides
	}
	if !strings.EqualFold(liveBoard, storedBoard) {
		return false
	}
	if v := strings.TrimSpace(res.Version); v != "" && strings.TrimSpace(dev.ROSVersion) != "" {
		if !strings.EqualFold(v, strings.TrimSpace(dev.ROSVersion)) {
			return false
		}
	}
	return true
}

// reconcileAddresses is the I/O orchestrator, called from pollTopology after
// neighbors have been upserted and before the topology graph is rebuilt. It
// gates on the feature flag, plans moves purely, then verifies+commits each one
// with a per-candidate stagger. Its own recover ensures a reconcile panic can
// never abort the topology rebuild.
func (m *Manager) reconcileAddresses(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: auto-follow: panic: %v\n%s", r, debug.Stack())
		}
	}()

	if !autoFollowEnabled(m.db) {
		return
	}

	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller: auto-follow: list devices: %v", err)
		return
	}
	neighbors, err := queries.ListAllNeighbors(m.db)
	if err != nil {
		log.Printf("poller: auto-follow: list neighbors: %v", err)
		return
	}

	macToDeviceID, ambiguous, addrToDeviceID := buildDeviceMACIndex(m.db, devices)

	interval := m.cfg.TopologyInterval
	if interval <= 0 {
		interval = defaultAutoFollowInterval
	}
	recentCutoff := time.Now().Add(-2 * interval)

	moves := planMoves(devices, macToDeviceID, ambiguous, addrToDeviceID, neighbors, recentCutoff)
	for _, mv := range moves {
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.verifyAndCommitMove(mv)
		time.Sleep(time.Second) // respect the per-device topology stagger
	}
}

// verifyAndCommitMove performs the authenticated anti-spoof check for a single
// move and, only on success, commits the new address. Every rejection is
// recorded as a loop_events audit row and the address is left untouched.
func (m *Manager) verifyAndCommitMove(mv addressMove) {
	dev, err := queries.GetDevice(m.db, mv.DeviceID)
	if err != nil {
		log.Printf("poller: auto-follow: get device %s: %v", mv.DeviceID, err)
		return
	}
	// A manual/concurrent edit changed the address since we planned — human wins.
	if dev.Address != mv.OldAddr {
		return
	}
	if dev.PasswordEnc == "" {
		m.recordIPRejection(dev.ID, mv, "no usable credentials (encryption key unset or decrypt failed)")
		log.Printf("poller: auto-follow: no usable credentials for %s — skipping", dev.Identity)
		return
	}

	// Authenticated dial to the NEW ip under a separate pool key so it never
	// clobbers the live connection to the old ip. A forged MNDP packet cannot
	// answer the real device's authenticated API.
	verifyKey := mv.DeviceID + ":verify"
	client, err := m.pool.Dial(verifyKey, mv.NewAddr, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS)
	if err != nil {
		m.recordIPRejection(dev.ID, mv, fmt.Sprintf("verify dial failed: %v", err))
		log.Printf("poller: auto-follow: verify dial to %s for %s failed: %v", mv.NewAddr, dev.Identity, err)
		return
	}
	defer m.pool.Close(verifyKey)

	res, err := routeros.GetSystemResource(client)
	if err != nil {
		m.recordIPRejection(dev.ID, mv, fmt.Sprintf("read system resource failed: %v", err))
		log.Printf("poller: auto-follow: read resource from %s for %s failed: %v", mv.NewAddr, dev.Identity, err)
		return
	}

	if !verifyIdentity(*dev, res) {
		m.recordIPRejection(dev.ID, mv, fmt.Sprintf("identity mismatch (stored board=%q version=%q; live board=%q version=%q)",
			dev.Board, dev.ROSVersion, res.Board, res.Version))
		log.Printf("poller: auto-follow: identity mismatch for %s at %s (board %q vs %q) — skipping",
			dev.Identity, mv.NewAddr, dev.Board, res.Board)
		return
	}

	// Commit: address only; leave identity/credentials/ports/tags/notes intact.
	dev.Address = mv.NewAddr
	if err := queries.UpdateDevice(m.db, dev); err != nil {
		log.Printf("poller: auto-follow: update device %s -> %s: %v", dev.Identity, mv.NewAddr, err)
		return
	}
	// Drop the stale live connection to the OLD ip so health/info redial the new one.
	m.pool.Close(mv.DeviceID)

	_, _ = queries.InsertLoopEvent(m.db, &queries.LoopEvent{
		DeviceID:   dev.ID,
		EventType:  "ip_address_changed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    fmt.Sprintf("%s → %s (verified board=%s)", mv.OldAddr, mv.NewAddr, res.Board),
	})
	m.hub.Publish("network.health.event", map[string]any{
		"device_id":  dev.ID,
		"event_type": "ip_address_changed",
		"severity":   "warn",
		"message":    fmt.Sprintf("%s → %s", mv.OldAddr, mv.NewAddr),
	})
	log.Printf("poller: auto-follow: %s %s → %s [verified board=%s]", dev.Identity, mv.OldAddr, mv.NewAddr, res.Board)
}

// recordIPRejection writes a fail-closed audit row for a move we refused to
// commit, so an admin can see why a discovered IP change was not applied.
func (m *Manager) recordIPRejection(deviceID string, mv addressMove, reason string) {
	_, _ = queries.InsertLoopEvent(m.db, &queries.LoopEvent{
		DeviceID:   deviceID,
		EventType:  "ip_address_changed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    fmt.Sprintf("rejected move %s → %s: %s", mv.OldAddr, mv.NewAddr, reason),
	})
}

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func onlyKey(set map[string]bool) string {
	for k := range set {
		return k
	}
	return ""
}
