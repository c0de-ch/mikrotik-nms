package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// checkProbeSource answers whether deviceID may be newly chosen as the device
// a probe runs on, and writes the refusal when it may not.
//
// A probe is work done by the device's CPU: an API /ping burst every cycle, or
// a /tool/fetch download that pegs a switch for up to two minutes. Only a
// strong (C3) device — in practice a router — is offered as a source; a
// switch, a small AP and a device not classified yet are refused. What is
// already configured on a weaker device keeps running and stays editable:
// the handlers call this only when a probe is created or moved.
func (s *Server) checkProbeSource(w http.ResponseWriter, deviceID string) bool {
	d, err := queries.GetDevice(s.db, deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "device not found")
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get device")
		return false
	}
	// The class the device is budgeted as: its effective class, C1 while it
	// is unclassified or on the light profile — the rule the live-view caps
	// follow (devclass.EffectiveLiveCaps).
	class := d.EffectiveClass().Budgeted()
	if d.Profile() == devclass.ProfileLight {
		class = devclass.C1
	}
	if d.StrongProbeSource() {
		return true
	}
	const why = "pings and speed tests run only from strong devices (class C3, routers), because they cost the probing device CPU"
	name := deviceDisplayName(*d)
	switch {
	case !d.ProbedClass().Valid() || d.ProbedClass().Budgeted() != d.ProbedClass():
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%s has not been classified yet: %s. Try again after its next inventory read.", name, why))
	case d.Profile() == devclass.ProfileLight:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s is on the light polling profile: %s.", name, why))
	default:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s is polled as a class %s device: %s.", name, class, why))
	}
	return false
}
