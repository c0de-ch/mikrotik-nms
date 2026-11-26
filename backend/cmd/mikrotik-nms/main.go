package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mikrotik-nms/backend/internal/api"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/mailer"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/telemetry"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	db, err := database.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Install the at-rest cipher for device credentials.
	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("failed to init encryption: %v", err)
	}
	queries.SetCipher(cipher)
	queries.AllowPlaintextSecrets(cfg.PlaintextSecrets)
	switch {
	case cipher.Enabled():
		if n, err := queries.EncryptPlaintextDeviceSecrets(db); err != nil {
			log.Printf("warning: could not encrypt existing device secrets: %v", err)
		} else if n > 0 {
			log.Printf("encrypted %d plaintext device password(s) at rest", n)
		}
	case cfg.PlaintextSecrets:
		log.Println("warning: MIKROTIK_NMS_DEV_PLAINTEXT_SECRETS is set — device passwords are stored unencrypted (development only)")
	default:
		log.Println("note: MIKROTIK_NMS_ENCRYPTION_KEY is not set — device passwords cannot be stored on this instance")
	}
	// A wrong key must be loud: those devices are not logged in to at all
	// (no empty-password attempts) until the key or the passwords are fixed.
	if n, err := queries.CountUnreadableDeviceSecrets(db); err != nil {
		log.Printf("warning: could not check the stored device secrets: %v", err)
	} else if n > 0 {
		log.Printf("ERROR: %d device password(s) cannot be decrypted with the configured MIKROTIK_NMS_ENCRYPTION_KEY — "+
			"those devices are not polled until the key is corrected or their passwords are entered again", n)
	}
	for _, gone := range []string{"MIKROTIK_NMS_DEFAULT_ROS_USER", "MIKROTIK_NMS_DEFAULT_ROS_PASS"} {
		if os.Getenv(gone) != "" {
			log.Printf("warning: %s is no longer used (a device is added with its own username and password) — remove it from the environment", gone)
		}
	}
	if os.Getenv("MIKROTIK_NMS_ROS_TLS_VERIFY") != "" {
		log.Println("warning: MIKROTIK_NMS_ROS_TLS_VERIFY is no longer used (every API-TLS session is verified against the certificate fingerprint pinned on the device) — remove it from the environment")
	}
	// A TLS device without a pinned fingerprint is not logged in to at all:
	// say so once, loudly, rather than leave it silently unpolled.
	if n, err := queries.CountTLSDevicesWithoutPin(db); err != nil {
		log.Printf("warning: could not check the devices' TLS pins: %v", err)
	} else if n > 0 {
		log.Printf("ERROR: %d device(s) use API-TLS without a pinned certificate fingerprint — "+
			"they are not polled until the fingerprint (/certificate print detail on the device) is entered on the Devices page", n)
	}
	if len(cfg.JWTSecret) < 32 {
		log.Println("warning: MIKROTIK_NMS_JWT_SECRET is shorter than 32 characters — use a longer, random secret")
	}

	// OpenTelemetry export (Settings → Observability; env defaults overlaid with
	// app_settings). When enabled, the stdlib log is mirrored to Loki and the
	// collected metrics/traces are exported to the OTLP endpoint (a collector
	// gateway that fans out to Loki/Tempo/dashboards).
	var otelShutdown func(context.Context) error
	otelCfg := telemetry.ConfigFromSettings(db, telemetry.Config{
		Enabled:        cfg.OTelEnabled,
		Endpoint:       cfg.OTelEndpoint,
		Protocol:       cfg.OTelProtocol,
		Insecure:       cfg.OTelInsecure,
		Headers:        telemetry.ParseHeaders(cfg.OTelHeaders),
		ServiceName:    cfg.OTelServiceName,
		SampleRatio:    cfg.OTelSampleRatio,
		ExportInterval: time.Minute,
	})
	if otelCfg.Enabled && otelCfg.Endpoint != "" {
		if prov, err := telemetry.Init(context.Background(), otelCfg, db); err != nil {
			log.Printf("warning: OpenTelemetry init failed (export disabled): %v", err)
		} else {
			otelShutdown = prov.Shutdown
			log.SetOutput(telemetry.NewLogWriter(os.Stderr)) // tee app logs → Loki
			log.Printf("OpenTelemetry export enabled → %s (OTLP/%s)", otelCfg.Endpoint, otelCfg.Protocol)
		}
	} else {
		log.Println("note: OpenTelemetry export disabled — enable it in Settings → Observability")
	}

	hub := ws.NewHub()
	go hub.Run()

	pool := routeros.NewPool()

	// The device I/O fence. MIKROTIK_NMS_POLLERS=off is the code default, so a
	// checkout started against a copy of a production database never contacts
	// the fleet; "local" dials only inside MIKROTIK_NMS_ALLOWED_TARGET_CIDRS.
	// Installed before the manager and the router exist, so nothing can dial
	// ahead of it.
	routeros.SetDialPolicy(&routeros.DialPolicy{Enabled: cfg.PollersEnabled(), Allowed: cfg.AllowedTargetCIDRs})
	// The client's questions to the database: which devices are weak (C1:
	// longer deadlines), and where a TLS pin mismatch is recorded.
	poller.InstallRouterOSHooks(db)
	if cfg.PollersEnabled() {
		n := 0
		if devices, err := queries.ListDevices(db); err == nil {
			n = len(devices)
		}
		log.Printf("pollers: local — polling %d devices, targets limited to %s", n, strings.Join(cfg.AllowedTargetCIDRStrings(), ","))
	} else {
		log.Println("pollers: off (MIKROTIK_NMS_POLLERS=off — no device or UDP I/O)")
	}

	// Informational only: the live SMTP config is resolved per request from
	// app_settings (Settings page) with this env config as the fallback, so a
	// nil mailer is passed to the router and the API builds one on demand.
	envMailer := mailer.New(mailer.Config{
		SMTPHost:          cfg.SMTPHost,
		SMTPPort:          cfg.SMTPPort,
		SMTPUser:          cfg.SMTPUser,
		SMTPPass:          cfg.SMTPPass,
		SMTPFrom:          cfg.SMTPFrom,
		SMTPTLSMode:       cfg.SMTPTLSMode,
		SMTPTLSSkipVerify: cfg.SMTPTLSSkipVerify,
		PublicBaseURL:     cfg.PublicBaseURL,
	})
	if !envMailer.Enabled() {
		log.Println("note: SMTP not configured via env — configure it in Settings (or env) to enable self-service password-reset emails")
	}

	pollerMgr := poller.NewManager(db, pool, hub, cfg)
	go pollerMgr.Start()

	// Flow collector counters → OTel gauges (read on each export cycle; a
	// cheap in-memory snapshot, no DB I/O).
	flows := pollerMgr.Flows()
	telemetry.SetFlowMetricsSource(func() telemetry.FlowMetrics { return flowMetrics(flows.Status(), time.Now()) })
	// The CPU gauge exports the sampler's own latest reading per device —
	// only while fresh, with its time — instead of whatever the device row
	// last held.
	telemetry.SetCPUSource(func() map[string]telemetry.CPUSample {
		latest := pollerMgr.LatestCPU()
		out := make(map[string]telemetry.CPUSample, len(latest))
		for id, r := range latest {
			out[id] = telemetry.CPUSample{Load: r.CPU, At: r.At}
		}
		return out
	})

	router := api.NewRouter(db, hub, cfg, pool, nil, pollerMgr.PortStats(), flows, pollerMgr.Cadence(), pollerMgr.ClientRefresh())

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("MikroTik NMS listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	pollerMgr.Stop()
	// The flow collector flushes its open minutes on shutdown; let it finish
	// before the pool and (deferred) the database close. Bounded: 5 s.
	if !flows.Wait(5 * time.Second) {
		log.Println("warning: flow collector did not finish its final flush within 5 s")
	}
	pool.CloseAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}

	if otelShutdown != nil {
		octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ocancel()
		if err := otelShutdown(octx); err != nil {
			log.Printf("otel shutdown error: %v", err)
		}
	}
}

