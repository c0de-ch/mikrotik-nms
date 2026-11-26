package queries

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
)

type Device struct {
	ID              string     `json:"id"`
	Address         string     `json:"address"`
	Identity        string     `json:"identity"`
	Platform        string     `json:"platform"`
	Board           string     `json:"board"`
	ROSVersion      string     `json:"ros_version"`
	FirmwareVersion string     `json:"firmware_version"`
	Architecture    string     `json:"architecture"`
	Username        string     `json:"username"`
	PasswordEnc     string     `json:"-"`
	UseTLS          bool       `json:"use_tls"`
	APIPort         int        `json:"api_port"`
	Status          string     `json:"status"`
	CPULoad         *int       `json:"cpu_load"`
	MemoryUsed      *int64     `json:"memory_used"`
	MemoryTotal     *int64     `json:"memory_total"`
	Uptime          *string    `json:"uptime"`
	LastSeen        *time.Time `json:"last_seen"`
	LastError       *string    `json:"last_error"`
	Tags            string     `json:"tags"`
	Notes           string     `json:"notes"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	// CPU class and polling controls (internal/devclass). DeviceClass is
	// what the last /system/resource probe derived ("" = never probed);
	// ClassOverride is an admin's gentler-only override; the JSON also
	// carries the computed effective_class (see MarshalJSON).
	DeviceClass        string     `json:"device_class"`
	ClassOverride      string     `json:"class_override"`
	ClassProbedAt      *time.Time `json:"class_probed_at"`
	PollProfile        string     `json:"poll_profile"` // "auto" | "light"
	PollingPausedUntil *time.Time `json:"polling_paused_until"`
	CPUCount           *int       `json:"cpu_count"`
	CPUMHzMax          *int       `json:"cpu_mhz_max"`

	// PinCertSHA256 is the fingerprint the device's TLS certificate must
	// have: the SHA-256 of its DER encoding, lower-case hex ("" = none — a
	// TLS device is then not logged in to at all). Not a secret.
	PinCertSHA256 string `json:"pin_cert_sha256"`

	// Polling is the live schedule the API attaches to a single device
	// (GET /devices/{id}); never stored, nil elsewhere.
	Polling *DevicePolling `json:"polling,omitempty"`

	// Role is what the stored facts say the device does in the network
	// (devclass.Role: router | weak-router | switch | ap-cpe | unknown),
	// RoleReason why, and FlowExportEligible whether flow export may be set
	// up on it. Attached by the API to GET /devices and /devices/{id}
	// (topology.DeviceRoles, flowguard.Judge); never stored.
	Role               string `json:"role,omitempty"`
	RoleReason         string `json:"role_reason,omitempty"`
	FlowExportEligible bool   `json:"flow_export_eligible,omitempty"`
}

// DevicePolling is how the pollers currently treat a device: the class and
// profile they budget it by, the pause in force, and when each scheduled
// read (devclass tier name → time) runs next. A paused device lists no next
// runs — nothing but liveness runs until the pause ends.
type DevicePolling struct {
	Class       devclass.Class       `json:"class"`
	Profile     devclass.Profile     `json:"profile"`
	PausedUntil *time.Time           `json:"paused_until"`
	NextRuns    map[string]time.Time `json:"next_runs"`
}

// deviceColumns is the SELECT list every Device scan reads, in scanDevice's
// order.
const deviceColumns = `id, address, identity, platform, board, ros_version, firmware_version, architecture,
		        username, password_enc, use_tls, api_port,
		        status, cpu_load, memory_used, memory_total, uptime, last_seen, last_error,
		        tags, notes, created_at, updated_at,
		        COALESCE(device_class, ''), COALESCE(class_override, ''), class_probed_at,
		        poll_profile, polling_paused_until, cpu_count, cpu_mhz_max,
		        COALESCE(pin_cert_sha256, '')`

func scanDevice(row rowScanner, d *Device) error {
	if err := row.Scan(
		&d.ID, &d.Address, &d.Identity, &d.Platform, &d.Board, &d.ROSVersion, &d.FirmwareVersion, &d.Architecture,
		&d.Username, &d.PasswordEnc, &d.UseTLS, &d.APIPort,
		&d.Status, &d.CPULoad, &d.MemoryUsed, &d.MemoryTotal, &d.Uptime, &d.LastSeen, &d.LastError,
		&d.Tags, &d.Notes, &d.CreatedAt, &d.UpdatedAt,
		&d.DeviceClass, &d.ClassOverride, &d.ClassProbedAt,
		&d.PollProfile, &d.PollingPausedUntil, &d.CPUCount, &d.CPUMHzMax,
		&d.PinCertSHA256,
	); err != nil {
		return err
	}
	d.PasswordEnc = decryptSecret(d.PasswordEnc)
	return nil
}

// ProbedClass is the class the last probe derived, C0 when never probed.
func (d *Device) ProbedClass() devclass.Class {
	c, ok := devclass.ParseClass(d.DeviceClass)
	if !ok {
		return devclass.C0Unknown
	}
	return c
}

// EffectiveClass is the class the pollers budget this device as: the probed
// class, or the admin override when it is gentler. Never C0 — an unprobed
// device is budgeted as C1.
func (d *Device) EffectiveClass() devclass.Class {
	c, _ := devclass.Effective(d.ProbedClass(), devclass.Class(d.ClassOverride))
	return c
}

// Profile is the device's polling profile (auto when unset or invalid).
func (d *Device) Profile() devclass.Profile {
	p, ok := devclass.ParseProfile(d.PollProfile)
	if !ok {
		return devclass.ProfileAuto
	}
	return p
}

// PollingPaused reports whether an admin pause is in force at now. Liveness
// keeps running while paused; every other poller skips the device.
func (d *Device) PollingPaused(now time.Time) bool {
	return d.PollingPausedUntil != nil && now.Before(*d.PollingPausedUntil)
}

// MarshalJSON adds the computed effective_class and polling_paused fields so
// every API response carries them without each handler recomputing.
func (d Device) MarshalJSON() ([]byte, error) {
	type plain Device // no methods: avoids recursing into MarshalJSON
	return json.Marshal(struct {
		plain
		EffectiveClass devclass.Class `json:"effective_class"`
		PollingPaused  bool           `json:"polling_paused"`
	}{plain(d), d.EffectiveClass(), d.PollingPaused(time.Now())})
}

func CreateDevice(db *sql.DB, d *Device) error {
	secret, err := encryptSecret(d.PasswordEnc)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO devices (id, address, identity, username, password_enc, use_tls, api_port,
		    platform, board, ros_version, architecture, status, tags, notes, pin_cert_sha256)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Address, d.Identity, d.Username, secret, d.UseTLS, d.APIPort,
		d.Platform, d.Board, d.ROSVersion, d.Architecture, d.Status, d.Tags, d.Notes, pinColumn(d.PinCertSHA256),
	)
	return err
}

func GetDevice(db *sql.DB, id string) (*Device, error) {
	d := &Device{}
	if err := scanDevice(db.QueryRow(`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, id), d); err != nil {
		return nil, err
	}
	return d, nil
}

