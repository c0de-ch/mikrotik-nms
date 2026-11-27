package poller

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The fleet is not idle. A device whose counters stand still and whose CPU
// sits at one value all hour leaves most of what the loops store at zero or
// not stored at all — port_stats_1m keeps no idle minute, so there would be
// no minute rows, no hourly rollup and no proration to pin. So the fake
// devices' interface counters, CPU load, free memory and uptime move with the
// simulated clock (DB-state goldens, dbstate_test.go).
//
// Each value is a pure function of the moment the command arrives: every run
// stores the same rows, and nothing a device answers depends on what it was
// sent before. Only values move: every table keeps its rows and every reply
// its size, so the call plans cannot see it. A moving value that changed a
// command would be a bug here — an uptime that went backwards is a reboot, a
// CPU near 80 % holds the hygiene audit back.

// trafficQuarters is how busy a port is in each minute of a five-minute
// cycle, in quarters of its nominal rate (hygiene.Port.Bps, the rate the
// personality's facts give the hygiene audit). The cycle averages the
// nominal rate, so what the NMS measures is what the personality says; and
// it changes from minute to minute, so how a read interval is shared out
// between the minutes it spans shows in every minute row. The port's
// position in the list shifts its cycle.
var trafficQuarters = [5]int64{4, 6, 2, 5, 3}

// rxEighths is the share of a port's traffic that comes in (rx), in eighths;
// the rest goes out (tx).
const rxEighths = 5

// Average packet sizes: what turns the byte counters into packet counters.
const (
	rxPacketBytes = 900
	txPacketBytes = 600
)

// trafficUnits integrates a port's traffic factor from the start of the plan
// to elapsed, in quarter-milliseconds (quarters × ms).
func trafficUnits(port int, elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return 0
	}
	ms := elapsed.Milliseconds()
	full, rest := ms/60_000, ms%60_000
	var units int64
	for m := int64(0); m < full; m++ {
		units += trafficQuarters[(m+int64(port))%5] * 60_000
	}
	return units + trafficQuarters[(full+int64(port))%5]*rest
}

// portBytes is what the n-th port has received and sent since the start: on
// average Bps bits a second while it runs, rxEighths of them received.
func (p planPersonality) portBytes(n int, elapsed time.Duration) (rx, tx int64) {
	port := p.Ports[n]
	if !port.Running || port.Bps <= 0 {
		return 0, 0
	}
	// bits = Bps × quarters/4 × ms/1000; bytes = bits/8.
	bitUnits := port.Bps * trafficUnits(n, elapsed)
	const perByte = 4 * 1000 * 8 * 8 // quarters, ms, bits, eighths
	return bitUnits * rxEighths / perByte, bitUnits * (8 - rxEighths) / perByte
}

// counterEvent is a drop or error counter that a port counts up during the
// hour: n more every per, from minute from to minute to.
type counterEvent struct {
	personality, port, counter string
	from, to                   int // minutes since the start
	n                          int64
	per                        time.Duration
}

// counterEvents: a congestion episode on the 48-port switch's uplink, a
// cable going bad on an access port of the 24-port one, a burst of wifi
// drops on the AP. Every other drop and error counter stays at zero.
var counterEvents = []counterEvent{
	{"switch48_clean", "sfp-sfpplus1", "tx-queue-drop", 20, 30, 7, time.Minute},
	{"switch24_clean", "ether3", "rx-error", 0, 60, 1, 7 * time.Minute},
	{"ap_clean", "wifi1", "tx-drop", 40, 45, 3, time.Minute},
}

// eventCount is the counter's value at elapsed (0 for a counter no event
// moves).
func (p planPersonality) eventCount(port, counter string, elapsed time.Duration) int64 {
	var total int64
	for _, ev := range counterEvents {
		if ev.personality != p.Name || ev.port != port || ev.counter != counter {
			continue
		}
		from, to := time.Duration(ev.from)*time.Minute, time.Duration(ev.to)*time.Minute
		if in := min(elapsed, to) - from; in > 0 {
			total += ev.n * int64(in/ev.per)
		}
	}
	return total
}

