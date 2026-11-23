package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// IP auto-follow proposals. The topology loop records a managed device seen
// by MNDP/CDP at a new address as a pending proposal and never logs in there
// itself; an admin approves it here — the one moment the stored credential is
// sent to the MNDP-learned address, so approval needs a fresh password login
// — or dismisses it.

// proposalMu serialises approvals and dismissals: two clicks on one proposal
// must not log in at the new address twice, and a dismissal must not close a
// proposal whose approval is still verifying.
var proposalMu sync.Mutex

func (s *Server) mountAddressProposalRoutes(r chi.Router) {
	r.Get("/devices/address-proposals", s.handleListAddressProposals)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole("admin"))
		r.Post("/devices/address-proposals/{id}/dismiss", s.handleDismissAddressProposal)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireFreshAuth(freshAuthMaxAge))
			r.Post("/devices/address-proposals/{id}/approve", s.handleApproveAddressProposal)
		})
	})
}

// handleListAddressProposals — GET /devices/address-proposals[?status=all|<status>]
// (pending only by default).
func (s *Server) handleListAddressProposals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		status = queries.ProposalPending
	case "all":
		status = ""
	case queries.ProposalPending, queries.ProposalApproved, queries.ProposalDismissed,
		queries.ProposalSuperseded, queries.ProposalExpired, queries.ProposalFailed:
	default:
		writeError(w, http.StatusBadRequest, "unknown status")
		return
	}
	ps, err := queries.ListAddressProposals(s.db, status, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list address proposals")
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// loadPendingProposal answers 404 for an unknown id and 409 for one already
// decided, and returns the pending proposal otherwise.
func (s *Server) loadPendingProposal(w http.ResponseWriter, r *http.Request) (*queries.AddressProposal, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid proposal id")
		return nil, false
	}
	p, err := queries.GetAddressProposal(s.db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get proposal")
		}
		return nil, false
	}
	if p.Status != queries.ProposalPending {
		writeError(w, http.StatusConflict, "proposal is "+p.Status)
		return nil, false
	}
	return p, true
}

func decider(r *http.Request) string {
	if u := auth.UserFromContext(r.Context()); u != nil {
		return u.Username
	}
	return ""
}

// handleApproveAddressProposal — POST /devices/address-proposals/{id}/approve.
// Logs in at the proposed address with the device's stored credential,
// checks board/version and the matched MAC, and re-points the device only
// when all of it holds. 200 with the updated proposal (status approved or
// failed — a refused verification is an answer, not a server error); 409
// when the proposal is no longer pending, the device's address changed since,
// or its polling is paused; 503/403 from the dial policy (the proposal stays
// pending).
func (s *Server) handleApproveAddressProposal(w http.ResponseWriter, r *http.Request) {
	proposalMu.Lock()
	defer proposalMu.Unlock()

	p, ok := s.loadPendingProposal(w, r)
	if !ok {
		return
	}
	dev, ok := s.loadDevice(w, p.DeviceID)
	if !ok {
		return
	}
	now := time.Now()
	if dev.Address != p.OldAddress {
		_, _ = queries.DecideAddressProposal(s.db, p.ID, queries.ProposalSuperseded, decider(r),
			"the device address changed", now)
		writeError(w, http.StatusConflict, fmt.Sprintf("the device is now polled at %s — proposal superseded", dev.Address))
		return
	}
	if s.writePollingPaused(w, p.DeviceID) {
		return
	}
	if writeDialPolicyError(w, routeros.PollingAllowed()) {
		return
	}

	mv := poller.AddressMove{DeviceID: p.DeviceID, OldAddr: p.OldAddress, NewAddr: p.NewAddress, MAC: p.MAC}
	summary, err := poller.VerifyAndCommitAddressMove(s.db, s.pool, s.hub, mv)
	if err != nil {
		if writeDialPolicyError(w, err) {
			return // environment, not a verdict: the proposal stays pending
		}
		var me *poller.MoveError
		if !errors.As(err, &me) {
			writeError(w, http.StatusInternalServerError, "failed to verify the move")
			return
		}
		if me.Category == poller.MoveStale {
			_, _ = queries.DecideAddressProposal(s.db, p.ID, queries.ProposalSuperseded, decider(r), me.Reason, now)
			writeError(w, http.StatusConflict, me.Reason)
			return
		}
		_ = queries.RecordAddressProposalOutcome(s.db, p.ID, queries.ProposalFailed, decider(r), me.Reason, now)
		s.audit(r, "address_move_refused", p.DeviceID,
			fmt.Sprintf("%s → %s: %s", p.OldAddress, p.NewAddress, me.Reason))
	} else {
		_ = queries.RecordAddressProposalOutcome(s.db, p.ID, queries.ProposalApproved, decider(r), summary, now)
		s.audit(r, "address_move_approved", p.DeviceID, summary)
	}
	s.writeProposal(w, p.ID)
}

// handleDismissAddressProposal — POST /devices/address-proposals/{id}/dismiss.
// Closes the proposal without contacting anything. The same move is not
// proposed again for queries.DismissedProposalQuiet (7 days); a failed
// approval keeps it quiet for FailedProposalQuiet (24 h).
func (s *Server) handleDismissAddressProposal(w http.ResponseWriter, r *http.Request) {
	proposalMu.Lock()
	defer proposalMu.Unlock()
	p, ok := s.loadPendingProposal(w, r)
	if !ok {
		return
	}
	done, err := queries.DecideAddressProposal(s.db, p.ID, queries.ProposalDismissed, decider(r), "dismissed", time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to dismiss proposal")
		return
	}
	if !done {
		writeError(w, http.StatusConflict, "proposal is no longer pending")
		return
	}
	s.audit(r, "address_move_dismissed", p.DeviceID, fmt.Sprintf("%s → %s", p.OldAddress, p.NewAddress))
	s.writeProposal(w, p.ID)
}

func (s *Server) writeProposal(w http.ResponseWriter, id int64) {
	p, err := queries.GetAddressProposal(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get proposal")
		return
	}
	writeJSON(w, http.StatusOK, p)
}
