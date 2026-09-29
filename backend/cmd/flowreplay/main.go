// Command flowreplay sends synthetic lab flow exports (RouterOS IPFIX,
// OPNsense NetFlow v9, an unknown sender, garbage) or replays captured
// datagrams to a flow collector, from distinct loopback source addresses so
// the collector sees distinct exporters. Development and end-to-end testing
// only — it never talks to real devices.
//
//	go run ./cmd/flowreplay -to 127.0.0.1:12055 [-scenario lab|ros|opn|unknown|garbage] \
//	    [-ros-src 127.0.0.2] [-opn-src 127.0.0.3] [-unknown-src 127.0.0.4] [-minutes 5] [-every 10s] \
//	    [-seed 1] [-skip-templates] [-mix] [-dir <capture dir>]
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

type sender struct {
	name string
	conn *net.UDPConn
	to   *net.UDPAddr
	sent int // this minute
	all  int
}

func newSender(name, src string, to *net.UDPAddr) (*sender, error) {
	ip := net.ParseIP(src)
	if ip == nil {
		return nil, fmt.Errorf("%s: invalid source address %q", name, src)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, fmt.Errorf("%s: bind %s: %w", name, src, err)
	}
	return &sender{name: name, conn: conn, to: to}, nil
}

func (s *sender) send(b []byte) {
	if _, err := s.conn.WriteToUDP(b, s.to); err != nil {
		log.Printf("%s: send: %v", s.name, err)
		return
	}
	s.sent++
	s.all++
}

// jitter scales v by a deterministic factor in [0.8, 1.2].
func jitter(rng *rand.Rand, v uint64) uint64 {
	return max(uint64(float64(v)*(0.8+0.4*rng.Float64())), 1)
}

type replay struct {
	rng           *rand.Rand
	to            *net.UDPAddr
	skipTemplates bool
	mix           bool          // add the lab host mix (mix.go)
	every         time.Duration // data interval (the mix's flow window)
	start         time.Time

	ros, opn, unknown, garbage *sender
	rosSeq, opnSeq, unkSeq     uint32
	rosSysInit                 uint64
	opnUptime0                 uint32
}

func (r *replay) rosTemplates(now time.Time) {
	if r.ros == nil || r.skipTemplates {
		return
	}
	r.ros.send(pktgen.RouterOSIPFIXTemplates(uint32(now.Unix()), r.rosSeq, 0, true))
}

func (r *replay) rosData(now time.Time) {
	if r.ros == nil {
		return
	}
	flows := pktgen.LabRouterOS(now, r.rosSysInit, true, r.ros.conn.LocalAddr().(*net.UDPAddr).IP.String(),
		r.to.IP.String(), uint16(r.to.Port))
	for i := range flows {
		if i == 0 {
			flows[i].Bytes, flows[i].Packets = 5_000_000, 3_500 // R1 at a realistic ~5 MB
		}
		flows[i].Bytes, flows[i].Packets = jitter(r.rng, flows[i].Bytes), jitter(r.rng, flows[i].Packets)
	}
	if r.mix {
		flows = append(flows, mixRouterOS(r.rng, now, r.every, r.rosSysInit)...)
	}
	for _, c := range chunk(flows, 14) { // ≤ 14 × 85 B records: one Ethernet MTU
		r.ros.send(pktgen.RouterOSIPFIXData(uint32(now.Unix()), r.rosSeq, 0, c...))
		r.rosSeq += uint32(len(c)) // IPFIX sequence = data records sent
	}
}

func (r *replay) opnUptime(now time.Time) uint32 {
	return r.opnUptime0 + uint32(now.Sub(r.start).Milliseconds())
}

func (r *replay) opnTemplates(now time.Time) {
	if r.opn == nil || r.skipTemplates {
		return
	}
	r.opnSeq++
	r.opn.send(pktgen.OPNsenseV9Templates(uint32(now.Unix()), r.opnUptime(now), r.opnSeq))
}

func (r *replay) opnData(now time.Time) {
	if r.opn == nil {
		return
	}
	up := r.opnUptime(now)
	flows := pktgen.LabOPNsense(up, r.opn.conn.LocalAddr().(*net.UDPAddr).IP.String(), r.to.IP.String(), uint16(r.to.Port))
	for i := 0; i < 3; i++ { // O1–O3; the self-export record stays exact
		flows[i].Bytes, flows[i].Packets = uint32(jitter(r.rng, uint64(flows[i].Bytes))), uint32(jitter(r.rng, uint64(flows[i].Packets)))
	}
	flows = append(flows, flows[0]) // an exact duplicate of O1 (the collector's dup filter drops it)
	r.opnSeq++
	r.opn.send(pktgen.OPNsenseV9Data(uint32(now.Unix()), up, r.opnSeq, flows...))
	if r.mix {
		for _, c := range chunk(mixOPNsense(r.rng, up, r.every), 20) { // ≤ 20 × 57 B records per datagram
			r.opnSeq++
			r.opn.send(pktgen.OPNsenseV9Data(uint32(now.Unix()), up, r.opnSeq, c...))
		}
	}
}

func (r *replay) unknownDatagram(now time.Time) {
	if r.unknown == nil {
		return
	}
	r.unkSeq++
	r.unknown.send(pktgen.OPNsenseV9Templates(uint32(now.Unix()), 1000+uint32(now.Sub(r.start).Milliseconds()), r.unkSeq))
}

