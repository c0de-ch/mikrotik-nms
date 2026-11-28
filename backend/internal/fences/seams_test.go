package fences

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"regexp"
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
//
// They read the source, they do not type-check it: what they see is what the
// files say — imports, the names a file uses, the types its structs declare.

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

// seamWiring are the files that declare or hold the seams without being a
// collect half: they build the seams and hand them to the collect halves.
var seamWiring = map[string]string{
	"internal/poller/seams.go":   "declares Seams and builds the monolith's (dbConfig, PoolExec, ingest.Sink)",
	"internal/poller/manager.go": "holds the monolith's Seams and hands them to each converted poller",
}

// collectHalves are the files of the pollers split into a collect half: every
// file that declares a collect-half type (a struct with a seam field — an
// edgeproto.Sink, an edgeproto.ConfigSource, a routeros.DeviceExec or the
// Seams) or a method of one. One poller is added with each conversion;
// collectHalfCrossings says what none of them may do.
var collectHalves = []string{
	"internal/poller/client_discovery.go",
}

// collectHalfFieldTypes are the field types a collect-half type may have, as
// written in its file, with why each holds neither the database nor a device
// session. A field of any other type fails: a wrapper that holds *sql.DB or
// the pool would carry them past every other check.
var collectHalfFieldTypes = map[string]string{
	"edgeproto.ConfigSource":     "the seam it learns its devices and settings from",
	"routeros.DeviceExec":        "the seam it reaches a device through",
	"edgeproto.Sink":             "the seam it reports to",
	"time.Duration":              "a period",
	"time.Time":                  "a time it keeps",
	"func() time.Time":           "its clock (a test seam)",
	"*tierClock":                 "the per-device tier schedule: in-memory slots, published to Schedule()",
	"[]leasesource.Lease":        "the external lease servers' latest leases, kept between their fetches",
	"func() []leasesource.Lease": "fetches the external lease servers' leases: how is the wiring's (Kea / OPNsense in the monolith)",
	// The one database read left in a collect half: the resolver reads the
	// dns_servers table itself on an uncached lookup. It goes when the
	// resolver takes its servers from the ConfigSource.
	"*resolver.Resolver": "reverse DNS of the clients — it still reads dns_servers through its own *sql.DB",
}

// collectHalfQueries are the names of internal/database/queries a collect
// half may use: pure helpers, which compute and touch no database.
var collectHalfQueries = map[string]string{
	"PreferIP": "the address a client merge keeps (IPv4 over IPv6) — the rule mac_lookup's upsert applies too",
}

// collectHalfForbidden are names of package poller a collect half must not
// use, with why: each one reaches the database or a device session around
// the seams.
var collectHalfForbidden = map[string]string{
	"poolRunner":     "a pooled session of its own: a collect half's device link is the routeros.DeviceExec it is given",
	"deviceRunner":   "the manager's pooled session: a collect half's device link is the routeros.DeviceExec it is given",
	"settingSeconds": "reads app_settings itself: a collect half reads its settings through ConfigSource.Setting",
	"Manager":        "the manager holds the database and the pool: a collect half is built from its Seams and what the wiring hands it",
}

// sessionNames are the names of internal/routeros that open or hold a device
// session (or open a socket), and sessionMethods the methods that hand one
// out: a collect half gets its link from DeviceExec and uses none of them.
var (
	sessionNames = map[string]bool{
		"Pool": true, "NewPool": true, "PoolExec": true, "ClientRunner": true, "OnceRunner": true,
		"RunCommand": true, "RunCommandWithTimeout": true, "DialOnce": true, "DialOnceWith": true,
		"Ping": true, "ScanSubnet": true, "ScanMNDP": true, "OutboundAddress": true,
	}
	sessionMethods = map[string]bool{"GetLive": true, "EnsureConnection": true, "Dial": true}
)

