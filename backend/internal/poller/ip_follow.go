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

// ipRejectionTTL collapses repeated identical rejections (same device + same
// proposed move + same failure category) to at most one audit row per window,
// so a device that is persistently unreachable at its stored address but keeps
// being sighted at a candidate IP that fails verification cannot flood
// loop_events. The first rejection of each (device, move, category) is always
// recorded.
const ipRejectionTTL = time.Hour

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
// per-device collapse of duplicate sightings, multi-IP conflict rejection,
// multi-homed-device suppression (current address still live on the same MAC),
// and the target-IP-already-managed (UNIQUE) guard. Deterministic and fully
// unit-testable.
func planMoves(devices []queries.Device, macToDeviceID map[string]string, ambiguous map[string]bool, addrToDeviceID map[string]string, neighbors []queries.Neighbor, recentCutoff time.Time) []addressMove {
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
		if n.LastSeen.Before(recentCutoff) {
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
	var moves []addressMove
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
		moves = append(moves, addressMove{DeviceID: dev.ID, OldAddr: dev.Address, NewAddr: newAddr, MAC: mac})
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
		m.recordIPRejection(dev.ID, "no_credentials", mv, "no usable credentials (encryption key unset or decrypt failed)")
		log.Printf("poller: auto-follow: no usable credentials for %s — skipping", dev.Identity)
		return
	}

	// Authenticated dial to the NEW ip under a separate pool key so it never
	// clobbers the live connection to the old ip. A forged MNDP packet cannot
	// answer the real device's authenticated API.
	verifyKey := mv.DeviceID + ":verify"
	client, err := m.pool.Dial(verifyKey, mv.NewAddr, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS)
	if err != nil {
		m.recordIPRejection(dev.ID, "dial_failed", mv, fmt.Sprintf("verify dial failed: %v", err))
		log.Printf("poller: auto-follow: verify dial to %s for %s failed: %v", mv.NewAddr, dev.Identity, err)
		return
	}
	defer m.pool.Close(verifyKey)

	res, err := routeros.GetSystemResource(client)
	if err != nil {
		m.recordIPRejection(dev.ID, "read_failed", mv, fmt.Sprintf("read system resource failed: %v", err))
		log.Printf("poller: auto-follow: read resource from %s for %s failed: %v", mv.NewAddr, dev.Identity, err)
		return
	}

	if !verifyIdentity(*dev, res) {
		m.recordIPRejection(dev.ID, "identity_mismatch", mv, fmt.Sprintf("identity mismatch (stored board=%q version=%q; live board=%q version=%q)",
			dev.Board, dev.ROSVersion, res.Board, res.Version))
		log.Printf("poller: auto-follow: identity mismatch for %s at %s (board %q vs %q) — skipping",
			dev.Identity, mv.NewAddr, dev.Board, res.Board)
		return
	}

	// Strongest anchor: confirm the authenticated device genuinely owns the MAC
	// we matched on. A board string ("cAP ax") is shared across every unit of a
	// model, but the burned-in interface MAC is globally unique. Best-effort —
	// a present-but-missing MAC is a hard reject; if interfaces can't be read we
	// fall back to the board/version match already verified above.
	if mv.MAC != "" {
		if ifaces, ierr := routeros.GetInterfaces(client); ierr != nil {
			log.Printf("poller: auto-follow: %s could not list interfaces at %s to confirm MAC: %v (proceeding on board/version)", dev.Identity, mv.NewAddr, ierr)
		} else if !interfacesContainMAC(ifaces, mv.MAC) {
			m.recordIPRejection(dev.ID, "mac_mismatch", mv, fmt.Sprintf("matched MAC %s not present on the device answering at %s", mv.MAC, mv.NewAddr))
			log.Printf("poller: auto-follow: MAC %s not found on device at %s for %s — skipping", mv.MAC, mv.NewAddr, dev.Identity)
			return
		}
	}

	// Commit address only, atomically, and only if it hasn't changed since we
	// planned (a manual edit wins). This avoids a read-modify-write race and
	// never rewrites the encrypted credential column.
	committed, err := queries.UpdateDeviceAddressIfUnchanged(m.db, dev.ID, mv.OldAddr, mv.NewAddr)
	if err != nil {
		m.recordIPRejection(dev.ID, "commit_failed", mv, fmt.Sprintf("commit failed: %v", err))
		log.Printf("poller: auto-follow: update device %s -> %s: %v", dev.Identity, mv.NewAddr, err)
		return
	}
	if !committed {
		log.Printf("poller: auto-follow: %s address changed concurrently — skipping commit to %s", dev.Identity, mv.NewAddr)
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
// commit, so an admin can see why a discovered IP change was not applied. The
// category is a coarse, stable failure class (e.g. "dial_failed",
// "identity_mismatch") used for dedup: identical rejections for the same
// proposed move AND category are suppressed for ipRejectionTTL so a stuck
// candidate can't flood the table — but a CHANGE in category (e.g. "nothing
// answered" escalating to "an impostor answered") still records once, preserving
// the security-relevant signal. The dedup is only updated after a successful
// insert, so a failed write doesn't silently swallow the next cycle's retry.
func (m *Manager) recordIPRejection(deviceID, category string, mv addressMove, reason string) {
	sig := ipRejectionSig(deviceID, category, mv)
	if m.ipRejectionSuppressed(sig) {
		return
	}
	if _, err := queries.InsertLoopEvent(m.db, &queries.LoopEvent{
		DeviceID:   deviceID,
		EventType:  "ip_address_changed",
		Severity:   "warn",
		MACAddress: mv.MAC,
		Message:    fmt.Sprintf("rejected move %s → %s: %s", mv.OldAddr, mv.NewAddr, reason),
	}); err != nil {
		log.Printf("poller: auto-follow: record rejection for %s: %v", deviceID, err)
		return // leave sig unmarked so the next cycle retries the audit
	}
	m.markIPRejection(sig)
}

func ipRejectionSig(deviceID, category string, mv addressMove) string {
	return deviceID + "|" + mv.OldAddr + "|" + mv.NewAddr + "|" + category
}

// ipRejectionSuppressed reports whether this exact rejection signature was
// already audited within ipRejectionTTL (read-only — does not mark).
func (m *Manager) ipRejectionSuppressed(sig string) bool {
	m.ipRejectMu.Lock()
	defer m.ipRejectMu.Unlock()
	last, ok := m.ipRejectSeen[sig]
	return ok && time.Since(last) < ipRejectionTTL
}

// markIPRejection stamps a signature as audited now and opportunistically prunes
// expired entries so the map stays bounded.
func (m *Manager) markIPRejection(sig string) {
	now := time.Now()
	m.ipRejectMu.Lock()
	defer m.ipRejectMu.Unlock()
	if m.ipRejectSeen == nil {
		m.ipRejectSeen = make(map[string]time.Time)
	}
	for k, t := range m.ipRejectSeen {
		if now.Sub(t) > ipRejectionTTL {
			delete(m.ipRejectSeen, k)
		}
	}
	m.ipRejectSeen[sig] = now
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
