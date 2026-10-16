package poller

import (
	"database/sql"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Finding kinds the login watch records in device_findings.
const (
	FindingParallelAPIPoller = "parallel_api_poller" // an API login from an address that is not ours
	FindingLoginFailure      = "login_failure"
)

const (
	// loginSelfSettingTTL bounds how often the nms_source_addresses setting
	// is re-read (Process runs once per device per cycle).
	loginSelfSettingTTL = time.Minute
	// loginOutboundTTL bounds how often the outbound address towards a device
	// is re-derived from the routing table.
	loginOutboundTTL = 10 * time.Minute
	// loginConfirmEvery bounds the /user/active reads per (device, user,
	// address): a poller that logs in every 30 s is confirmed once an hour,
	// not every cycle.
	loginConfirmEvery = time.Hour
)

// LoginWatch turns the account lines of a device's log into findings. Its
// purpose is one question — is something other than this NMS polling the
// device over the API? — answered from the lines the shared log read already
// delivers, plus at most one /user/active print per device per cycle to
// confirm that an unexpected session is still there.
//
// "Ours" is the union of the nms_source_addresses setting (for deployments
// behind NAT or with several addresses) and the address this host would use
// to reach the device, learned from the routing table without any traffic.
type LoginWatch struct {
	db *sql.DB

	mu        sync.Mutex
	selfSet   map[netip.Addr]bool // from the setting
	selfAt    time.Time
	outbound  map[string]outboundAddr // device address → learned source address
	confirmed map[string]time.Time    // device|user|address → last /user/active confirmation

	// seams for tests
	outboundFn func(host string, port int) (netip.Addr, bool)
}

type outboundAddr struct {
	addr netip.Addr
	ok   bool
	at   time.Time
}

// NewLoginWatch returns a watch writing findings to db.
func NewLoginWatch(db *sql.DB) *LoginWatch {
	return &LoginWatch{
		db:         db,
		selfSet:    make(map[netip.Addr]bool),
		outbound:   make(map[string]outboundAddr),
		confirmed:  make(map[string]time.Time),
		outboundFn: routeros.OutboundAddress,
	}
}

// Process applies the account lines among entries (one device's new log
// lines) to the findings. firstSight suppresses the lines of a buffer we
// see for the first time — a backend restart must not report every login of
// the past weeks as happening now.
func (w *LoginWatch) Process(dev *queries.Device, run routeros.Runner, entries []routeros.LogEntry, firstSight bool, now time.Time) {
	if firstSight || len(entries) == 0 {
		return
	}
	events := routeros.LoginEventsFrom(entries)
	if len(events) == 0 {
		return
	}

	// Candidates: API logins from addresses that are not ours.
	type candidate struct {
		user, address, via string
	}
	seen := make(map[candidate]bool)
	var candidates []candidate
	for i := range events {
		ev := &events[i]
		switch ev.Action {
		case "failure":
			w.record(dev.ID, FindingLoginFailure, "info", loginDetail(ev.User, ev.Address, ev.Via))
		case "login":
			if !ev.IsAPI() || ev.Address == "" || w.isSelf(dev, ev.Address, now) {
				continue
			}
			c := candidate{ev.User, ev.Address, ev.Via}
			if !seen[c] {
				seen[c] = true
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		return
	}

	// Confirm with one /user/active read per device per cycle, and only when
	// some candidate was not confirmed within the hour; the finding is a
	// warning while the session is still open, info when it is not.
	var active []routeros.ActiveUser
	confirmed := false
	for _, c := range candidates {
		key := dev.ID + "|" + c.user + "|" + c.address
		w.mu.Lock()
		last, ok := w.confirmed[key]
		w.mu.Unlock()
		severity := "info"
		if !ok || now.Sub(last) >= loginConfirmEvery {
			if !confirmed {
				confirmed = true
				if users, err := routeros.GetActiveUsers(run); err == nil {
					active = users
				}
			}
			w.mu.Lock()
			w.confirmed[key] = now
			w.mu.Unlock()
			if hasActiveAPISession(active, c.user, c.address) {
				severity = "warn"
			}
			log.Printf("login watch: %s: API login by %q from %s via %s (%s)", dev.Identity, c.user, c.address, c.via, severity)
		}
		w.record(dev.ID, FindingParallelAPIPoller, severity, loginDetail(c.user, c.address, c.via))
	}
}

func loginDetail(user, address, via string) string {
	if address == "" {
		return "user " + user + " via " + via
	}
	return "user " + user + " from " + address + " via " + via
}

func (w *LoginWatch) record(deviceID, kind, severity, detail string) {
	if err := queries.UpsertDeviceFinding(w.db, &queries.DeviceFinding{
		DeviceID: deviceID, Kind: kind, Severity: severity, Detail: detail,
	}); err != nil {
		log.Printf("login watch: record finding: %v", err)
	}
}

// hasActiveAPISession reports whether /user/active still lists an API
// session for the user from that address.
func hasActiveAPISession(active []routeros.ActiveUser, user, address string) bool {
	want, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	want = want.Unmap()
	for _, a := range active {
		if a.Name != user || (a.Via != "api" && a.Via != "api-ssl") {
			continue
		}
		if got, err := netip.ParseAddr(a.Address); err == nil && got.Unmap() == want {
			return true
		}
	}
	return false
}

// isSelf reports whether address is one the NMS's own sessions come from.
func (w *LoginWatch) isSelf(dev *queries.Device, address string, now time.Time) bool {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	addr = addr.Unmap()

	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Sub(w.selfAt) >= loginSelfSettingTTL {
		w.selfSet = parseSelfAddresses(w.settingValue())
		w.selfAt = now
	}
	if w.selfSet[addr] {
		return true
	}
	ob, ok := w.outbound[dev.Address]
	if !ok || now.Sub(ob.at) >= loginOutboundTTL {
		ob = outboundAddr{at: now}
		ob.addr, ob.ok = w.outboundFn(dev.Address, dev.APIPort)
		w.outbound[dev.Address] = ob
	}
	return ob.ok && ob.addr == addr
}

func (w *LoginWatch) settingValue() string {
	if w.db == nil {
		return ""
	}
	v, err := queries.GetSetting(w.db, "nms_source_addresses")
	if err != nil {
		return ""
	}
	return v
}

// parseSelfAddresses parses the CSV setting; junk entries are skipped.
func parseSelfAddresses(csv string) map[netip.Addr]bool {
	out := make(map[netip.Addr]bool)
	for _, s := range strings.Split(csv, ",") {
		if a, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil {
			out[a.Unmap()] = true
		}
	}
	return out
}
