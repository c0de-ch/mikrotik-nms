package api

import (
	"database/sql"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/mailer"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Server struct {
	db       *sql.DB
	hub      *ws.Hub
	cfg      *config.Config
	pool     *routeros.Pool
	mailer   mailer.Sender
	resolver *resolver.Resolver

	// portStats is the fleet-wide port-counter collector behind the
	// /traffic/ports/*, /traffic/path, /traffic/sankey and /traffic/summary
	// endpoints. May be nil (tests); handlers then serve an empty snapshot.
	portStats portStatsSource

	// flows is the flow collector behind the /flows/* endpoints. nil = not
	// wired (tests); handlers then serve DB state only.
	flows flowSource

	// flowCache holds aggregated flow rows of recent /flows/* queries;
	// cleared by every flow admin write (see flowChanged).
	flowCache flowAggCache

	// topoMu guards topo, the role graph + lookups the traffic endpoints
	// share, rebuilt at most every trafficTopoTTL (see trafficTopology).
	topoMu sync.Mutex
	topo   *trafficTopo
}

// flowSource is the part of *flow.Collector the flow handlers read; tests
// substitute a fake. nil = collector not wired.
type flowSource interface {
	Enabled() bool
	Status() flow.Status
	FlushedThrough() time.Time
	RolledThrough() time.Time
	Reload()
}

