package fences

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// The seams of the collect/apply split (design 3.5; M1 converts one poller
// per PR): a collect half learns what to read from an edgeproto.ConfigSource,
// reaches a device only through the routeros.DeviceExec it is given and
// reports what it saw to an edgeproto.Sink; the apply half, internal/ingest,
// does the writing. These fences keep the pieces apart. Like the others they
// are ratchets: a new crossing fails, and so does a listed entry that is no
// longer true.

const modulePath = "github.com/mikrotik-nms/backend/"

// edgeprotoMayImport is all edgeproto imports of this module besides the
// standard library. It is shared by the edge, central and the gateway, and
// central must not link the device package.
var edgeprotoMayImport = map[string]string{
	"internal/devclass": "the tiers a DeviceSpec's cadence is keyed by (pure, in every binary)",
}

// ingestMayImport is all ingest imports of this module besides the standard
// library: it writes what observations say, and reaches no device and no
// network.
var ingestMayImport = map[string]string{
	"internal/edgeproto":        "the observations it applies",
	"internal/database/queries": "the query functions it applies them with",
}

// ingestImporters are the files outside internal/ingest that import it, with
// why: a collect half reports to its Sink and never applies anything itself.
var ingestImporters = map[string]string{
	"internal/poller/seams.go": "the monolith's seams: the in-process Sink applies through ingest",
}

// collectHalves are the files of the pollers split into a collect half. One
// is added with each converted poller; collectHalfCrossings says what none
// of them may do.
var collectHalves = []string{
	"internal/poller/client_discovery.go",
}

// collectHalfQueries are the names of internal/database/queries a collect
// half may use: pure helpers, which compute and touch no database.
var collectHalfQueries = map[string]string{
	"PreferIP": "the address a client merge keeps (IPv4 over IPv6) — the rule mac_lookup's upsert applies too",
}

// sessionNames are the names of internal/routeros that open or hold a device
// session, and sessionMethods the methods that hand one out: a collect half
// gets its link from DeviceExec and uses none of them.
var (
	sessionNames = map[string]bool{
		"Pool": true, "NewPool": true, "PoolExec": true, "ClientRunner": true, "RunCommand": true,
		"RunCommandWithTimeout": true, "DialOnce": true, "DialOnceWith": true, "Ping": true,
		"ScanSubnet": true, "ScanMNDP": true,
	}
	sessionMethods = map[string]bool{"GetLive": true, "EnsureConnection": true, "Dial": true}
)

