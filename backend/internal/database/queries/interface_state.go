package queries

import (
	"database/sql"
	"time"
)

type InterfaceState struct {
	ID                string    `json:"id"`
	DeviceID          string    `json:"device_id"`
	InterfaceName     string    `json:"interface_name"`
	InterfaceType     string    `json:"interface_type"`
	Running           bool      `json:"running"`
	Disabled          bool      `json:"disabled"`
	Slave             bool      `json:"slave"`
	LastLinkUp        string    `json:"last_link_up"`
	LastLinkDown      string    `json:"last_link_down"`
	FlapCountWindow   int       `json:"flap_count_window"`
	LoopProtectStatus string    `json:"loop_protect_status"`
	Comment           string    `json:"comment"`
	LastPolled        time.Time `json:"last_polled"`
}

func UpsertInterfaceState(db *sql.DB, s *InterfaceState) error {
	running := 0
	if s.Running {
		running = 1
	}
	disabled := 0
	if s.Disabled {
		disabled = 1
	}
	slave := 0
	if s.Slave {
		slave = 1
	}
	_, err := db.Exec(
		`INSERT INTO interface_state (id, device_id, interface_name, interface_type,
		   running, disabled, slave, last_link_up, last_link_down, flap_count_window,
		   loop_protect_status, comment, last_polled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, interface_name) DO UPDATE SET
		   interface_type=excluded.interface_type,
		   running=excluded.running,
		   disabled=excluded.disabled,
		   slave=excluded.slave,
		   last_link_up=excluded.last_link_up,
		   last_link_down=excluded.last_link_down,
		   flap_count_window=excluded.flap_count_window,
		   loop_protect_status=excluded.loop_protect_status,
		   comment=excluded.comment,
		   last_polled=CURRENT_TIMESTAMP`,
		s.ID, s.DeviceID, s.InterfaceName, s.InterfaceType,
		running, disabled, slave, s.LastLinkUp, s.LastLinkDown, s.FlapCountWindow,
		s.LoopProtectStatus, s.Comment,
	)
	return err
}

func ListInterfaceStates(db *sql.DB) ([]InterfaceState, error) {
	rows, err := db.Query(
		`SELECT id, device_id, interface_name, interface_type, running, disabled, slave,
		   last_link_up, last_link_down, flap_count_window, loop_protect_status, comment,
		   last_polled
		 FROM interface_state ORDER BY device_id, interface_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InterfaceState
	for rows.Next() {
		var s InterfaceState
		var running, disabled, slave int
		if err := rows.Scan(&s.ID, &s.DeviceID, &s.InterfaceName, &s.InterfaceType,
			&running, &disabled, &slave, &s.LastLinkUp, &s.LastLinkDown,
			&s.FlapCountWindow, &s.LoopProtectStatus, &s.Comment, &s.LastPolled); err != nil {
			return nil, err
		}
		s.Running = running == 1
		s.Disabled = disabled == 1
		s.Slave = slave == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteStaleInterfaceStates removes rows whose last_polled is older than
// cutoff — used to drop interfaces that disappeared from the device.
func DeleteStaleInterfaceStates(db *sql.DB, cutoff time.Time) error {
	_, err := db.Exec(`DELETE FROM interface_state WHERE last_polled < ?`, cutoff)
	return err
}