func ListDevices(db *sql.DB) ([]Device, error) {
	rows, err := db.Query(`SELECT ` + deviceColumns + ` FROM devices ORDER BY identity, address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var d Device
		if err := scanDevice(rows, &d); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// SetDeviceClass caches the class a /system/resource probe derived, with the
// facts behind it. cpu_mhz_max only ever grows: cpu-frequency is the current
// (possibly scaled-down) frequency, and the class wants the nominal one.
func SetDeviceClass(db *sql.DB, id string, class devclass.Class, facts devclass.ResourceFacts, probedAt time.Time) error {
	var cpuCount, mhz any
	if facts.CPUCount > 0 {
		cpuCount = facts.CPUCount
	}
	if facts.CPUFrequencyMHz > 0 {
		mhz = facts.CPUFrequencyMHz
	}
	_, err := db.Exec(
		`UPDATE devices SET device_class=?, class_probed_at=?,
		        cpu_count=COALESCE(?, cpu_count),
		        cpu_mhz_max=MAX(COALESCE(?, 0), COALESCE(cpu_mhz_max, 0)),
		        updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		string(class), probedAt.UTC(), cpuCount, mhz, id,
	)
	return err
}

// SetDevicePollProfile stores the polling profile ("auto" or "light").
func SetDevicePollProfile(db *sql.DB, id string, profile devclass.Profile) error {
	if !profile.Valid() {
		profile = devclass.ProfileAuto
	}
	_, err := db.Exec(`UPDATE devices SET poll_profile=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, string(profile), id)
	return err
}

// SetDeviceClassOverride stores an admin class override; an empty class
// clears it. Callers validate that the override is gentler than the probed
// class (devclass.Effective); the row stores whatever it is given.
func SetDeviceClassOverride(db *sql.DB, id string, override devclass.Class) error {
	var v any
	if override != "" {
		v = string(override)
	}
	_, err := db.Exec(`UPDATE devices SET class_override=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, v, id)
	return err
}

