// Package flow is the NetFlow v5/v9, IPFIX and sFlow collector: UDP
// listeners with an exporter allow-list, guarded decoding (flow/decode, the
// only goflow2 importer), NAT normalisation, per-minute top-N aggregation
// into flow_1m (+ hourly rollup into flow_1h), template persistence, the
// ifIndex → name sync, native observation points and the address plan.
//
// Rows are stored per exporter and never summed across exporters: one packet
// may be seen by several exporters (and twice by one), so every view picks a
// single observation point (exporter + ifIndex set).
package flow

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Config is what main passes (from config.Config).
type Config struct {
	Listen     []string // UDP bind addresses, e.g. [":2055"]; empty = collector off
	CaptureDir string   // dev only: write the first 20 raw datagrams per exporter here; "" = off
}

// WebSocket topics.
const (
	TopicFlushed = "flows.flushed" // FlushedEvent after each minute flush (only when subscribed)
	TopicStatus  = "flows.status"  // Status every 30 s (only when subscribed)
)

// Exporter states.
const (
	StateOK       = "ok"       // enabled and last datagram < 3 min ago
	StateStale    = "stale"    // enabled, seen before, last datagram >= 3 min ago
	StateNever    = "never"    // enabled, never seen
	StateDisabled = "disabled" // enabled = false
)

// staleAfter is how long after its last datagram an exporter turns stale.
const staleAfter = 3 * time.Minute

// FlushedEvent is published on TopicFlushed after each minute flush.
type FlushedEvent struct {
	ExporterIDs    []int64   `json:"exporter_ids"`
	Bucket         time.Time `json:"bucket"`          // minute just flushed
	FlushedThrough time.Time `json:"flushed_through"` // = Bucket + 1m
}

// Status is the collector's health: configuration, global counters and one
// entry per flow_exporters row.
type Status struct {
	Enabled        bool             `json:"enabled"`
	Listen         []string         `json:"listen"`          // never nil
	ListenErrors   []string         `json:"listen_errors"`   // never nil; "<addr>: <bind error>"
	StartedAt      *time.Time       `json:"started_at"`      // null when disabled
	FlushedThrough *time.Time       `json:"flushed_through"` // null before the first flush
	RolledThrough  *time.Time       `json:"rolled_through"`
	RcvBufBytes    int              `json:"rcvbuf_bytes"`
	Global         GlobalStats      `json:"global"`
	Exporters      []ExporterStatus `json:"exporters"`       // never nil; every flow_exporters row, ordered by name
	Unknown        []UnknownSender  `json:"unknown_senders"` // never nil; newest first, <= 16
	// Blocked are the senders dropped before decode (flow_sender_policy):
	// deleted exporters and managed devices that must not export. Never nil;
	// ordered by name, then address.
	Blocked []BlockedSender `json:"blocked_senders"`
	// AutoAcceptClasses is the flow_auto_accept_classes setting in force:
	// "" (nothing is accepted automatically) or "router".
	AutoAcceptClasses string `json:"auto_accept_classes"`
}

// GlobalStats are collector-wide counters, all cumulative since process start.
type GlobalStats struct {
	Datagrams         int64 `json:"datagrams"`
	UnknownDatagrams  int64 `json:"unknown_datagrams"`
	Rejected          int64 `json:"rejected"` // bad version, precheck failures, disabled exporter
	Blocked           int64 `json:"blocked"`  // from blocked senders, dropped before decode
	RateLimited       int64 `json:"rate_limited"`
	QueueDropped      int64 `json:"queue_dropped"`
	KernelDropped     int64 `json:"kernel_dropped"` // /proc/self/net/udp{,6} drops for our sockets
	WriterDropped     int64 `json:"writer_dropped"`
	LateRecords       int64 `json:"late_records"`
	OverflowRecords   int64 `json:"overflow_records"`
	SelfExportDropped int64 `json:"self_export_dropped"`
	DupDropped        int64 `json:"dup_dropped"`
	Panics            int64 `json:"panics"`
}