// socketPackages are the standard packages that open connections or run
// other programs; reachesTheNetwork also counts everything under net/http.
// net/netip, net/url, net/mail and net/textproto are pure and not listed.
var socketPackages = map[string]bool{
	"net": true, "net/http": true, "net/smtp": true, "net/rpc": true, "net/rpc/jsonrpc": true,
	"crypto/tls": true, "log/syslog": true, "os/exec": true,
}

// reachesTheNetwork reports a standard package that opens connections.
func reachesTheNetwork(path string) bool {
	return socketPackages[path] || strings.HasPrefix(path, "net/http/")
}

// filesOf returns the non-test files of the package pkg (its directory, not
// the packages below it).
func filesOf(t *testing.T, pkg string) []sourceFile {
	t.Helper()
	var out []sourceFile
	for _, src := range sources(t) {
		if path.Dir(src.rel) == pkg {
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

// linkCrossings walks every module package pkg links — its imports, theirs,
// and so on — and lists each import on the way that reaches a device or the
// network: internal/routeros, the RouterOS library or any other library
// outside the standard one, or a socket package of the standard library.
// Each crossing names the chain that links it, so that a package the direct
// imports allow cannot carry the device package in.
func linkCrossings(t *testing.T, pkg string) []string {
	t.Helper()
	files := sources(t)
	found := false
	for _, src := range files {
		if path.Dir(src.rel) == pkg {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s has no source files: the fence looks at nothing", pkg)
	}
	return linkCrossingsIn(files, pkg)
}

// linkCrossingsIn is linkCrossings over files.
func linkCrossingsIn(files []sourceFile, pkg string) []string {
	byDir := map[string][]sourceFile{}
	for _, src := range files {
		dir := path.Dir(src.rel)
		byDir[dir] = append(byDir[dir], src)
	}
	chain := map[string]string{pkg: pkg} // package → how pkg reaches it
	queue := []string{pkg}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, src := range byDir[cur] {
			for _, imp := range importNames(src.file) {
				via := chain[cur] + " → "
				switch {
				case isStdlib(imp):
					if reachesTheNetwork(imp) {
						out = append(out, fmt.Sprintf("%s%s (%s)", via, imp, src.rel))
					}
				case strings.HasPrefix(imp, modulePath):
					dep := strings.TrimPrefix(imp, modulePath)
					if dep == devicePackage || strings.HasPrefix(dep, devicePackage+"/") {
						out = append(out, fmt.Sprintf("%s%s (%s)", via, dep, src.rel))
						continue
					}
					if _, seen := chain[dep]; !seen {
						chain[dep] = chain[cur] + " → " + dep
						queue = append(queue, dep)
					}
				default:
					out = append(out, fmt.Sprintf("%s%s (%s)", via, imp, src.rel))
				}
			}
		}
	}
	sort.Strings(out)
	return slicesCompact(out)
}

func slicesCompact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// edgeproto is the boundary both sides share: the standard library and
// devclass only, and nothing it links reaches a device or the network.
func TestEdgeprotoImportsOnlyTheStandardLibraryAndDevclass(t *testing.T) {
	for _, c := range importCrossings(t, "internal/edgeproto", edgeprotoMayImport) {
		t.Errorf("%s. internal/edgeproto is shared by the edge, central and the gateway: it imports the standard "+
			"library (no network) and %v only — a seam that needs the device package belongs in internal/routeros, "+
			"as routeros.DeviceExec does.", c, sortedKeys(edgeprotoMayImport))
	}
	for _, c := range linkCrossings(t, "internal/edgeproto") {
		t.Errorf("internal/edgeproto links %s. Nothing edgeproto links may reach a device or the network: central "+
			"links edgeproto without the device package.", c)
	}
}

// ingest writes what observations say: no device, no network, no collect
// half — neither in its own imports nor in anything they link.
func TestIngestReachesNoDeviceAndNoNetwork(t *testing.T) {
	for _, c := range importCrossings(t, "internal/ingest", ingestMayImport) {
		t.Errorf("%s. internal/ingest is the apply half: it imports the standard library (no network) and %v only. "+
			"What it needs from a device or the network is the collect half's to read and report in an Observation.",
			c, sortedKeys(ingestMayImport))
	}
	for _, c := range linkCrossings(t, "internal/ingest") {
		t.Errorf("internal/ingest links %s. The apply half is central's: nothing it links may reach a device or the "+
			"network. A type the database stores that a device read produces belongs in internal/edgeproto "+
			"(routeros keeps an alias), as edgeproto.TracerouteHop does.", c)
	}
}

// The transitive walk sees a device package two imports away, and a socket
// package of the standard library, but not net/netip.
func TestTheLinkCheckSeesACrossingTwoImportsAway(t *testing.T) {
	const lib = `package lib

import (
	"net/netip"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

var _ netip.Addr
var _ routeros.Runner
`
	const top = `package top

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/lib"
)

var _ = http.MethodGet
var _ = lib.X
`
	parse := func(name, src string) sourceFile {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		return sourceFile{rel: name, file: f}
	}
	planted := []sourceFile{parse("internal/top/top.go", top), parse("internal/lib/lib.go", lib)}
	got := linkCrossingsIn(planted, "internal/top")
	want := []string{
		"internal/top → internal/lib → internal/routeros (internal/lib/lib.go)",
		"internal/top → net/http (internal/top/top.go)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("planted link crossings seen:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
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

// seamField reports whether a field of type expr (in a file with the import
// names names) is a seam: an edgeproto.Sink or ConfigSource, a
// routeros.DeviceExec, or package poller's Seams.
func seamField(expr ast.Expr, names map[string]string) bool {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return seamField(e.X, names)
	case *ast.Ident:
		return e.Name == "Seams"
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch names[pkg.Name] {
		case modulePath + "internal/edgeproto":
			return e.Sel.Name == "Sink" || e.Sel.Name == "ConfigSource"
		case modulePath + "internal/routeros":
			return e.Sel.Name == "DeviceExec"
		case modulePath + "internal/poller":
			return e.Sel.Name == "Seams"
		}
	}
	return false
}

// collectHalfTypes returns the struct types f declares that have a seam
// field: the collect halves (or, in seamWiring, what builds them).
func collectHalfTypes(f *ast.File) map[string]*ast.StructType {
	names := importNames(f)
	out := map[string]*ast.StructType{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if seamField(field.Type, names) {
					out[ts.Name.Name] = st
					break
				}
			}
		}
	}
	return out
}

// receiverType is the name of the type a method is declared on, "" for a
// function.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if idx, ok := expr.(*ast.IndexExpr); ok { // a generic receiver
		expr = idx.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// canonicalType rewrites the import names of typ (types.ExprString of a
// field's type) to the packages' own names, so that ros.DeviceExec reads
// routeros.DeviceExec whatever the file calls the import.
func canonicalType(typ string, names map[string]string) string {
	for local, imp := range names {
		own := imp[strings.LastIndex(imp, "/")+1:]
		if imp == routerosLibrary {
			own = "routeros"
		}
		if local == own || local == "_" || local == "." {
			continue
		}
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(local) + `\.`)
		typ = re.ReplaceAllString(typ, "${1}"+own+".")
	}
	return typ
}

// fieldCrossings lists the fields of the collect-half types of f whose type
// collectHalfFieldTypes does not allow, and records in used the allowed ones
// it saw.
func fieldCrossings(f *ast.File, used map[string]bool) []string {
	names := importNames(f)
	var out []string
	for name, st := range collectHalfTypes(f) {
		for _, field := range st.Fields.List {
			typ := canonicalType(types.ExprString(field.Type), names)
			fieldNames := "(embedded)"
			if len(field.Names) > 0 {
				var ns []string
				for _, n := range field.Names {
					ns = append(ns, n.Name)
				}
				fieldNames = strings.Join(ns, ", ")
			}
			if _, ok := collectHalfFieldTypes[typ]; ok {
				used[typ] = true
				continue
			}
			out = append(out, fmt.Sprintf("has field %s.%s of type %s: a collect-half type holds only its seams and "+
				"what collectHalfFieldTypes allows — a value that holds the database or a session would carry them past "+
				"every other check. If this type holds neither, add it there with why", name, fieldNames, typ))
		}
	}
	return out
}

// collectHalfCrossings lists what f does that a collect half must not, each
// as "<what>: <why>": hold a database handle, apply through ingest, use a
// name of internal/database/queries that is not a pure helper, open or hold
// a device session itself, or use a name of package poller that does one of
// these. used records the allowed names it saw.
func collectHalfCrossings(f *ast.File, used map[string]bool) []string {
	names := importNames(f)
	out := fieldCrossings(f, used)
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
					if _, pure := collectHalfQueries[n.Sel.Name]; pure {
						used["queries."+n.Sel.Name] = true
					} else {
						out = append(out, "uses queries."+n.Sel.Name+": a collect half reads through its ConfigSource and writes through its Sink")
					}
					return false
				case modulePath + "internal/routeros":
					if sessionNames[n.Sel.Name] {
						out = append(out, "uses routeros."+n.Sel.Name+": a collect half's device link is the routeros.DeviceExec it is given")
					}
					return false
				}
			}
			if sessionMethods[n.Sel.Name] {
				out = append(out, "calls ."+n.Sel.Name+": a collect half's device link is the routeros.DeviceExec it is given")
			}
		case *ast.Ident:
			if why, ok := collectHalfForbidden[n.Name]; ok {
				out = append(out, "uses "+n.Name+": "+why)
			}
		}
		return true
	})
	sort.Strings(out)
	return slicesCompact(out)
}

