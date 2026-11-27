// Package fences holds the dependency fences of the device I/O choke point,
// as tests: which packages may import the RouterOS library and which files
// may open a network connection themselves. Everything else reaches a device
// through internal/routeros (Pool.Dial, DialOnce, Ping, ScanSubnet,
// ScanMNDP …), where the dial policy, the deadlines, the TLS pins and the
// command observer live.
//
// The seams fences (seams_test.go) keep the collect/apply split of the
// pollers apart: internal/edgeproto imports only the standard library and
// devclass, internal/ingest reaches no device and no network, only the
// monolith's wiring imports ingest, and a poller split into its collect half
// (collectHalves) neither holds the database nor opens a device session.
//
// The fences are a ratchet: what exists today is listed with its reason, a
// new crossing fails the test, and so does a listed crossing that is gone
// (the list only shrinks; collectHalves grows with each converted poller).
package fences
