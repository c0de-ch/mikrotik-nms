package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mikrotik-nms/backend/internal/api"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
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
	if !cipher.Enabled() {
		log.Println("warning: MIKROTIK_NMS_ENCRYPTION_KEY is not set — device passwords are stored unencrypted at rest")
	} else if n, err := queries.EncryptPlaintextDeviceSecrets(db); err != nil {
		log.Printf("warning: could not encrypt existing device secrets: %v", err)
	} else if n > 0 {
		log.Printf("encrypted %d plaintext device password(s) at rest", n)
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

	pool := routeros.NewPool(cfg.ROSTLSVerify)

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

	router := api.NewRouter(db, hub, cfg, pool, nil)

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