// The pollers split into a collect half stay on their seams: every file
// that declares a collect-half type or one of its methods is listed, and
// none of them crosses.
func TestCollectHalvesGoThroughTheSeams(t *testing.T) {
	listed := map[string]bool{}
	for _, rel := range collectHalves {
		listed[rel] = true
	}
	byRel := map[string]sourceFile{}
	halfTypes := map[string]map[string]bool{} // package dir → collect-half type names
	wiringSeen := map[string]bool{}
	for _, src := range sources(t) {
		byRel[src.rel] = src
		declared := collectHalfTypes(src.file)
		if len(declared) == 0 {
			continue
		}
		if _, wiring := seamWiring[src.rel]; wiring {
			wiringSeen[src.rel] = true
			continue
		}
		dir := path.Dir(src.rel)
		if halfTypes[dir] == nil {
			halfTypes[dir] = map[string]bool{}
		}
		for name := range declared {
			halfTypes[dir][name] = true
		}
		if !listed[src.rel] {
			t.Errorf("%s declares %v, a struct with a seam field, but is not in collectHalves: a poller on the "+
				"seams is fenced only once each of its files is listed. If the file only builds the seams, add it to "+
				"seamWiring with why.", src.rel, sortedKeys(toReasons(declared)))
		}
	}
	for rel := range seamWiring {
		if !wiringSeen[rel] {
			t.Errorf("%s declares no struct with a seam field any more: remove it from seamWiring.", rel)
		}
	}
	// A method of a collect-half type is part of it, wherever it is declared.
	for _, src := range sources(t) {
		halves := halfTypes[path.Dir(src.rel)]
		if len(halves) == 0 || listed[src.rel] {
			continue
		}
		for _, decl := range src.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && halves[receiverType(fn)] {
				t.Errorf("%s declares (%s).%s, a method of a collect half: add the file to collectHalves.",
					src.rel, receiverType(fn), fn.Name.Name)
			}
		}
	}
	used := map[string]bool{}
	for _, rel := range collectHalves {
		src, ok := byRel[rel]
		if !ok {
			t.Errorf("%s is listed in collectHalves but is not a source file: fix the list.", rel)
			continue
		}
		for _, c := range collectHalfCrossings(src.file, used) {
			t.Errorf("%s %s.", rel, c)
		}
	}
	for typ := range collectHalfFieldTypes {
		if !used[typ] {
			t.Errorf("no collect half has a field of type %s any more: remove it from collectHalfFieldTypes.", typ)
		}
	}
	for name := range collectHalfQueries {
		if !used["queries."+name] {
			t.Errorf("no collect half uses queries.%s any more: remove it from collectHalfQueries.", name)
		}
	}
}

