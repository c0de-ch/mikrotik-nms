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
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
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

	hub := ws.NewHub()
	go hub.Run()

	pool := routeros.NewPool(cfg.ROSTLSVerify)

	pollerMgr := poller.NewManager(db, pool, hub, cfg)
	go pollerMgr.Start()

	router := api.NewRouter(db, hub, cfg, pool)

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
}
