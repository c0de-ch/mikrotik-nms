package fences

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const routerosLibrary = "github.com/go-routeros/routeros/v3"

// devicePackage is the one package tree that talks to devices.
const devicePackage = "internal/routeros"

// libraryImportsToRemove are the files outside internal/routeros that still
// import the RouterOS library — for its types only; none of them can open a
// session, the vendored library has no dialers. They go away as the Runner
// seam stops exposing library types. Nothing may be added here.
var libraryImportsToRemove = map[string]string{
	"internal/poller/firmware.go":                 "*ros.Client of a one-shot firmware session",
	"internal/poller/portstats_device.go":         "*ros.Reply in a read helper's signature",
	"internal/flow/ifsync.go":                     "*ros.Reply in a read helper's signature",
	"internal/hygiene/hygienetest/personality.go": "test support: scripts replies for the Runner seam",
}

// rawNetworkAllowed are the files outside internal/routeros that open a
// socket themselves, with what they talk to. A file that reaches a device
// does not belong here: it goes through internal/routeros.
var rawNetworkAllowed = map[string]string{
	"internal/flow/collector.go":    "the flow collector's UDP listener (MIKROTIK_NMS_FLOW_LISTEN), bound only with polling on",
	"internal/mailer/mailer.go":     "SMTP to the configured mail server",
	"internal/resolver/resolver.go": "DNS queries to the configured resolvers",
	"cmd/flowreplay/main.go":        "dev tool: sends synthetic flow datagrams to a collector",
}

// rawNetworkCalls are the selectors that open a connection or a datagram
// socket, per imported package path.
var rawNetworkCalls = map[string]map[string]bool{
	"net": {
		"Dial": true, "DialTimeout": true, "DialTCP": true, "DialUDP": true, "DialIP": true, "DialUnix": true,
		"Dialer": true, "ListenUDP": true, "ListenPacket": true, "ListenIP": true, "ListenMulticastUDP": true,
		"ListenConfig": true,
	},
	"crypto/tls": {"Dial": true, "DialWithDialer": true, "Dialer": true},
	"net/smtp":   {"Dial": true},
}

// sourceFile is one non-test Go file of the backend module.
type sourceFile struct {
	rel  string // slash-separated, relative to the module root
	file *ast.File
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("fences: no go.mod above the test directory")
		}
		dir = parent
	}
}

// sources parses every non-test Go file of the module. The vendored library
// (third_party, its own module) is not part of it.
func sources(t *testing.T) []sourceFile {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var out []sourceFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "third_party" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, sourceFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("fences: read the module: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("fences: only %d source files found under %s — the walk is broken", len(out), root)
	}
	return out
}

func inDevicePackage(rel string) bool {
	return strings.HasPrefix(rel, devicePackage+"/")
}

// importNames maps the local name of each import of f to its path.
func importNames(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if path == routerosLibrary {
			name = "routeros" // the package name of a /v3 module path
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = path
	}
	return names
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Only internal/routeros imports the RouterOS library.
func TestOnlyTheDevicePackageImportsTheRouterOSLibrary(t *testing.T) {
	found := map[string]bool{}
	for _, src := range sources(t) {
		if inDevicePackage(src.rel) {
			continue
		}
		for _, path := range importNames(src.file) {
			if path == routerosLibrary || strings.HasPrefix(path, routerosLibrary+"/") {
				found[src.rel] = true
			}
		}
	}
	for _, rel := range sorted(found) {
		if _, known := libraryImportsToRemove[rel]; !known {
			t.Errorf("%s imports %s. Only %s talks to devices: use its functions and the Runner seam "+
				"(routeros.Runner, routeros.ClientRunner) instead of the library's types.", rel, routerosLibrary, devicePackage)
		}
	}
	for rel := range libraryImportsToRemove {
		if !found[rel] {
			t.Errorf("%s no longer imports the RouterOS library: remove it from libraryImportsToRemove.", rel)
		}
	}
}

// Only internal/routeros opens connections to devices; the few other files
// that open a socket are listed with what they talk to.
func TestOnlyListedFilesOpenSocketsThemselves(t *testing.T) {
	found := map[string]map[string]bool{} // file → calls
	for _, src := range sources(t) {
		if inDevicePackage(src.rel) {
			continue
		}
		names := importNames(src.file)
		ast.Inspect(src.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if calls := rawNetworkCalls[names[pkg.Name]]; calls[sel.Sel.Name] {
				if found[src.rel] == nil {
					found[src.rel] = map[string]bool{}
				}
				found[src.rel][pkg.Name+"."+sel.Sel.Name] = true
			}
			return true
		})
	}
	files := make(map[string]bool, len(found))
	for rel := range found {
		files[rel] = true
	}
	for _, rel := range sorted(files) {
		if _, known := rawNetworkAllowed[rel]; !known {
			t.Errorf("%s opens a socket itself (%s). A device is reached through %s — Pool.Dial, DialOnce, Ping, "+
				"ScanSubnet, ScanMNDP — where the dial policy, deadlines and TLS pins apply. If this is not a device, "+
				"add the file to rawNetworkAllowed with what it talks to.", rel, strings.Join(sorted(found[rel]), ", "), devicePackage)
		}
	}
	for rel := range rawNetworkAllowed {
		if !files[rel] {
			t.Errorf("%s no longer opens a socket: remove it from rawNetworkAllowed.", rel)
		}
	}
}

// The check itself: a planted crossing is seen, whatever the import is
// called, and a selector on something that is not the package is not.
func TestTheSocketCheckSeesAPlantedCrossing(t *testing.T) {
	const planted = `package x

import (
	n "net"
	"crypto/tls"
)

type dialer struct{ Dial func() }

func f(d dialer) {
	_, _ = n.DialTimeout("tcp", "192.0.2.1:8728", 0)
	_, _ = tls.Dial("tcp", "192.0.2.1:8729", nil)
	_ = n.JoinHostPort("192.0.2.1", "8728") // not a socket
	d.Dial()                                // not the net package
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	names := importNames(f)
	var got []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && rawNetworkCalls[names[pkg.Name]][sel.Sel.Name] {
				got = append(got, names[pkg.Name]+"."+sel.Sel.Name)
			}
		}
		return true
	})
	if strings.Join(got, " ") != "net.DialTimeout crypto/tls.Dial" {
		t.Errorf("planted crossings seen = %v, want net.DialTimeout and crypto/tls.Dial only", got)
	}
}