// Every name the collect-half check looks for exists, so a renamed session
// function is not silently let through.
func TestTheCollectHalfCheckNamesWhatExists(t *testing.T) {
	declared := map[string]map[string]bool{} // package dir → top-level names
	for _, src := range sources(t) {
		dir := path.Dir(src.rel)
		if declared[dir] == nil {
			declared[dir] = map[string]bool{}
		}
		for _, decl := range src.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if rt := receiverType(d); rt != "" {
					declared[dir]["."+d.Name.Name] = true
				} else {
					declared[dir][d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						declared[dir][s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							declared[dir][n.Name] = true
						}
					}
				}
			}
		}
	}
	for name := range sessionNames {
		if !declared[devicePackage][name] {
			t.Errorf("sessionNames lists routeros.%s, which internal/routeros does not declare: fix the list.", name)
		}
	}
	for name := range sessionMethods {
		if !declared[devicePackage]["."+name] {
			t.Errorf("sessionMethods lists .%s, a method internal/routeros does not declare: fix the list.", name)
		}
	}
	for name := range collectHalfForbidden {
		if !declared["internal/poller"][name] && !declared["internal/poller"]["."+name] {
			t.Errorf("collectHalfForbidden lists %s, which internal/poller does not declare: fix the list.", name)
		}
	}
	for name := range collectHalfQueries {
		if !declared["internal/database/queries"][name] {
			t.Errorf("collectHalfQueries lists queries.%s, which internal/database/queries does not declare: fix the list.", name)
		}
	}
}

