package poller

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// classHysteresis is how long a probed class change must hold before it is
// applied (design §4.2). The first probe that disagrees with the stored class
// starts a candidate; the change lands on a probe at least this long after
// it that still agrees, every probe in between agreeing as well. A one-off
// odd print (a VM resized for an hour, a read during an upgrade) never moves
// a device's cadences, and a device does not flap between two plans.
const classHysteresis = 24 * time.Hour

// classCandidate is a pending class change: the class the probes report
// and when they started to.
type classCandidate struct {
	class devclass.Class
	since time.Time
}

// classGate holds the pending class changes. In memory: a restart only
// restarts a candidate's 24 h, the stored class keeps applying meanwhile.
type classGate struct {
	mu      sync.Mutex
	pending map[string]classCandidate
}

func newClassGate() *classGate {
	return &classGate{pending: make(map[string]classCandidate)}
}

// classVerdict is what one probe does to the stored class.
type classVerdict int

const (
	classHold    classVerdict = iota // keep the stored class, write nothing
	classConfirm                     // the probe agrees: refresh the stored class's probe time and facts
	classApply                       // store the probed class
)

// judge decides what a probe that derived probed means for a device stored as
// current (devclass.C0Unknown when never classified) at now. started is set
// when the probe opened a new candidate, for one log line.
//
//   - A device never classified takes the probed class at once.
//   - A probe without usable facts (C0) says nothing: it keeps the stored
//     class and leaves a candidate alone.
//   - A probe that agrees confirms the stored class and drops a candidate.
//   - A disagreeing probe starts a candidate, or — when it agrees with a
//     candidate at least classHysteresis old — applies it.
func (g *classGate) judge(deviceID string, current, probed devclass.Class, now time.Time) (verdict classVerdict, started *classCandidate) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case current == devclass.C0Unknown:
		delete(g.pending, deviceID)
		return classApply, nil
	case probed == devclass.C0Unknown:
		return classHold, nil
	case probed == current:
		delete(g.pending, deviceID)
		return classConfirm, nil
	}
	cand, ok := g.pending[deviceID]
	if ok && cand.class == probed {
		if now.Sub(cand.since) >= classHysteresis {
			delete(g.pending, deviceID)
			return classApply, nil
		}
		return classHold, nil
	}
	cand = classCandidate{class: probed, since: now}
	g.pending[deviceID] = cand
	return classHold, &cand
}

// forget drops the candidates of devices not in keep.
func (g *classGate) forget(keep map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.pending {
		if !keep[id] {
			delete(g.pending, id)
		}
	}
}

// probeClass derives dev's CPU class from the proplisted resource print and
// stores it through the hysteresis: the first classification at once, a
// change only after classHysteresis of agreeing probes. cpu-frequency is the
// current, possibly scaled-down frequency, so the facts carry the highest one
// ever seen. Every class change is logged once and written to the audit log
// (action device_class_probed, no user: the system did it).
func (m *Manager) probeClass(dev queries.Device, fields map[string]string, now time.Time) {
	facts := devclass.ParseResourceFacts(fields)
	if dev.CPUMHzMax != nil && *dev.CPUMHzMax > facts.CPUFrequencyMHz {
		facts.CPUFrequencyMHz = *dev.CPUMHzMax
	}
	probed := devclass.Classify(facts)
	current := dev.ProbedClass()
	name := nonempty(dev.Identity, dev.Address)

	verdict, started := m.classes.judge(dev.ID, current, probed, now)
	switch verdict {
	case classApply:
		if err := queries.SetDeviceClass(m.db, dev.ID, probed, facts, now); err != nil {
			log.Printf("poller: store class of %s: %v", name, err)
			return
		}
		if probed == current {
			return // a probe without facts on a never-classified device: recorded, no change
		}
		log.Printf("poller: %s is class %s (was %s): %s", name, probed, current, describeFacts(facts))
		if _, err := queries.InsertAuditLog(m.db, &queries.AuditEntry{
			Action: "device_class_probed", DeviceID: dev.ID,
			Detail: fmt.Sprintf("%s → %s (%s)", current, probed, describeFacts(facts)),
		}); err != nil {
			log.Printf("poller: audit class change of %s: %v", name, err)
		}
	case classConfirm:
		if err := queries.SetDeviceClass(m.db, dev.ID, current, facts, now); err != nil {
			log.Printf("poller: store class of %s: %v", name, err)
		}
	case classHold:
		if started != nil {
			log.Printf("poller: %s probed as %s (%s); it stays %s unless the probes still say %s in %s",
				name, probed, describeFacts(facts), current, probed, classHysteresis)
		}
	}
}

// describeFacts is a short form of the facts behind a class, for logs.
func describeFacts(f devclass.ResourceFacts) string {
	var parts []string
	if f.BoardName != "" {
		parts = append(parts, f.BoardName)
	}
	if f.Architecture != "" {
		parts = append(parts, f.Architecture)
	}
	switch {
	case f.CPUCount == 1:
		parts = append(parts, "1 core")
	case f.CPUCount > 1:
		parts = append(parts, fmt.Sprintf("%d cores", f.CPUCount))
	}
	if f.CPUFrequencyMHz > 0 {
		parts = append(parts, fmt.Sprintf("%d MHz", f.CPUFrequencyMHz))
	}
	if f.TotalMemoryBytes > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB", f.TotalMemoryBytes>>20))
	}
	if len(parts) == 0 {
		return "no facts"
	}
	return strings.Join(parts, ", ")
}