// filesOf returns the non-test files of the package tree pkg.
func filesOf(t *testing.T, pkg string) []sourceFile {
	t.Helper()
	var out []sourceFile
	for _, src := range sources(t) {
		if strings.HasPrefix(src.rel, pkg+"/") {
			out = append(out, src)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no source files: the fence looks at nothing", pkg)
	}
	return out
}

func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// reachesTheNetwork reports a standard package that opens connections.
func reachesTheNetwork(path string) bool {
	return path == "net" || strings.HasPrefix(path, "net/") || path == "crypto/tls"
}

// importCrossings lists what the files of pkg import beyond the standard
// library (bar its network packages) and the module packages in allowed.
func importCrossings(t *testing.T, pkg string, allowed map[string]string) []string {
	t.Helper()
	var out []string
	for _, src := range filesOf(t, pkg) {
		for _, path := range importNames(src.file) {
			switch {
			case isStdlib(path):
				if reachesTheNetwork(path) {
					out = append(out, src.rel+" imports "+path)
				}
			case strings.HasPrefix(path, modulePath):
				if _, ok := allowed[strings.TrimPrefix(path, modulePath)]; !ok {
					out = append(out, src.rel+" imports "+path)
				}
			default:
				out = append(out, src.rel+" imports "+path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// edgeproto is the boundary both sides share: the standard library and
// devclass only.
func TestEdgeprotoImportsOnlyTheStandardLibraryAndDevclass(t *testing.T) {
	for _, c := range importCrossings(t, "internal/edgeproto", edgeprotoMayImport) {
		t.Errorf("%s. internal/edgeproto is shared by the edge, central and the gateway: it imports the standard "+
			"library (no network) and %v only — a seam that needs the device package belongs in internal/routeros, "+
			"as routeros.DeviceExec does.", c, sortedKeys(edgeprotoMayImport))
	}
}

// ingest writes what observations say: no device, no network, no collect
// half.
func TestIngestReachesNoDeviceAndNoNetwork(t *testing.T) {
	for _, c := range importCrossings(t, "internal/ingest", ingestMayImport) {
		t.Errorf("%s. internal/ingest is the apply half: it imports the standard library (no network) and %v only. "+
			"What it needs from a device or the network is the collect half's to read and report in an Observation.",
			c, sortedKeys(ingestMayImport))
	}
}

// Only the monolith's wiring of the seams imports ingest.
func TestOnlyTheMonolithWiringImportsIngest(t *testing.T) {
	const ingestPath = modulePath + "internal/ingest"
	found := map[string]bool{}
	for _, src := range sources(t) {
		if strings.HasPrefix(src.rel, "internal/ingest/") {
			continue
		}
		for _, path := range importNames(src.file) {
			if path == ingestPath || strings.HasPrefix(path, ingestPath+"/") {
				found[src.rel] = true
			}
		}
	}
	for _, rel := range sorted(found) {
		if _, known := ingestImporters[rel]; !known {
			t.Errorf("%s imports internal/ingest. A collect half reports what it saw to its edgeproto.Sink; only the "+
				"wiring that builds the in-process Sink applies through ingest. If this is such wiring, add the file to "+
				"ingestImporters with why.", rel)
		}
	}
	for rel := range ingestImporters {
		if !found[rel] {
			t.Errorf("%s no longer imports internal/ingest: remove it from ingestImporters.", rel)
		}
	}
}

// collectHalfCrossings lists what f does that a collect half must not, each
// as "<what>: <why>": hold a database handle, apply through ingest, use a
// name of internal/database/queries that is not a pure helper, or open or
// hold a device session itself.
func collectHalfCrossings(f *ast.File) []string {
	names := importNames(f)
	var out []string
	for _, path := range names {
		switch {
		case path == "database/sql":
			out = append(out, "imports database/sql: the collect half reads through its ConfigSource and writes through its Sink")
		case path == modulePath+"internal/ingest":
			out = append(out, "imports internal/ingest: the collect half reports to its Sink")
		case path == routerosLibrary || strings.HasPrefix(path, routerosLibrary+"/"):
			out = append(out, "imports "+path+": only internal/routeros talks to devices")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok {
				switch names[pkg.Name] {
				case modulePath + "internal/database/queries":
					if _, pure := collectHalfQueries[n.Sel.Name]; !pure {
						out = append(out, "uses queries."+n.Sel.Name+": a collect half reads through its ConfigSource and writes through its Sink")
					}
					return true
				case modulePath + "internal/routeros":
					if sessionNames[n.Sel.Name] {
						out = append(out, "uses routeros."+n.Sel.Name+": a collect half's device link is the routeros.DeviceExec it is given")
					}
					return true
				}
			}
			if sessionMethods[n.Sel.Name] {
				out = append(out, "calls ."+n.Sel.Name+": a collect half's device link is the routeros.DeviceExec it is given")
			}
		case *ast.Ident:
			if n.Name == "poolRunner" {
				out = append(out, "uses poolRunner: a collect half's device link is the routeros.DeviceExec it is given")
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// The pollers split into a collect half stay on their seams.
func TestCollectHalvesGoThroughTheSeams(t *testing.T) {
	byRel := map[string]sourceFile{}
	for _, src := range sources(t) {
		byRel[src.rel] = src
	}
	for _, rel := range collectHalves {
		src, ok := byRel[rel]
		if !ok {
			t.Errorf("%s is listed in collectHalves but is not a source file: fix the list.", rel)
			continue
		}
		for _, c := range collectHalfCrossings(src.file) {
			t.Errorf("%s %s.", rel, c)
		}
	}
}

// The check itself: a planted collect half that writes, holds the database
// and dials is seen on every count; the pure helper and the read helpers on
// the link it was given are not.
func TestTheCollectHalfCheckSeesAPlantedCrossing(t *testing.T) {
	const planted = `package poller

import (
	"database/sql"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/ingest"
	ros "github.com/mikrotik-nms/backend/internal/routeros"
)

type p struct {
	db   *sql.DB
	pool *ros.Pool
	exec ros.DeviceExec
}

func (x *p) poll() {
	_ = queries.UpsertMACLookup(x.db, nil)
	_ = queries.PreferIP("192.0.2.1", "")
	_ = ingest.NewSink(x.db)
	_ = x.pool.GetLive("d1")
	_ = poolRunner(x.pool, "d1")
	_, _ = ros.GetARPTable(x.exec.Device(nil, "d1"))
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range collectHalfCrossings(f) {
		what, _, _ := strings.Cut(c, ":")
		got = append(got, what)
	}
	want := []string{
		"calls .GetLive",
		"imports database/sql",
		"imports internal/ingest",
		"uses poolRunner",
		"uses queries.UpsertMACLookup",
		"uses routeros.Pool",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("planted crossings seen:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
