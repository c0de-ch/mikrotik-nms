// Command deploy-agent is a tiny webhook listener that runs a configurable
// shell command when GitHub sends a signed push event for an allowed repo
// and ref.
//
// It is designed to live on a host inside the user's network so that secrets
// (Proxmox API tokens, SSH keys, etc.) never leave the local machine. The
// public repository on GitHub holds only a shared HMAC secret used to sign
// the webhook payload — that secret is sufficient to authenticate the
// webhook but is useless on its own without access to this agent's
// network.
//
// Configuration is read entirely from environment variables; see
// deploy/webhook-agent/agent.env.example for the full list.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type config struct {
	listen        string
	webhookSecret string
	allowedRepo   string // e.g. "owner/repo"
	allowedRef    string // e.g. "refs/heads/main"
	deployCommand string // shell command run via /bin/sh -c
	deployTimeout time.Duration
}

func loadConfig() (*config, error) {
	envOr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c := &config{
		listen:        envOr("LISTEN", ":9000"),
		webhookSecret: os.Getenv("WEBHOOK_SECRET"),
		allowedRepo:   os.Getenv("ALLOWED_REPO"),
		allowedRef:    envOr("ALLOWED_REF", "refs/heads/main"),
		deployCommand: os.Getenv("DEPLOY_COMMAND"),
		deployTimeout: 30 * time.Minute,
	}
	if c.webhookSecret == "" {
		return nil, errors.New("WEBHOOK_SECRET is required")
	}
	if c.allowedRepo == "" {
		return nil, errors.New("ALLOWED_REPO is required (e.g. owner/repo)")
	}
	if c.deployCommand == "" {
		return nil, errors.New("DEPLOY_COMMAND is required")
	}
	if t := os.Getenv("DEPLOY_TIMEOUT"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, fmt.Errorf("DEPLOY_TIMEOUT: %w", err)
		}
		c.deployTimeout = d
	}
	return c, nil
}

type pushEvent struct {
	Ref        string `json:"ref"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"head_commit"`
}

type server struct {
	cfg     *config
	mu      sync.Mutex // serialize deploys
	seen    *seenCache
	running bool
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20)) // 5 MiB cap
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if !verifySignature(body, r.Header.Get("X-Hub-Signature-256"), s.cfg.webhookSecret) {
		log.Printf("invalid signature from %s", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	delivery := r.Header.Get("X-GitHub-Delivery")
	if s.seen.has(delivery) {
		fmt.Fprintf(w, "duplicate delivery %s — ignored\n", delivery)
		return
	}
	s.seen.add(delivery)

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		fmt.Fprintln(w, "pong")
		return
	case "push":
		// fallthrough
	default:
		log.Printf("ignoring %s event from %s", event, r.RemoteAddr)
		fmt.Fprintf(w, "ignored: event=%s\n", event)
		return
	}

	var pe pushEvent
	if err := json.Unmarshal(body, &pe); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if pe.Repository.FullName != s.cfg.allowedRepo {
		log.Printf("ignoring push for %s (allowed: %s)", pe.Repository.FullName, s.cfg.allowedRepo)
		fmt.Fprintf(w, "ignored: repo=%s\n", pe.Repository.FullName)
		return
	}
	if pe.Ref != s.cfg.allowedRef {
		log.Printf("ignoring push for ref %s (allowed: %s)", pe.Ref, s.cfg.allowedRef)
		fmt.Fprintf(w, "ignored: ref=%s\n", pe.Ref)
		return
	}
	if pe.Deleted {
		fmt.Fprintln(w, "ignored: branch deleted")
		return
	}

	commit := pe.HeadCommit.ID
	msg := strings.Split(pe.HeadCommit.Message, "\n")[0]
	log.Printf("queueing deploy: %s @ %s — %s", pe.Repository.FullName, shortCommit(commit), msg)
	go s.runDeploy(commit, msg)

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "accepted: deploy queued for %s\n", shortCommit(commit))
}

func (s *server) runDeploy(commit, msg string) {
	s.mu.Lock()
	s.running = true
	defer func() {
		s.running = false
		s.mu.Unlock()
	}()

	log.Printf("[deploy] starting commit=%s", shortCommit(commit))
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.deployTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", s.cfg.deployCommand)
	cmd.Env = append(os.Environ(),
		"DEPLOY_COMMIT="+commit,
		"DEPLOY_COMMIT_MESSAGE="+msg,
	)
	cmd.Stdout = newPrefixWriter(os.Stdout, "[deploy] ")
	cmd.Stderr = newPrefixWriter(os.Stderr, "[deploy] ")

	start := time.Now()
	if err := cmd.Run(); err != nil {
		log.Printf("[deploy] FAILED after %s: %v", time.Since(start).Truncate(time.Second), err)
		return
	}
	log.Printf("[deploy] OK in %s", time.Since(start).Truncate(time.Second))
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"deployBusy":  running,
		"allowedRepo": s.cfg.allowedRepo,
		"allowedRef":  s.cfg.allowedRef,
	})
}

func verifySignature(body []byte, sigHeader, secret string) bool {
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := mac.Sum(nil)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// seenCache tracks recently processed delivery IDs to drop duplicates
// caused by GitHub retries.
type seenCache struct {
	mu  sync.Mutex
	ids map[string]time.Time
	ttl time.Duration
}

func newSeenCache(ttl time.Duration) *seenCache {
	return &seenCache{ids: make(map[string]time.Time), ttl: ttl}
}

func (c *seenCache) has(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.ids[id]
	if !ok {
		return false
	}
	if time.Since(t) > c.ttl {
		delete(c.ids, id)
		return false
	}
	return true
}

func (c *seenCache) add(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids[id] = time.Now()
	if len(c.ids) > 1000 {
		now := time.Now()
		for k, t := range c.ids {
			if now.Sub(t) > c.ttl {
				delete(c.ids, k)
			}
		}
	}
}

// prefixWriter prefixes every line written through it with the given string.
type prefixWriter struct {
	w      io.Writer
	prefix []byte
	bol    bool // beginning of line
}

func newPrefixWriter(w io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{w: w, prefix: []byte(prefix), bol: true}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	written := 0
	for _, c := range b {
		if p.bol {
			if _, err := p.w.Write(p.prefix); err != nil {
				return written, err
			}
			p.bol = false
		}
		if _, err := p.w.Write([]byte{c}); err != nil {
			return written, err
		}
		written++
		if c == '\n' {
			p.bol = true
		}
	}
	return written, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	srv := &server{cfg: cfg, seen: newSeenCache(24 * time.Hour)}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", srv.handleWebhook)
	mux.HandleFunc("/healthz", srv.handleHealth)

	log.Printf("deploy-agent listening on %s", cfg.listen)
	log.Printf("  allowed repo: %s", cfg.allowedRepo)
	log.Printf("  allowed ref:  %s", cfg.allowedRef)
	log.Printf("  deploy timeout: %s", cfg.deployTimeout)

	httpSrv := &http.Server{
		Addr:         cfg.listen,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