// flowMetrics adapts the flow collector's status to the telemetry snapshot
// (the flow package never imports telemetry).
func flowMetrics(st flow.Status, now time.Time) telemetry.FlowMetrics {
	g := st.Global
	m := telemetry.FlowMetrics{
		Dropped: map[string]int64{
			"queue":        g.QueueDropped,
			"kernel":       g.KernelDropped,
			"rate_limited": g.RateLimited,
			"writer":       g.WriterDropped,
			"unknown":      g.UnknownDatagrams,
			"overflow":     g.OverflowRecords,
			"late":         g.LateRecords,
			"dup":          g.DupDropped,
			"self_export":  g.SelfExportDropped,
			"rejected":     g.Rejected,
		},
		Exporters: make([]telemetry.FlowExporterMetrics, 0, len(st.Exporters)),
	}
	for _, e := range st.Exporters {
		age := -1.0
		if e.LastSeen != nil {
			age = max(now.Sub(*e.LastSeen).Seconds(), 0)
		}
		m.Exporters = append(m.Exporters, telemetry.FlowExporterMetrics{
			Name:               e.Name,
			Address:            e.Address,
			Datagrams:          e.Counters["datagrams"],
			Flows:              e.Counters["flows"],
			Bytes:              e.Counters["bytes"],
			DecodeErrors:       e.Counters["decode_errors"],
			TemplateMisses:     e.Counters["template_misses"],
			SeqLost:            e.Counters["seq_lost"],
			LastSeenAgeSeconds: age,
		})
	}
	return m
}
