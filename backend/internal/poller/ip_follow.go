package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// Auto-follow IP changes — detect and propose.
//
// When a managed device changes its IP, the poller keeps dialing the old static
// devices.address and the device looks offline. MikroTik neighbor discovery
// (MNDP/CDP) still surfaces the device at its new IP — keyed by the same
// interface MAC we already cache. This reconciler matches a discovered neighbor
// MAC back to a managed device and records the move as a proposal
// (device_address_proposals). It never logs in at the new address itself: an
// admin approves the proposal, and only then does VerifyAndCommitAddressMove
// dial the new IP with the stored credential, confirm board/version/MAC and
// re-point devices.address.
//
// It is OPT-IN (app_setting "auto_follow_ip", default OFF) and fail-closed:
// any doubt (ambiguous MAC, conflicting sightings, multi-homed device) yields
// no proposal, and a verification that fails on approval leaves the address
// untouched and records a rejected audit event. MNDP is unauthenticated and
// forgeable, so a discovery packet is never trusted as proof — and, because
// the credential would go to whoever answers, never acted on without a human.

// defaultAutoFollowInterval is used to size the "recent sighting" window when
// a reporting device's neighbor interval is unknown (see recentSighting).
const defaultAutoFollowInterval = 60 * time.Second

// AddressMove is one proposed re-point. It is the output of the pure
// matcher (planMoves) and carries no I/O — the unit-test seam.
type AddressMove struct {
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
// per device. It applies: MAC normalization, staleness filtering (recent
// decides per row whether the sighting is recent), idempotency, per-device
// collapse of duplicate sightings, multi-IP conflict rejection, multi-homed-
// device suppression (current address still live on the same MAC), and the
// target-IP-already-managed (UNIQUE) guard. Deterministic and fully
// unit-testable.
func planMoves(devices []queries.Device, macToDeviceID map[string]string, ambiguous map[string]bool, addrToDeviceID map[string]string, neighbors []queries.Neighbor, recent func(queries.Neighbor) bool) []AddressMove {
	deviceByID := make(map[string]queries.Device, len(devices))
	for _, d := range devices {
		deviceByID[d.ID] = d
	}

	// Collect the set of distinct proposed new addresses per device, remembering
	// the exact discovered neighbor MAC that proposed each address (for the audit
	// trail — a device may have several interface MACs). Separately, track
	// devices whose CURRENT dev.Address is also being sighted on one of their
	// MACs within the same recency window — those are multi-homed (e.g. a switch
	// with L3 SVIs on multiple VLANs sharing the bridge MAC) and must NOT be
	// "moved", or auto-follow ping-pongs them every poll cycle.
	proposed := make(map[string]map[string]string) // deviceID -> newAddr -> discovered MAC
	liveOnOldAddr := make(map[string]bool)         // deviceID -> dev.Address still live
	for _, n := range neighbors {
		if n.NeighborMAC == "" || n.NeighborAddress == "" {
			continue
		}
		if !recent(n) {
			continue // stale sighting — ignore
		}
		if !isFollowableCandidate(n.NeighborAddress) {
			continue // link-local / non-routable / non-IP — never a management target
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
			liveOnOldAddr[deviceID] = true
			continue // idempotent — already correct
		}
		set := proposed[deviceID]
		if set == nil {
			set = make(map[string]string)
			proposed[deviceID] = set
		}
		set[n.NeighborAddress] = mac // last writer wins; all MACs map to this device
	}

	// Deterministic output: iterate devices in their input order.
	var moves []AddressMove
	for _, dev := range devices {
		set := proposed[dev.ID]
		if len(set) == 0 {
			continue
		}
		if liveOnOldAddr[dev.ID] {
			log.Printf("poller: auto-follow: %s multi-homed (current %s and discovered %v both live on the same MAC) — skipping",
				dev.Identity, dev.Address, keysOf(set))
			continue
		}
		if len(set) != 1 {
			log.Printf("poller: auto-follow: ambiguous new address for %s: %v — skipping", dev.Identity, keysOf(set))
			continue // >1 = conflicting sightings (anti-flap)
		}
		newAddr, mac := onlyEntry(set)
		if owner, ok := addrToDeviceID[newAddr]; ok && owner != dev.ID {
			// Target IP already owned by another managed device — skip to avoid
			// a UNIQUE violation / accidental two-device IP swap.
			log.Printf("poller: auto-follow: new address %s for %s already owned by another device — skipping", newAddr, dev.Identity)
			continue
		}
		// Reserve this target within the batch so a later device proposing the
		// same new IP is caught by the guard above rather than colliding on the
		// devices.address UNIQUE constraint at commit time.
		addrToDeviceID[newAddr] = dev.ID
		moves = append(moves, AddressMove{DeviceID: dev.ID, OldAddr: dev.Address, NewAddr: newAddr, MAC: mac})
	}
	return moves
}

// isFollowableCandidate reports whether a discovered neighbor address is a
// plausible new *management* address worth chasing. It rejects anything that is
// not a routable unicast IP literal: link-local (IPv4 169.254.0.0/16, IPv6
// fe80::/10), unspecified, loopback and multicast are never stable management
// addresses, and an IPv6 link-local additionally has no usable host:port form
// here (it needs a zone), which is what produced the "too many colons in
// address" verify-dial failures observed in production. A non-IP string (e.g. a
// hostname) is also rejected — we only ever re-point to a verifiable IP.
func isFollowableCandidate(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return false
	}
	return true
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

// proposalTTL is how long a pending address proposal stays open without a new
// sighting before the reconcile expires it.
const proposalTTL = 24 * time.Hour

// reconcileAddresses runs from pollTopology after neighbors have been
// upserted and before the topology graph is rebuilt. It plans moves purely
// and records each as a pending proposal (device_address_proposals) for an
// admin to approve — it never dials anything. Its own recover ensures a
// reconcile panic can never abort the topology rebuild.
//
// Why no automatic move: verifying a move means logging in at an address
// learned from an unauthenticated, forgeable MNDP/CDP packet, i.e. sending
// the device's stored credential to whoever claims that address. Over plain
// API (most of a fleet) the password crosses the wire to that host before
// any identity check can run, so only a human may decide to try it.
//
// TODO(P0-13 follow-up): pins exist now (devices.pin_cert_sha256) and the
// verification dial is pinned like every other: for a TLS device the
// certificate at mv.NewAddr is checked before any login, so approving such a
// move can no longer leak its credential. Moving a pinned TLS device here
// without approval is still not done — what is missing is bookkeeping, not
// proof: one attempt per new proposal rather than per topology tick, a
// handshake-only probe that does not raise tls_pin_mismatch for an address
// that merely is not the device, and an audit trail for a decision nobody
// made. Until then every move is a proposal.
func (m *Manager) reconcileAddresses(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: auto-follow: panic: %v\n%s", r, debug.Stack())
		}
	}()

	now := m.clock()
	// Settle first, whatever the gate: an edited address or a day without a
	// sighting closes a proposal even after the feature was switched off.
	if _, err := queries.SettleAddressProposals(m.db, now, proposalTTL); err != nil {
		log.Printf("poller: auto-follow: settle proposals: %v", err)
	}

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
	moves := planMoves(devices, macToDeviceID, ambiguous, addrToDeviceID, neighbors,
		m.recentSighting(devices, now))
	identity := make(map[string]string, len(devices))
	for _, d := range devices {
		identity[d.ID] = d.Identity
	}
	for _, mv := range moves {
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.proposeMove(mv, identity[mv.DeviceID], now)
	}
}