// errorCounters are the drop and error counters of the Tier 0 print.
var errorCounters = []string{"rx-error", "tx-error", "rx-drop", "tx-drop", "tx-queue-drop"}

// hasErrorCounters reports whether the n-th port carries them: the ether
// ports of an RB5009 do not (parseInterfaceStats), and the minute rows keep
// that as NULL, never as zero drops.
func (p planPersonality) hasErrorCounters(n int) bool {
	return !(strings.HasPrefix(p.Board, "RB5009") && p.Ports[n].Type == "ether")
}

// cpuWave is how a device's CPU load moves around its personality's value
// from one minute to the next (a cycle of six, shifted per device), and
// cpuBusy the stretch in which a C1 device is busy: never near the 80 % at
// which the hygiene audit stays away.
var cpuWave = [6]int{0, 2, 5, 3, 1, 4}

const (
	cpuBusyFrom, cpuBusyTo = 30, 35 // minutes
	cpuBusyExtra           = 30     // percent
)

// cpuAt is the device's cpu-load at elapsed: its personality's value, the
// wave, two more in the last third of each minute and one in the middle
// one, and the busy stretch on a C1 device.
func (p planPersonality) cpuAt(base int, elapsed time.Duration) int {
	minute := int(elapsed / time.Minute)
	second := int(elapsed % time.Minute / time.Second)
	cpu := base + cpuWave[(minute+int(p.nameHash()%6))%6] + second/20
	if p.Class == devclass.C1 && minute >= cpuBusyFrom && minute < cpuBusyTo {
		cpu += cpuBusyExtra
	}
	return min(cpu, 100)
}

// freeMemoryAt is the device's free memory at elapsed: its personality's
// value, less up to 15 pages of 64 KiB that come and go from minute to
// minute.
func (p planPersonality) freeMemoryAt(base int64, elapsed time.Duration) int64 {
	minute := int64(elapsed / time.Minute)
	return max(base-((minute*37+int64(p.nameHash()%16))%16)*65536, 0)
}

// formatUptime writes seconds the way RouterOS prints an uptime
// ("6w4d2h51m3s"); routeros.ParseUptime reads it back.
func formatUptime(seconds int64) string {
	w, rest := seconds/(7*86400), seconds%(7*86400)
	d, rest := rest/86400, rest%86400
	h, rest := rest/3600, rest%3600
	m, s := rest/60, rest%60
	switch {
	case w > 0:
		return fmt.Sprintf("%dw%dd%dh%dm%ds", w, d, h, m, s)
	case d > 0:
		return fmt.Sprintf("%dd%dh%dm%ds", d, h, m, s)
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// resourceAt is the device's /system/resource/print row at elapsed: the
// fixture's row with the CPU load, the free memory and the uptime moved on.
// An uptime the fixture does not give, or cannot be read, stays as it is.
func (p planPersonality) resourceAt(fixture map[string]string, elapsed time.Duration) map[string]string {
	row := maps.Clone(fixture)
	if base, err := strconv.Atoi(fixture["cpu-load"]); err == nil {
		row["cpu-load"] = strconv.Itoa(p.cpuAt(base, elapsed))
	}
	if base, err := strconv.ParseInt(fixture["free-memory"], 10, 64); err == nil {
		row["free-memory"] = strconv.FormatInt(p.freeMemoryAt(base, elapsed), 10)
	}
	if up, ok := routeros.ParseUptime(fixture["uptime"]); ok {
		row["uptime"] = formatUptime(up + int64(elapsed/time.Second))
	}
	return row
}

// animate makes the device's interface and resource prints answer what the
// device counts at the moment each one arrives, on the plan's clock.
func (d *planDevice) animate(now func() time.Time) {
	p := planPersonality{d.p}
	fixture := maps.Clone(p.Replies["/system/resource/print"].Rows[0])
	d.fake.OnCommand("/interface/print", func() {
		d.fake.SetReply("/interface/print", p.interfaceRowsAt(now().Sub(callPlanStart))...)
	})
	d.fake.OnCommand("/system/resource/print", func() {
		d.fake.SetReply("/system/resource/print", p.resourceAt(fixture, now().Sub(callPlanStart)))
	})
}
