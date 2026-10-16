// Package devclass derives a RouterOS device's CPU class from the facts
// /system/resource/print reports, and turns that class into the polling
// cadences and budgets the pollers must respect. It is pure: no database, no
// network, no clock except what the caller passes in.
//
// Classes key how much the NMS may ask of a device; a device's role (router,
// switch, AP, …) and capabilities decide which reads apply, never how much.
// Budgets must follow the class, never a device name: every MIPS board reads
// high CPU under the same load an ARM board shrugs off.
package devclass

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// Class is a device's CPU class. Higher is more capable; "gentler" means a
// lower class, i.e. less load allowed.
type Class string

const (
	// C0Unknown is a device that has never been probed. It is budgeted as
	// C1 until its first /system/resource read.
	C0Unknown Class = "C0"
	// C1 is the weakest class: MIPS, or at most 128 MiB of RAM (a 1-core
	// 650 MHz CRS354, a hAP lite, a PowerBox).
	C1 Class = "C1"
	// C2 is a small ARM: 32-bit, or at most two cores at up to 1 GHz
	// nominal (CRS3xx switches on a single 800 MHz core).
	C2 Class = "C2"
	// C3 is a capable arm64 or x86_64 device with two or more cores
	// (RB5009, CCR2004, CHR, the ax access points).
	C3 Class = "C3"
)

// mib is one mebibyte in bytes.
const mib = 1 << 20

// c1MemoryLimit is the total-memory ceiling (inclusive) that makes a device
// C1 whatever its CPU.
const c1MemoryLimit = 128 * mib

// ParseClass returns the Class for its string form ("C1" … "C3", "C0"),
// case-insensitively. ok is false for anything else, including "".
func ParseClass(s string) (Class, bool) {
	switch Class(strings.ToUpper(strings.TrimSpace(s))) {
	case C0Unknown:
		return C0Unknown, true
	case C1:
		return C1, true
	case C2:
		return C2, true
	case C3:
		return C3, true
	}
	return "", false
}

// Valid reports whether c is one of the known classes.
func (c Class) Valid() bool {
	_, ok := ParseClass(string(c))
	return ok
}

// String returns the class name, "C0" for the zero value.
func (c Class) String() string {
	if c == "" {
		return string(C0Unknown)
	}
	return string(c)
}

// Label is a short human-readable name for the class.
func (c Class) Label() string {
	switch c {
	case C1:
		return "weak"
	case C2:
		return "small"
	case C3:
		return "capable"
	}
	return "unprobed"
}

// Rank orders classes by capability: C1 (and the unprobed C0, budgeted as
// C1) is 1, C3 is 3. An invalid class ranks as C1.
func (c Class) Rank() int {
	switch c {
	case C2:
		return 2
	case C3:
		return 3
	}
	return 1
}

// Budgeted is the class whose cadences and budgets apply: C0 is treated as
// C1 until the first probe, and anything invalid as C1 too.
func (c Class) Budgeted() Class {
	switch c {
	case C1, C2, C3:
		return c
	}
	return C1
}

// ResourceFacts are the /system/resource/print fields the classification
// reads. CPUFrequencyMHz is the *current* frequency and drops under frequency
// scaling, so callers pass the maximum value they have seen for the device;
// it is consulted only for boards missing from the board table.
type ResourceFacts struct {
	Architecture     string // architecture-name: mipsbe, mmips, smips, arm, arm64, x86_64, …
	CPUCount         int    // cpu-count
	CPUFrequencyMHz  int    // cpu-frequency, maximum seen (MHz)
	TotalMemoryBytes int64  // total-memory (bytes)
	BoardName        string // board-name
}