// Rates are per-minute averages.
type Rates struct {
	Datagrams float64 `json:"datagrams"`
	Flows     float64 `json:"flows"`
	Bytes     float64 `json:"bytes"`
}

// ExporterStatus is one exporter's configuration and health.
type ExporterStatus struct {
	ID               int64            `json:"id"`
	Name             string           `json:"name"`
	Address          string           `json:"address"`
	Kind             string           `json:"kind"`
	DeviceID         *string          `json:"device_id"`
	Enabled          bool             `json:"enabled"`
	Auto             bool             `json:"auto"`
	SamplingOverride int              `json:"sampling_override"`
	NATAddresses     []string         `json:"nat_addresses"` // never nil
	State            string           `json:"state"`         // StateOK | StateStale | StateNever | StateDisabled
	LastSeen         *time.Time       `json:"last_seen"`
	Protocol         string           `json:"protocol"` // netflow5 | netflow9 | ipfix | sflow5 | ""
	SamplingRate     int              `json:"sampling_rate"`
	Templates        int              `json:"templates"`      // live template count (0 when collector off)
	TemplateState    string           `json:"template_state"` // ok | waiting | persisted | n/a
	PerMinute        Rates            `json:"per_minute"`     // average over the last 5 min (0 when off)
	Counters         map[string]int64 `json:"counters"`       // every key of ExporterCounterKeys present
	TimeSource       map[string]int64 `json:"time_source"`    // keys: sysuptime absolute uptime_est export
	ClockSkewMs      int64            `json:"clock_skew_ms"`
	LastError        string           `json:"last_error"`
	LastErrorAt      *time.Time       `json:"last_error_at"`
	InterfacesSeen   int              `json:"interfaces_seen"` // distinct non-zero ifIndexes with last_seen < 1 h
	// DeviceRole / DeviceRoleReason: the role of the managed device at the
	// exporter's address (devclass.Role; "" when it is not a managed device).
	DeviceRole       string `json:"device_role"`
	DeviceRoleReason string `json:"device_role_reason"`
	// ExportWarning is set when that device must not export flows (switch,
	// access point, CPE, C1): the exporter keeps working, the reason is shown.
	ExportWarning string `json:"export_warning"`
}

// BlockedSender is a sender whose datagrams are dropped before decode until
// an admin unblocks or re-adds it.
type BlockedSender struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Source  string `json:"source"` // deleted (a deleted exporter) | auto (a device that must not export)
	// DeviceID is the managed device the block was created for ("" = none).
	DeviceID         string     `json:"device_id"`
	DeviceName       string     `json:"device_name"`
	DeviceRole       string     `json:"device_role"`
	DeviceRoleReason string     `json:"device_role_reason"`
	CreatedBy        string     `json:"created_by"` // "" = the collector
	CreatedAt        *time.Time `json:"created_at"`
	LastSeen         *time.Time `json:"last_seen"` // last dropped datagram
	Datagrams        int64      `json:"datagrams"` // dropped since blocked
}

// BlockedSenderFromRow builds a blocked sender from its row (persisted
// counters; the device fields are left to the caller).
func BlockedSenderFromRow(p queries.FlowSenderPolicy) BlockedSender {
	return BlockedSender{Address: p.Address, Name: p.Name, Kind: p.Kind, Reason: p.Reason, Source: p.Source,
		DeviceID: p.DeviceID, CreatedBy: p.CreatedBy, CreatedAt: timePtr(p.CreatedAt), LastSeen: timePtr(p.LastSeen),
		Datagrams: p.Datagrams}
}

// ExporterCounterKeys: datagrams, flows, bytes, decode_errors, template_misses
// and seq_lost are persisted-cumulative + unpersisted live delta; the rest are
// since process start. reboots counts detected exporter reboots,
// iface_overflow the ifIndexes beyond the per-exporter cap that got no
// interface row / point, oversized the datagrams over 9216 bytes rejected.
var ExporterCounterKeys = []string{"datagrams", "flows", "bytes", "decode_errors", "template_misses", "replayed",
	"pending_dropped", "seq_lost", "rate_limited", "dup_dropped", "self_export_dropped", "nat_dst_records",
	"reboots", "iface_overflow", "oversized"}

