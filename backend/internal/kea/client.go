// Package kea provides a client for the Kea DHCP Control Agent REST API.
//
// HTTP Basic Auth is supported by embedding credentials in the URL, e.g.
//
//	http://user:pass@10.0.0.1:8000
//
// since net/http honors userinfo on the request URL. For unauthenticated
// agents (the common case on a trusted management LAN) just use the bare URL.
package kea

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Lease4 represents an IPv4 DHCP lease from Kea.
type Lease4 struct {
	IPAddress string `json:"ip-address"`
	HWAddress string `json:"hw-address"`
	Hostname  string `json:"hostname"`
	State     int    `json:"state"` // 0=default (active), 1=declined, 2=expired
	SubnetID  int    `json:"subnet-id"`
}

type command struct {
	Command string   `json:"command"`
	Service []string `json:"service"`
}

type response struct {
	Result    int    `json:"result"` // 0=success, 1=error, 3=empty
	Text      string `json:"text"`
	Arguments struct {
		Leases []Lease4 `json:"leases"`
	} `json:"arguments"`
}

// Client queries the Kea Control Agent.
type Client struct {
	url    string
	client *http.Client
}

// New creates a Kea client for the given Control Agent URL (e.g. "http://192.0.2.81:8000").
func New(url string) *Client {
	return &Client{
		url:    strings.TrimRight(url, "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetLeases4 returns all active IPv4 leases.
func (c *Client) GetLeases4() ([]Lease4, error) {
	body, err := json.Marshal(command{Command: "lease4-get-all", Service: []string{"dhcp4"}})
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kea: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kea: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kea: read body: %w", err)
	}

	// Kea wraps the response in an array
	var responses []response
	if err := json.Unmarshal(data, &responses); err != nil {
		return nil, fmt.Errorf("kea: decode: %w", err)
	}

	if len(responses) == 0 {
		return nil, fmt.Errorf("kea: empty response")
	}

	r := responses[0]
	if r.Result != 0 {
		return nil, fmt.Errorf("kea: %s", r.Text)
	}

	// Filter to active leases and normalize MAC to uppercase. Kea stores
	// hw-address in colon notation already, no separator munging needed.
	active := make([]Lease4, 0, len(r.Arguments.Leases))
	for _, l := range r.Arguments.Leases {
		if l.State != 0 {
			continue
		}
		l.HWAddress = strings.ToUpper(l.HWAddress)
		active = append(active, l)
	}
	return active, nil
}