func NewRouter(db *sql.DB, hub *ws.Hub, cfg *config.Config, pool *routeros.Pool, m mailer.Sender,
	portStats *poller.PortStatsCollector, flows *flow.Collector) http.Handler {
	s := &Server{db: db, hub: hub, cfg: cfg, pool: pool, mailer: m, resolver: resolver.New(db)}
	if portStats != nil {
		s.portStats = portStats
	}
	if flows != nil { // avoid a typed-nil interface
		s.flows = flows
	}

	r := chi.NewRouter()

	// Middleware
	// OpenTelemetry HTTP tracing + server metrics. Uses the global providers, so
	// it's a cheap no-op when OTel export is disabled. Outermost so spans wrap the
	// whole request.
	r.Use(otelhttp.NewMiddleware("http.server", otelhttp.WithSpanNameFormatter(
		func(_ string, req *http.Request) string { return req.Method + " " + req.URL.Path },
	)))
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger) // redacts the WS ?token= from access logs
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	corsOpts := cors.Options{
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}
	if len(cfg.AllowedOrigins) > 0 {
		corsOpts.AllowedOrigins = cfg.AllowedOrigins
	} else {
		// No allow-list configured: reflect any origin (backwards compatible),
		// but warn so operators know to lock browser access down in production.
		corsOpts.AllowOriginFunc = func(r *http.Request, origin string) bool { return true }
		log.Println("warning: MIKROTIK_NMS_ALLOWED_ORIGINS is not set — CORS accepts any origin; set it to restrict browser access")
	}
	r.Use(cors.Handler(corsOpts))

	// Throttle unauthenticated auth endpoints against brute force.
	authLimiter := newRateLimiter(10, time.Minute)
	// Per-username throttle on password-reset requests to curb email bombing /
	// enumeration (keyed on the submitted username, not on user existence).
	resetLimiter := newRateLimiter(3, 15*time.Minute)

	r.Route("/api/v1", func(r chi.Router) {
		// Public endpoints
		r.Get("/health", s.handleHealth)
		r.Group(func(r chi.Router) {
			r.Use(authLimiter.middleware)
			r.Post("/auth/login", s.handleLogin)
			r.Post("/auth/refresh", s.handleRefresh)
			r.Post("/auth/setup", s.handleSetup)
			// Self-service password reset (public, enumeration-safe).
			r.Post("/auth/request-reset", s.limitResetPerUser(resetLimiter, s.handleRequestReset))
			r.Post("/auth/perform-reset", s.handlePerformReset)
		})

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
			r.Get("/devices/{id}/ports", s.handleGetDevicePorts)
			r.Get("/devices/{id}/neighbors", s.handleListNeighbors)
			r.Get("/devices/{id}/addresses", s.handleDeviceAddresses)

			// Device write operations + deep discovery (admin only)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Post("/devices", s.handleCreateDevice)
				r.Put("/devices/{id}", s.handleUpdateDevice)
				r.Delete("/devices/{id}", s.handleDeleteDevice)
				r.Get("/discovery/deep", s.handleDeepScan)
			})

			// Topology
			r.Get("/topology", s.handleGetTopology)

			// Traffic
			s.mountTrafficRoutes(r)

			// Flow export (NetFlow / IPFIX / sFlow): reads for every user,
			// configuration writes admin-only.
			s.mountFlowRoutes(r)

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
			r.Post("/network-health/events/{id}/ack", s.handleAckNetworkHealthEvent)
			r.Post("/network-health/events/ack-all", s.handleAckAllNetworkHealthEvents)

			// Connectivity monitoring (ICMP probes run FROM RouterOS devices)
			r.Get("/connectivity/targets", s.handleListPingTargets)
			r.Get("/connectivity/targets/{id}/samples", s.handleGetPingSamples)
			r.Get("/connectivity/targets/{id}/traceroutes", s.handleListTracerouteRuns)
			r.Get("/connectivity/clients/{mac}/timeline", s.handleClientTimeline)
			r.Get("/connectivity/speedtests", s.handleListSpeedTests)
			r.Get("/connectivity/speedtests/{id}/samples", s.handleGetSpeedSamples)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Post("/connectivity/targets", s.handleCreatePingTarget)
				r.Put("/connectivity/targets/{id}", s.handleUpdatePingTarget)
				r.Delete("/connectivity/targets/{id}", s.handleDeletePingTarget)
				r.Post("/connectivity/targets/{id}/run", s.handleRunPingTarget)
				r.Post("/connectivity/targets/{id}/traceroute", s.handleRunTraceroute)
				r.Post("/connectivity/speedtests", s.handleCreateSpeedTest)
				r.Put("/connectivity/speedtests/{id}", s.handleUpdateSpeedTest)
				r.Delete("/connectivity/speedtests/{id}", s.handleDeleteSpeedTest)
				r.Post("/connectivity/speedtests/{id}/run", s.handleRunSpeedTest)
			})

			// VLANs (bridge VLAN table + user-editable labels)
			r.Get("/vlans", s.handleListVLANs)
			r.Get("/vlan-labels", s.handleListVLANLabels)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Put("/vlan-labels", s.handleUpdateVLANLabel)
			})

			// App settings
			r.Get("/settings", s.handleGetSettings)
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole("admin"))
				r.Put("/settings", s.handleUpdateSettings)
				r.Post("/settings/opnsense/test", s.handleTestOpnsense)
				r.Post("/settings/otel/test", s.handleTestOTel)
				r.Post("/settings/mail/test", s.handleTestMail)
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

// mountTrafficRoutes registers the traffic endpoints. The port-specific
// endpoints take device/iface as query params (RouterOS interface names can
// contain spaces and slashes), so every new route is a static path; chi
// prefers static segments over {param}, and the legacy per-interface history
// route stays last.
func (s *Server) mountTrafficRoutes(r chi.Router) {
	r.Get("/traffic/summary", s.handleGetTrafficSummary)
	r.Get("/traffic/links", s.handleGetTrafficLinks)
	r.Get("/traffic/ports/latest", s.handleTrafficPortsLatest)
	r.Get("/traffic/ports/top", s.handleTrafficPortsTop)
	r.Get("/traffic/ports/roles", s.handleTrafficPortRoles)
	r.Get("/traffic/ports/history", s.handleTrafficPortHistory)
	r.Get("/traffic/ports/behind", s.handleTrafficPortBehind)
	r.Get("/traffic/path", s.handleTrafficPath)
	r.Get("/traffic/sankey", s.handleTrafficSankey)
	r.Get("/traffic/{deviceId}/{iface}", s.handleGetTraffic) // unchanged, keep last
}
