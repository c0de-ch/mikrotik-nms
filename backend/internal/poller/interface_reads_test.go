package poller

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The one-interface-read contract: the port-stats collector's Tier 0 print
// is the only scheduled /interface/print (and its Tier 4a print the only
// /interface/ethernet/print); inventory, the port monitor, uplinks and the
// flow ifIndex sync read its snapshot. This guard fails when an interface
// read appears anywhere else, so a new one is a deliberate, documented
// exception rather than a regression of the device load.
func TestInterfaceTableReadOnlyByTier0(t *testing.T) {
	allowed := map[string][]string{
		// Tier 0 and Tier 4a themselves.
		"poller/portstats.go": {"routeros.GetInterfaceStats(", "routeros.GetEthernetStats("},
		// Identity check of a device answering at a candidate address,
		// on the verify connection: no snapshot covers it.
		"poller/ip_follow.go": {"routeros.GetInterfaces("},
		// The ifIndex sync's fallback while no snapshot exists yet.
		"flow/ifsync.go": {"routeros.GetInterfaceStats("},
	}
	needles := []string{
		"routeros.GetInterfaceStats(", "routeros.GetEthernetStats(", "routeros.GetInterfaces(",
		`"/interface/print"`, `"/interface/ethernet/print"`,
	}

	internal, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{internal, filepath.Join(internal, "..", "cmd")}
	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == filepath.Join(internal, "routeros") {
					return filepath.SkipDir // where the readers are defined
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(internal, path)
			rel = filepath.ToSlash(rel)
			checked++
			for _, n := range needles {
				if strings.Contains(string(src), n) && !slices.Contains(allowed[rel], n) {
					t.Errorf("%s reads the interface table (%s): use the port-stats snapshot (DeviceSnapshot) instead", rel, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files checked: the walk is not looking where the code is", checked)
	}
}