// proposeMove records one planned move as a pending proposal and announces
// it — a loop_events row and a network.health.event — only when the proposal
// is new, so a device sighted every cycle produces one event, not one per
// topology tick.
func (m *Manager) proposeMove(mv AddressMove, identity string, now time.Time) {
	created, err := queries.UpsertAddressProposal(m.db, mv.DeviceID, mv.OldAddr, mv.NewAddr, mv.MAC, now)
	if err != nil {
		log.Printf("poller: auto-follow: record proposal for %s: %v", identity, err)
		return
	}
	if !created {
		return
	}
	msg := fmt.Sprintf("seen at %s (polled at %s) — approve the move on the Devices page", mv.NewAddr, mv.OldAddr)
	_, _ = queries.InsertLoopEvent(m.db, &queries.LoopEvent{
		DeviceID:   mv.DeviceID,
		EventType:  "ip_address_proposed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    msg,
	})
	m.hub.Publish("network.health.event", map[string]any{
		"device_id":  mv.DeviceID,
		"event_type": "ip_address_proposed",
		"severity":   "warn",
		"message":    msg,
	})
	log.Printf("poller: auto-follow: %s seen at %s (polled at %s) — proposed, awaiting approval", identity, mv.NewAddr, mv.OldAddr)
}

// recentSighting is auto-follow's staleness rule: a neighbor row is a recent
// sighting when the reporting device saw it within two of its own neighbor
// reads (each device reads /ip/neighbor on its class cadence, so a fixed
// window would drop every weak switch's sightings). Strictly by the wall
// clock — a paused reporter's frozen rows are not fresh evidence of a move.
func (m *Manager) recentSighting(devices []queries.Device, now time.Time) func(queries.Neighbor) bool {
	window := make(map[string]time.Duration, len(devices))
	for _, d := range devices {
		iv := m.cadence.Interval(d, devclass.TierNeighbors, now)
		if iv <= 0 {
			iv = defaultAutoFollowInterval
		}
		window[d.ID] = 2 * iv
	}
	return func(n queries.Neighbor) bool {
		w, ok := window[n.DeviceID]
		return ok && now.Sub(n.LastSeen) <= w
	}
}

