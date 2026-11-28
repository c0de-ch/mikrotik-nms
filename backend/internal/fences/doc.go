// Package fences holds the dependency fences of the device I/O choke point,
// as tests: which packages may import the RouterOS library and which files
// may open a network connection themselves. Everything else reaches a device
// through internal/routeros (Pool.Dial, DialOnce, Ping, ScanSubnet,
// ScanMNDP …), where the dial policy, the deadlines, the TLS pins and the
// command observer live.
//
// The seams fences (seams_test.go) keep the collect/apply split of the
// pollers apart: internal/edgeproto imports only the standard library and
// devclass, internal/ingest only edgeproto and queries — and nothing either
// links reaches internal/routeros, a library or a socket; only the
// monolith's wiring imports ingest; and a poller split into its collect half
// is fenced by type, not by file: every file that declares a struct with a
// seam field, or a method of one, is listed in collectHalves, its fields
// have only the types collectHalfFieldTypes allows (one of them, the
// resolver, still reads dns_servers itself), and none of its files imports
// database/sql or ingest, uses a queries function that touches the
// database, or opens a device session.
//
// The fences are a ratchet: what exists today is listed with its reason, a
// new crossing fails the test, and so does a listed crossing that is gone
// (the list only shrinks; collectHalves grows with each converted poller).
package fences
