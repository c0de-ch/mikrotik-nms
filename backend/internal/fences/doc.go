// Package fences holds the dependency fences of the device I/O choke point,
// as tests: which packages may import the RouterOS library and which files
// may open a network connection themselves. Everything else reaches a device
// through internal/routeros (Pool.Dial, DialOnce, Ping, ScanSubnet,
// ScanMNDP …), where the dial policy, the deadlines, the TLS pins and the
// command observer live.
//
// The fences are a ratchet: what exists today is listed with its reason, a
// new crossing fails the test, and so does a listed crossing that is gone
// (the list only shrinks).
package fences