// Move verification failure categories (MoveError.Category).
const (
	MoveStale            = "stale"             // the device address changed since the move was planned
	MoveNoCredentials    = "no_credentials"    // nothing stored to log in with
	MoveDialFailed       = "dial_failed"       // nothing (or not the device's credential) answered at the new address
	MoveReadFailed       = "read_failed"       // logged in, but /system/resource could not be read
	MoveIdentityMismatch = "identity_mismatch" // board/version differ: another host answered
	MoveMACMismatch      = "mac_mismatch"      // the matched MAC is not on the device answering
	MoveCommitFailed     = "commit_failed"     // the database write failed
)

// MoveError is a refused address move: a stable category plus the reason
// shown to the admin. It unwraps to the dial error, so callers can tell the
// dial policy (polling off, target not allowed) from a failed verification.
type MoveError struct {
	Category string
	Reason   string
	Err      error
}

func (e *MoveError) Error() string { return e.Reason }
func (e *MoveError) Unwrap() error { return e.Err }

// VerifyAndCommitAddressMove performs the authenticated anti-spoof check for
// one move and, only when it passes, commits the new address. The API calls
// it when an admin approves a proposal: that approval is what allows the
// stored credential to be sent to the MNDP-learned address. It returns a
// one-line summary on success and a *MoveError otherwise; every refusal
// after the staleness check is also written to loop_events, and the address
// is left untouched.
func VerifyAndCommitAddressMove(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, mv AddressMove) (string, error) {
	dev, err := queries.GetDevice(db, mv.DeviceID)
	if err != nil {
		return "", fmt.Errorf("get device %s: %w", mv.DeviceID, err)
	}
	// A manual/concurrent edit changed the address since we planned — human wins.
	if dev.Address != mv.OldAddr {
		return "", &MoveError{Category: MoveStale,
			Reason: fmt.Sprintf("the device is now polled at %s, not %s", dev.Address, mv.OldAddr)}
	}
	reject := func(category, reason string, cause error) error {
		recordMoveRejection(db, mv, reason)
		log.Printf("poller: auto-follow: %s %s → %s refused: %s", dev.Identity, mv.OldAddr, mv.NewAddr, reason)
		return &MoveError{Category: category, Reason: reason, Err: cause}
	}
	if dev.PasswordEnc == "" {
		return "", reject(MoveNoCredentials, "no usable credentials (encryption key unset or decrypt failed)", nil)
	}

	// Authenticated dial to the NEW ip under a separate pool key so it never
	// clobbers the live connection to the old ip — and is not held back by
	// the polling key's reconnect backoff. A forged MNDP packet cannot
	// answer the real device's authenticated API; for a TLS device the
	// pinned certificate is checked before the login is sent at all, and
	// the session is counted against the device (DialTarget).
	verifyKey := mv.DeviceID + ":verify"
	target := DialTarget(*dev)
	target.Address = mv.NewAddr
	client, err := pool.Dial(verifyKey, target, dev.Username, dev.PasswordEnc)
	// Whatever the outcome, nothing of this one-off dial stays in the pool:
	// not the session, and not the address and credential it was made with.
	defer pool.Forget(verifyKey)
	if err != nil {
		return "", reject(MoveDialFailed, fmt.Sprintf("verify dial failed: %v", err), err)
	}

	res, err := routeros.GetSystemResource(client)
	if err != nil {
		return "", reject(MoveReadFailed, fmt.Sprintf("read system resource failed: %v", err), err)
	}
	if !verifyIdentity(*dev, res) {
		return "", reject(MoveIdentityMismatch, fmt.Sprintf("identity mismatch (stored board=%q version=%q; live board=%q version=%q)",
			dev.Board, dev.ROSVersion, res.Board, res.Version), nil)
	}

	// Strongest anchor: confirm the authenticated device genuinely owns the MAC
	// we matched on. A board string ("cAP ax") is shared across every unit of a
	// model, but the burned-in interface MAC is globally unique. Best-effort —
	// a present-but-missing MAC is a hard reject; if interfaces can't be read we
	// fall back to the board/version match already verified above. One
	// proplisted print (name, type, MAC) on the verify connection: no
	// snapshot covers a device answering at an address it is not known at.
	if mv.MAC != "" {
		if ifaces, ierr := routeros.GetInterfaces(routeros.ClientRunner{Client: client}); ierr != nil {
			log.Printf("poller: auto-follow: %s could not list interfaces at %s to confirm MAC: %v (proceeding on board/version)", dev.Identity, mv.NewAddr, ierr)
		} else if !interfacesContainMAC(ifaces, mv.MAC) {
			return "", reject(MoveMACMismatch, fmt.Sprintf("matched MAC %s not present on the device answering at %s", mv.MAC, mv.NewAddr), nil)
		}
	}

	// Commit address only, atomically, and only if it hasn't changed since we
	// planned (a manual edit wins). This avoids a read-modify-write race and
	// never rewrites the encrypted credential column.
	committed, err := queries.UpdateDeviceAddressIfUnchanged(db, dev.ID, mv.OldAddr, mv.NewAddr)
	if err != nil {
		return "", reject(MoveCommitFailed, fmt.Sprintf("commit failed: %v", err), err)
	}
	if !committed {
		return "", &MoveError{Category: MoveStale, Reason: "the device address changed while the move was being verified"}
	}
	// Drop the live connection to the OLD ip AND its remembered dial spec:
	// Close keeps the spec, so the next GetLive would redial the old address
	// with the stored credential — whoever holds that address now. The
	// pollers redial from the device row, which carries the new address.
	pool.Forget(mv.DeviceID)

	summary := fmt.Sprintf("%s → %s (verified board=%s)", mv.OldAddr, mv.NewAddr, res.Board)
	_, _ = queries.InsertLoopEvent(db, &queries.LoopEvent{
		DeviceID:   dev.ID,
		EventType:  "ip_address_changed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    summary,
	})
	hub.Publish("network.health.event", map[string]any{
		"device_id":  dev.ID,
		"event_type": "ip_address_changed",
		"severity":   "warn",
		"message":    fmt.Sprintf("%s → %s", mv.OldAddr, mv.NewAddr),
	})
	log.Printf("poller: auto-follow: %s %s [approved]", dev.Identity, summary)
	return summary, nil
}

