package api

import "net/http"

// handleSystemInfo reports instance-level facts about how this backend runs:
// the polling mode (MIKROTIK_NMS_POLLERS) and the subnets it may reach
// (MIKROTIK_NMS_ALLOWED_TARGET_CIDRS). Authenticated, unlike /health, because
// the allow-list names internal subnets.
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	cidrs := []string{}
	if s.cfg != nil {
		cidrs = s.cfg.AllowedTargetCIDRStrings()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id":          instanceID,
		"pollers":              s.pollersMode(),
		"allowed_target_cidrs": cidrs,
	})
}
