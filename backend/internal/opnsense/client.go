// Package opnsense fetches Kea DHCP leases from the OPNsense REST API.
//
// OPNsense doesn't normally expose the raw Kea Control Agent; it wraps Kea
// behind its own REST endpoint at /api/kea/leases4/search (+ leases6) which
// requires HTTP Basic Auth with a user-issued key+secret pair (System →
// Access → Users → <user> → API keys → "+ Add").
//
// Returned leases are filtered to active (state="active") and the MAC is
// upper-cased to match the rest of the NMS conventions.
package opnsense

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Lease is a single DHCP lease from OPNsense's Kea wrapper.
// The OPNsense field names are normalised to match what the rest of the
// codebase expects (HWAddress, IPAddress, Hostname).
type Lease struct {
	IPAddress string
	HWAddress string
	Hostname  string
	State     int // Kea numeric state: 0=active, 1=declined, 2=expired
	Type      string
}

// Client queries OPNsense's Kea lease REST endpoints.
type Client struct {
	baseURL   string
	authValue string
	http      *http.Client
}

// Config holds the connection settings for an OPNsense instance.
type Config struct {
	URL       string // e.g. "https://opnsense.lan:1443"
	APIKey    string
	APISecret string
	VerifyTLS bool
}

// New creates a Client. Returns nil if URL/key/secret are not all set.
func New(cfg Config) *Client {
	if cfg.URL == "" || cfg.APIKey == "" || cfg.APISecret == "" {
		return nil
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !cfg.VerifyTLS}, //nolint:gosec // user-controlled toggle
	}
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.APIKey + ":" + cfg.APISecret))
	return &Client{
		baseURL:   strings.TrimRight(cfg.URL, "/"),
		authValue: "Basic " + auth,
		http:      &http.Client{Transport: tr, Timeout: 15 * time.Second},
	}
}

// rawRow models the OPNsense lease row. Field names map to the JSON keys
// returned by /api/kea/leases{4,6}/search. Verified against OPNsense 25.x
// — `state` is a Kea numeric (0=active), `type` is usually "" for v4 rows.
type rawRow struct {
	Address  string `json:"address"`
	HWAddr   string `json:"hwaddr"`
	Hostname string `json:"hostname"`
	State    int    `json:"state"`
	Type     string `json:"type"`
}

type searchResponse struct {
	Total   int      `json:"total"`
	RowCount int     `json:"rowCount"`
	Rows    []rawRow `json:"rows"`
}

// GetLeases returns the union of active IPv4 and IPv6 leases. Errors on one
// endpoint don't prevent the other from being returned; both errors are
// joined into the returned error (if any).
func (c *Client) GetLeases() ([]Lease, error) {
	v4, err4 := c.fetch("/api/kea/leases4/search", "v4")
	v6, err6 := c.fetch("/api/kea/leases6/search", "v6")

	out := append(v4, v6...)
	switch {
	case err4 != nil && err6 != nil:
		return out, fmt.Errorf("opnsense: leases4: %w; leases6: %v", err4, err6)
	case err4 != nil:
		return out, fmt.Errorf("opnsense: leases4: %w", err4)
	case err6 != nil:
		return out, fmt.Errorf("opnsense: leases6: %w", err6)
	}
	return out, nil
}

func (c *Client) fetch(path, leaseType string) ([]Lease, error) {
	// OPNsense's search endpoints use POST with a bootstrap-table-style body.
	// rowCount=-1 means "all rows".
	body := strings.NewReader(`{"current":1,"rowCount":-1,"sort":{},"searchPhrase":""}`)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.authValue)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a chunk of the body for context (rate-limit it so a 1MB error
		// page doesn't blow up the log line).
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := make([]Lease, 0, len(sr.Rows))
	for _, r := range sr.Rows {
		// Kea state: 0=active, 1=declined, 2=expired. Keep only active.
		if r.State != 0 {
			continue
		}
		if r.HWAddr == "" || r.Address == "" {
			continue
		}
		t := r.Type
		if t == "" {
			t = leaseType
		}
		out = append(out, Lease{
			IPAddress: r.Address,
			HWAddress: strings.ToUpper(r.HWAddr),
			Hostname:  r.Hostname,
			State:     r.State,
			Type:      t,
		})
	}
	return out, nil
}
