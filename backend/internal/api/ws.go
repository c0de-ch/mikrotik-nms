package api

import (
	"log"
	"net/http"

	"github.com/mikrotik-nms/backend/internal/ws"
	"nhooyr.io/websocket"
)

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}

	client := ws.NewClient(s.hub, conn)
	s.hub.Register(client)

	go client.WritePump(r.Context())
	client.ReadPump(r.Context())
}
