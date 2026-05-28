package api

import (
	"log"
	"net/http"
	"net/url"

	"github.com/mikrotik-nms/backend/internal/ws"
	"nhooyr.io/websocket"
)

// originHostPatterns converts configured origin URLs (scheme://host[:port]) into
// the host[:port] patterns nhooyr/websocket matches the Origin header against.
func originHostPatterns(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			out = append(out, u.Host)
		} else {
			out = append(out, o)
		}
	}
	return out
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if len(s.cfg.AllowedOrigins) > 0 {
		// Enforce the configured allow-list (blocks cross-site WS hijacking).
		opts.OriginPatterns = originHostPatterns(s.cfg.AllowedOrigins)
	} else {
		// No allow-list: accept any origin (backwards compatible). Lock this
		// down by setting MIKROTIK_NMS_ALLOWED_ORIGINS.
		opts.OriginPatterns = []string{"*"}
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}

	client := ws.NewClient(s.hub, conn)
	s.hub.Register(client)

	go client.WritePump(r.Context())
	client.ReadPump(r.Context())
}
