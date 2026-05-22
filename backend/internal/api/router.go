package api

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

type Server struct {
	db       *sql.DB
	hub      *ws.Hub
	cfg      *config.Config
	pool     *routeros.Pool
	resolver *resolver.Resolver
}

func NewRouter(db *sql.DB, hub *ws.Hub, cfg *config.Config, pool *routeros.Pool) http.Handler {
	s := &Server{db: db, hub: hub, cfg: cfg, pool: pool, resolver: resolver.New(db)}

	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowOriginFunc: func(r *http.Request, origin string) bool { return true },
		AllowedMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:  []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Route("/api/v1", func(r chi.Router) {
		// Public endpoints
		r.Get("/health", s.handleHealth)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/setup", s.handleSetup)

		// Authenticated endpoints
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(cfg.JWTSecret))

			r.Post("/auth/logout", s.handleLogout)
			r.Get("/auth/me", s.handleMe)

			// Users (admin only)
			r.Route("/users", func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Get("/", s.handleListUsers)
				r.Post("/", s.handleCreateUser)
				r.Delete("/{id}", s.handleDeleteUser)
			})

			// Discovery
			r.Get("/discovery", s.handleDiscoverDevices)
			r.Get("/clients", s.handleScanClients)
			r.Get("/clients/cached", s.handleCachedClients)
			r.Get("/debug/wifi", s.handleDebugWifiRaw)

			// WiFi tracking & MAC lookup
			r.Get("/wifi/current", s.handleWifiCurrent)
			r.Get("/wifi/history", s.handleWifiHistory)
			r.Get("/mac-lookup", s.handleMACLookup)

			// Devices
			r.Get("/devices", s.handleListDevices)
			r.Get("/devices/{id}", s.handleGetDevice)
			r.Get("/devices/{id}/interfaces", s.handleListInterfaces)
			r.Get("/devices/{id}/neighbors", s.handleListNeighbors)

			// Device write operations (admin only)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Post("/devices", s.handleCreateDevice)
				r.Put("/devices/{id}", s.handleUpdateDevice)
				r.Delete("/devices/{id}", s.handleDeleteDevice)
			})

			// Topology
			r.Get("/topology", s.handleGetTopology)

			// Traffic
			r.Get("/traffic/summary", s.handleGetTrafficSummary)
			r.Get("/traffic/{deviceId}/{iface}", s.handleGetTraffic)

			// Firmware
			r.Get("/firmware", s.handleListFirmware)
			r.Get("/firmware/upgrade/{jobId}", s.handleGetUpgradeJob)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Post("/firmware/check", s.handleCheckFirmware)
				r.Post("/firmware/upgrade", s.handleUpgradeFirmware)
				r.Post("/firmware/channel", s.handleSetChannel)
				r.Post("/firmware/routerboard", s.handleUpgradeRouterboard)
			})

			// NetBox export
			r.Get("/netbox/export", s.handleNetboxExport)
			r.Get("/netbox/export/{type}", s.handleNetboxExportCSV)

			// DNS
			r.Get("/dns", s.handleListDNSServers)
			r.Post("/dns/resolve", s.handleResolveDNS)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Post("/dns", s.handleCreateDNSServer)
				r.Put("/dns/{id}", s.handleUpdateDNSServer)
				r.Delete("/dns/{id}", s.handleDeleteDNSServer)
			})

			// Network health (bridge / STP / loop detection)
			r.Get("/network-health", s.handleNetworkHealth)
			r.Get("/network-health/events", s.handleNetworkHealthEvents)

			// App settings
			r.Get("/settings", s.handleGetSettings)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Put("/settings", s.handleUpdateSettings)
				r.Post("/admin/purge-history", s.handlePurgeHistory)
				r.Get("/admin/export/{table}", s.handleExportTable)
				r.Post("/admin/import/{table}", s.handleImportTable)
				r.Get("/admin/backup", s.handleFullBackup)
				r.Post("/admin/restore", s.handleFullRestore)
			})

			// WebSocket
			r.Get("/ws", s.handleWebSocket)
		})
	})

	return r
}