func (r *replay) garbageDatagrams(now time.Time) {
	if r.garbage == nil {
		return
	}
	junk := make([]byte, 64+r.rng.IntN(512))
	for i := range junk {
		junk[i] = byte(r.rng.UintN(256))
	}
	junk[0], junk[1] = 0x45, 0 // never a valid version
	r.garbage.send(junk)
	r.garbage.send(pktgen.V5(uint32(now.Unix()), 1_000_000, 30, 1)) // truncated v5
	hdr := pktgen.EthIPv4TCPHeader(28, "192.168.28.25", "1.1.1.1", 50514, 443, 1500)
	sf := pktgen.SFlowSwitch("192.168.78.201", 1, 512, 1, 1, 2, 1518, hdr)
	r.garbage.send(sf[:len(sf)/2]) // truncated sFlow
	r.garbage.send(pktgen.SFlowExpandedHugeCount())
	r.garbage.send(pktgen.IPFIXZeroFieldTemplateThenData())
}

func (r *replay) summary(now time.Time) {
	var parts []string
	for _, s := range []*sender{r.ros, r.opn, r.unknown, r.garbage} {
		if s == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", s.name, s.sent))
		s.sent = 0
	}
	fmt.Printf("%s sent: %s\n", now.Format("15:04:05"), strings.Join(parts, " "))
}

// replayDir sends every <address>-<unixnano>.bin capture in name order from
// the address in its file name.
func replayDir(dir string, to *net.UDPAddr, gap time.Duration) error {
	names, err := filepath.Glob(filepath.Join(dir, "*.bin"))
	if err != nil {
		return err
	}
	slices.Sort(names)
	senders := map[string]*sender{}
	for _, name := range names {
		base := strings.TrimSuffix(filepath.Base(name), ".bin")
		i := strings.LastIndexByte(base, '-')
		if i <= 0 {
			log.Printf("skip %s: no <address>-<unixnano> name", name)
			continue
		}
		addr, err := netip.ParseAddr(base[:i])
		if err != nil {
			log.Printf("skip %s: %v", name, err)
			continue
		}
		s := senders[addr.String()]
		if s == nil {
			if s, err = newSender(addr.String(), addr.String(), to); err != nil {
				return err
			}
			senders[addr.String()] = s
		}
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		s.send(b)
		time.Sleep(gap)
	}
	for a, s := range senders {
		fmt.Printf("replayed %d datagrams from %s\n", s.all, a)
		s.conn.Close()
	}
	return nil
}

func main() {
	to := flag.String("to", "127.0.0.1:12055", "collector address")
	scenario := flag.String("scenario", "lab", "lab | ros | opn | unknown | garbage")
	rosSrc := flag.String("ros-src", "127.0.0.2", "source address of the RouterOS (IPFIX) exporter")
	opnSrc := flag.String("opn-src", "127.0.0.3", "source address of the OPNsense (NetFlow v9) exporter")
	unkSrc := flag.String("unknown-src", "127.0.0.4", "source address of the unknown sender")
	minutes := flag.Float64("minutes", 5, "run time in minutes")
	every := flag.Duration("every", 10*time.Second, "data interval")
	seed := flag.Uint64("seed", 1, "random seed (volumes are scaled ±20 %)")
	skipTemplates := flag.Bool("skip-templates", false, "never send template sets (restart / persistence test)")
	mix := flag.Bool("mix", false, "add a multi-host mix of lab hosts talking to common internet services (ros/opn)")
	dir := flag.String("dir", "", "replay *.bin captures from this directory instead")
	flag.Parse()

	dst, err := net.ResolveUDPAddr("udp4", *to)
	if err != nil {
		log.Fatalf("-to: %v", err)
	}
	if *dir != "" {
		if err := replayDir(*dir, dst, 20*time.Millisecond); err != nil {
			log.Fatal(err)
		}
		return
	}

	now := time.Now()
	r := &replay{rng: rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15)), to: dst, skipTemplates: *skipTemplates,
		mix: *mix, every: *every, start: now, rosSeq: 1, rosSysInit: uint64(now.Add(-72 * time.Hour).UnixMilli()), opnUptime0: 500_000_000}
	want := map[string]bool{}
	switch *scenario {
	case "lab":
		want["ros"], want["opn"], want["unknown"] = true, true, true
	case "ros", "opn", "unknown", "garbage":
		want[*scenario] = true
	default:
		log.Fatalf("-scenario %q: want lab, ros, opn, unknown or garbage", *scenario)
	}
	mk := func(name, src string) *sender {
		s, err := newSender(name, src, dst)
		if err != nil {
			log.Fatal(err)
		}
		return s
	}
	if want["ros"] {
		r.ros = mk("ros", *rosSrc)
	}
	if want["opn"] {
		r.opn = mk("opn", *opnSrc)
	}
	if want["unknown"] {
		r.unknown = mk("unknown", *unkSrc)
	}
	if want["garbage"] {
		r.garbage = mk("garbage", *rosSrc)
	}

	// Start: RouterOS templates first; OPNsense data BEFORE its templates
	// (template-after-data), templates 5 s later.
	r.rosTemplates(now)
	r.rosData(now)
	r.opnData(now)
	r.unknownDatagram(now)
	r.garbageDatagrams(now)

	end := now.Add(time.Duration(*minutes * float64(time.Minute)))
	dataT := time.NewTicker(*every)
	minuteT := time.NewTicker(time.Minute)
	opnFirstTemplates := time.NewTimer(5 * time.Second)
	defer dataT.Stop()
	defer minuteT.Stop()
	for {
		select {
		case now := <-dataT.C:
			if now.After(end) {
				r.summary(now)
				return
			}
			r.rosData(now)
			r.opnData(now)
			r.garbageDatagrams(now)
		case now := <-opnFirstTemplates.C:
			r.opnTemplates(now)
		case now := <-minuteT.C:
			r.rosTemplates(now)
			r.opnTemplates(now)
			r.unknownDatagram(now)
			r.summary(now)
		}
	}
}