// ParseResourceFacts reads the classification fields out of one
// /system/resource/print sentence. Missing or malformed numbers are zero.
func ParseResourceFacts(m map[string]string) ResourceFacts {
	f := ResourceFacts{
		Architecture: strings.TrimSpace(m["architecture-name"]),
		BoardName:    strings.TrimSpace(m["board-name"]),
	}
	if v, err := strconv.Atoi(strings.TrimSpace(m["cpu-count"])); err == nil && v > 0 {
		f.CPUCount = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(m["cpu-frequency"])); err == nil && v > 0 {
		f.CPUFrequencyMHz = v
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(m["total-memory"]), 10, 64); err == nil && v > 0 {
		f.TotalMemoryBytes = v
	}
	return f
}

// Empty reports whether nothing usable was probed.
func (f ResourceFacts) Empty() bool {
	return f.Architecture == "" && f.CPUCount == 0 && f.CPUFrequencyMHz == 0 &&
		f.TotalMemoryBytes == 0 && f.BoardName == ""
}

// Classify derives the class from probed facts (design §4.2). The first
// matching rule wins:
//
//  1. C1: architecture mipsbe, mmips or smips, or total memory ≤ 128 MiB.
//  2. C2: 32-bit ARM, or at most 2 cores at ≤ 1 GHz nominal.
//  3. C3: arm64 or x86_64 with ≥ 2 cores.
//  4. otherwise C2 (conservative).
//
// Cores and nominal frequency come from the board table when the board is
// known; otherwise from the facts (cpu-count and the maximum cpu-frequency
// seen). Empty facts are C0Unknown.
func Classify(f ResourceFacts) Class {
	if f.Empty() {
		return C0Unknown
	}
	arch := normalizeArch(f.Architecture)
	cores, mhz, mem := f.CPUCount, f.CPUFrequencyMHz, f.TotalMemoryBytes
	if spec, ok := LookupBoard(f.BoardName); ok {
		// The datasheet beats the print for cores and frequency (the print
		// shows the scaled-down current frequency); probed facts fill in
		// anything the table leaves at zero.
		if arch == "" {
			arch = normalizeArch(spec.Architecture)
		}
		if spec.Cores > 0 {
			cores = spec.Cores
		}
		if spec.NominalMHz > 0 {
			mhz = spec.NominalMHz
		}
		if mem == 0 {
			mem = spec.MemoryBytes
		}
	}
	if isMIPS(arch) || (mem > 0 && mem <= c1MemoryLimit) {
		return C1
	}
	if isARM32(arch) || (cores > 0 && cores <= 2 && mhz > 0 && mhz <= 1000) {
		return C2
	}
	if (isARM64(arch) || isX86(arch)) && cores >= 2 {
		return C3
	}
	return C2
}

// Effective applies an admin override to a probed class. An override may
// only make a device gentler: one towards a more capable class than the
// probed (budgeted) one is ignored and reported with clamped=true. An empty
// or invalid override leaves the probed class alone. Unprobed devices are
// budgeted as C1, so for them only a C1 override is accepted.
func Effective(probed, override Class) (Class, bool) {
	base := probed.Budgeted()
	ov, ok := ParseClass(string(override))
	if !ok || ov == C0Unknown {
		return base, false
	}
	if ov.Rank() > base.Rank() {
		return base, true
	}
	return ov, false
}

// Phase derives a stable per-device scheduling phase from an identifier, so
// every device's loops are offset from each other without any state.
func Phase(deviceID string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(deviceID))
	return h.Sum64()
}

func normalizeArch(a string) string {
	return strings.ToLower(strings.TrimSpace(a))
}

func isMIPS(arch string) bool {
	switch arch {
	case "mipsbe", "mmips", "smips", "mipsle":
		return true
	}
	return false
}

func isARM32(arch string) bool {
	switch arch {
	case "arm", "arm32", "armv7", "armv7l", "armhf":
		return true
	}
	return false
}

func isARM64(arch string) bool {
	switch arch {
	case "arm64", "aarch64", "armv8":
		return true
	}
	return false
}

func isX86(arch string) bool {
	switch arch {
	case "x86_64", "amd64", "x86", "i386", "x64":
		return true
	}
	return false
}