// recordMoveRejection writes the audit row for a move verification refused,
// so the network-health history shows why an approved move was not applied.
// Approval is a human action, so every refusal is recorded (no dedup).
func recordMoveRejection(db *sql.DB, mv AddressMove, reason string) {
	if _, err := queries.InsertLoopEvent(db, &queries.LoopEvent{
		DeviceID:   mv.DeviceID,
		EventType:  "ip_address_changed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    fmt.Sprintf("rejected move %s → %s: %s", mv.OldAddr, mv.NewAddr, reason),
	}); err != nil {
		log.Printf("poller: auto-follow: record rejection for %s: %v", mv.DeviceID, err)
	}
}

// interfacesContainMAC reports whether any of the device's interfaces carries
// the given MAC (case-insensitive), used to confirm the authenticated device at
// the new IP genuinely owns the MAC we matched on.
func interfacesContainMAC(ifaces []routeros.InterfaceInfo, mac string) bool {
	want := strings.ToUpper(strings.TrimSpace(mac))
	if want == "" {
		return false
	}
	for _, i := range ifaces {
		if strings.ToUpper(strings.TrimSpace(i.MACAddress)) == want {
			return true
		}
	}
	return false
}

func keysOf(set map[string]string) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// onlyEntry returns the single addr->MAC entry of a len-1 map (the caller has
// already guarded len(set) == 1).
func onlyEntry(set map[string]string) (addr, mac string) {
	for a, m := range set {
		return a, m
	}
	return "", ""
}