// The check itself: a planted collect half that uses every name the check
// looks for — each session name and method, each forbidden name, a library
// import, a field that holds the database — is seen on every count; the pure
// helper, the allowed fields and the read helpers on the link it was given
// are not.
func TestTheCollectHalfCheckSeesAPlantedCrossing(t *testing.T) {
	var body strings.Builder
	want := []string{
		"has field p.db of type *sql.DB",
		"has field p.mgr of type *Manager",
		"has field p.pool of type *routeros.Pool",
		"imports database/sql",
		"imports internal/ingest",
		"imports " + routerosLibrary,
		"uses queries.UpsertMACLookup",
	}
	for _, name := range sorted(sessionNames) {
		fmt.Fprintf(&body, "\t_ = ros.%s\n", name)
		want = append(want, "uses routeros."+name)
	}
	for _, name := range sorted(sessionMethods) {
		fmt.Fprintf(&body, "\t_ = x.mgr.%s()\n", name)
		want = append(want, "calls ."+name)
	}
	for _, name := range sortedKeys(collectHalfForbidden) {
		fmt.Fprintf(&body, "\t_ = %s\n", name)
		want = append(want, "uses "+name)
	}
	planted := `package poller

import (
	"database/sql"
	"time"

	lib "` + routerosLibrary + `"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
	"github.com/mikrotik-nms/backend/internal/ingest"
	ros "github.com/mikrotik-nms/backend/internal/routeros"
)

type p struct {
	sink     edgeproto.Sink
	exec     ros.DeviceExec
	interval time.Duration
	db       *sql.DB
	pool     *ros.Pool
	mgr      *Manager
}

var _ *lib.Client

func (x *p) poll() {
	_ = queries.UpsertMACLookup(x.db, nil)
	_ = queries.PreferIP("192.0.2.1", "")
	_ = ingest.NewSink(x.db)
	_, _ = ros.GetARPTable(x.exec.Device(nil, "d1"))
` + body.String() + `}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range collectHalfCrossings(f, map[string]bool{}) {
		what, _, _ := strings.Cut(c, ":")
		got = append(got, what)
	}
	sort.Strings(want)
	// "uses Manager" is seen once, though the planted file names it twice.
	want = slicesCompact(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("planted crossings seen:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func toReasons(m map[string]*ast.StructType) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = ""
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