// TimeSourceKeys are the keys of ExporterStatus.TimeSource.
var TimeSourceKeys = []string{"sysuptime", "absolute", "uptime_est", "export"}

// Template states.
const (
	TemplateOK        = "ok"
	TemplateWaiting   = "waiting"
	TemplatePersisted = "persisted"
	TemplateNA        = "n/a"
)

// UnknownSender is a source that sent datagrams but is not an exporter.
type UnknownSender struct {
	Address    string    `json:"address"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Datagrams  int64     `json:"datagrams"`
	Protocol   string    `json:"protocol"`    // sniffed from the header; "" when unrecognised
	DeviceID   *string   `json:"device_id"`   // set when the address is a managed device that was not accepted
	DeviceName string    `json:"device_name"` // identity or address; "" when not a device
	// DeviceRole / DeviceRoleReason: that device's role (devclass.Role).
	DeviceRole       string `json:"device_role"`
	DeviceRoleReason string `json:"device_role_reason"`
}

// disabledStatus is the Status of a nil or disabled collector.
func disabledStatus() Status {
	return Status{
		Listen:       []string{},
		ListenErrors: []string{},
		Exporters:    []ExporterStatus{},
		Unknown:      []UnknownSender{},
		Blocked:      []BlockedSender{},
	}
}

// exporterState derives the state of an exporter from its configuration and
// last datagram.
func exporterState(enabled bool, lastSeen, now time.Time) string {
	switch {
	case !enabled:
		return StateDisabled
	case lastSeen.IsZero():
		return StateNever
	case now.Sub(lastSeen) < staleAfter:
		return StateOK
	}
	return StateStale
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// ExporterStatusFromRow builds the status of an exporter from its DB row only
// (persisted counters; live-only fields zero). The API uses it when the
// collector is disabled/nil and for write responses.
func ExporterStatusFromRow(e queries.FlowExporter, now time.Time) ExporterStatus {
	s := ExporterStatus{
		ID:               e.ID,
		Name:             e.Name,
		Address:          e.Address,
		Kind:             e.Kind,
		Enabled:          e.Enabled,
		Auto:             e.Auto,
		SamplingOverride: e.SamplingOverride,
		NATAddresses:     make([]string, 0, len(e.NATAddresses)),
		State:            exporterState(e.Enabled, e.LastSeen, now),
		LastSeen:         timePtr(e.LastSeen),
		Protocol:         e.Protocol,
		SamplingRate:     max(e.SamplingRate, 1),
		TemplateState:    TemplateNA,
		Counters:         make(map[string]int64, len(ExporterCounterKeys)),
		TimeSource:       make(map[string]int64, len(TimeSourceKeys)),
		ClockSkewMs:      e.ClockSkewMs,
		LastError:        e.LastError,
		LastErrorAt:      timePtr(e.LastErrorAt),
	}
	if e.DeviceID != "" {
		d := e.DeviceID
		s.DeviceID = &d
	}
	for _, a := range e.NATAddresses {
		s.NATAddresses = append(s.NATAddresses, a.String())
	}
	for _, k := range ExporterCounterKeys {
		s.Counters[k] = 0
	}
	for _, k := range TimeSourceKeys {
		s.TimeSource[k] = 0
	}
	s.Counters["datagrams"] = e.Datagrams
	s.Counters["flows"] = e.Flows
	s.Counters["bytes"] = e.Bytes
	s.Counters["decode_errors"] = e.DecodeErrors
	s.Counters["template_misses"] = e.TemplateMisses
	s.Counters["seq_lost"] = e.SeqLost
	return s
}

// parseListenPort returns the port of one listen entry, or 0 when the entry
// is not host:port with a port in 1..65535.
func parseListenPort(entry string) int {
	_, p, err := net.SplitHostPort(strings.TrimSpace(entry))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// ListenPort returns the port of the first valid listen entry, 0 when none.
func ListenPort(listen []string) int {
	for _, l := range listen {
		if p := parseListenPort(l); p > 0 {
			return p
		}
	}
	return 0
}