// pausedDevicesSQL selects the ids of the devices whose polling is paused at
// the bound time (bind sqlTime(now)), for staleness rules that must not age a
// paused device's rows. The stored pause end is compared to the second: it is
// written as a time.Time (its String form, "… +0000 UTC"), which is longer than
// the bound layout and would otherwise outlast its own second; an ISO "T"
// separator compares like the space.
const pausedDevicesSQL = `SELECT id FROM devices WHERE replace(substr(polling_paused_until, 1, 19), 'T', ' ') > ?`

// PauseDevicePolling suspends every poller but liveness for the device until
// the given time.
func PauseDevicePolling(db *sql.DB, id string, until time.Time) error {
	_, err := db.Exec(`UPDATE devices SET polling_paused_until=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, until.UTC(), id)
	return err
}

// ResumeDevicePolling clears a pause.
func ResumeDevicePolling(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE devices SET polling_paused_until=NULL, updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	return err
}

// UpdateDevice saves a device's editable fields. It never touches the stored
// password: an edit that does not carry a new one must not rewrite it (the
// value in d may be a stand-in for a secret that could not be decrypted).
// Use SetDevicePassword for a new password.
func UpdateDevice(db *sql.DB, d *Device) error {
	_, err := db.Exec(
		`UPDATE devices SET address=?, identity=?, username=?, use_tls=?, api_port=?,
		        tags=?, notes=?, pin_cert_sha256=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		d.Address, d.Identity, d.Username, d.UseTLS, d.APIPort,
		d.Tags, d.Notes, pinColumn(d.PinCertSHA256), d.ID,
	)
	return err
}

// SetDevicePassword stores a new password for a device, encrypted at rest.
// An empty password is refused: clearing a credential is not an edit.
func SetDevicePassword(db *sql.DB, id, password string) error {
	if password == "" {
		return fmt.Errorf("%w: the password is empty", ErrSecretStorage)
	}
	secret, err := encryptSecret(password)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE devices SET password_enc=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, secret, id)
	return err
}

// UpdateDeviceAddressIfUnchanged atomically re-points a device's address only if
// it still equals oldAddr at write time. Returns true when a row was updated and
// false when the address changed concurrently (a manual edit wins). Avoids a
// read-modify-write race and never rewrites the encrypted credential column.
func UpdateDeviceAddressIfUnchanged(db *sql.DB, id, oldAddr, newAddr string) (bool, error) {
	res, err := db.Exec(
		`UPDATE devices SET address=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND address=?`,
		newAddr, id, oldAddr,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func UpdateDeviceHealth(db *sql.DB, id string, status string, cpuLoad *int, memUsed, memTotal *int64, uptime *string, lastError *string) error {
	_, err := db.Exec(
		`UPDATE devices SET status=?, cpu_load=?, memory_used=?, memory_total=?, uptime=?,
		        last_seen=CURRENT_TIMESTAMP, last_error=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		status, cpuLoad, memUsed, memTotal, uptime, lastError, id,
	)
	return err
}

// MarkDeviceOnline records a successful liveness check (a reachable "ping")
// without touching the cached cpu/memory/uptime/version fields. The frequent
// liveness poll only proves the device is up; the heavier info poll refreshes
// the stats on its own (longer) cycle, so this keeps the last-known values
// intact instead of blanking them every health tick.
func MarkDeviceOnline(db *sql.DB, id string) error {
	_, err := db.Exec(
		`UPDATE devices SET status='online', last_seen=CURRENT_TIMESTAMP,
		        last_error=NULL, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		id,
	)
	return err
}

// MarkDeviceUnreachable records the outcome of a failed poll WITHOUT touching
// last_seen, so last_seen keeps meaning "last successful contact". The health
// poller decides whether status should be "offline" (grace period elapsed) or
// stay "online" (still within the grace window).
func MarkDeviceUnreachable(db *sql.DB, id, status string, lastError *string) error {
	_, err := db.Exec(
		`UPDATE devices SET status=?, last_error=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		status, lastError, id,
	)
	return err
}

func UpdateDeviceInfo(db *sql.DB, id, platform, board, rosVersion, fwVersion, arch string) error {
	_, err := db.Exec(
		`UPDATE devices SET platform=?, board=?, ros_version=?, firmware_version=?, architecture=?,
		        updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		platform, board, rosVersion, fwVersion, arch, id,
	)
	return err
}

func DeleteDevice(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
