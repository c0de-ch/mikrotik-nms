package devclass

import "testing"

func TestClassifyFleetExamples(t *testing.T) {
	tests := []struct {
		name  string
		facts ResourceFacts
		want  Class
	}{
		{"never probed", ResourceFacts{}, C0Unknown},
		{"CRS354 (1-core 650 MHz MIPS, 64 MiB)",
			ResourceFacts{Architecture: "mipsbe", CPUCount: 1, CPUFrequencyMHz: 650, TotalMemoryBytes: 64 * mib, BoardName: "CRS354-48G-4S+2Q+"}, C1},
		{"PowerBox Pro (mipsbe, 128 MiB)",
			ResourceFacts{Architecture: "mipsbe", CPUCount: 1, CPUFrequencyMHz: 720, TotalMemoryBytes: 128 * mib, BoardName: "PowerBox Pro"}, C1},
		{"mAP lite (smips)",
			ResourceFacts{Architecture: "smips", CPUCount: 1, CPUFrequencyMHz: 650, TotalMemoryBytes: 64 * mib, BoardName: "mAP lite"}, C1},
		{"wAP R (smips)",
			ResourceFacts{Architecture: "smips", CPUCount: 1, CPUFrequencyMHz: 650, TotalMemoryBytes: 64 * mib, BoardName: "wAP R-2nD"}, C1},
		{"any board with 128 MiB is C1 whatever the CPU",
			ResourceFacts{Architecture: "arm", CPUCount: 4, CPUFrequencyMHz: 716, TotalMemoryBytes: 128 * mib, BoardName: "cAP ac"}, C1},
		{"CRS326 (1-core 800 MHz ARM, 512 MiB)",
			ResourceFacts{Architecture: "arm", CPUCount: 1, CPUFrequencyMHz: 800, TotalMemoryBytes: 512 * mib, BoardName: "CRS326-24G-2S+"}, C2},
		{"CRS328",
			ResourceFacts{Architecture: "arm", CPUCount: 1, CPUFrequencyMHz: 800, TotalMemoryBytes: 512 * mib, BoardName: "CRS328-24P-4S+"}, C2},
		{"CRS305 (98DX3236)",
			ResourceFacts{Architecture: "arm", CPUCount: 1, CPUFrequencyMHz: 800, TotalMemoryBytes: 512 * mib, BoardName: "CRS305-1G-4S+"}, C2},
		{"RB5009 (arm64, 4 cores)",
			ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 1400, TotalMemoryBytes: 1024 * mib, BoardName: "RB5009UG+S+"}, C3},
		{"CCR2004",
			ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 1700, TotalMemoryBytes: 4096 * mib, BoardName: "CCR2004-16G-2S+"}, C3},
		{"cAP ax (arm64, 4 cores at 864 MHz)",
			ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 864, TotalMemoryBytes: 256 * mib, BoardName: "cAP ax"}, C3},
		{"wAP ax",
			ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 864, TotalMemoryBytes: 256 * mib, BoardName: "wAP ax"}, C3},
		{"CHR (x86_64, cores from the host)",
			ResourceFacts{Architecture: "x86_64", CPUCount: 2, CPUFrequencyMHz: 2400, TotalMemoryBytes: 1024 * mib, BoardName: "CHR"}, C3},
		{"single-core CHR is only C2",
			ResourceFacts{Architecture: "x86_64", CPUCount: 1, CPUFrequencyMHz: 2400, TotalMemoryBytes: 1024 * mib, BoardName: "CHR"}, C2},
		{"unknown arm64 board, 2 cores at 1 GHz: C2 rule wins over C3",
			ResourceFacts{Architecture: "arm64", CPUCount: 2, CPUFrequencyMHz: 1000, TotalMemoryBytes: 256 * mib, BoardName: "Mystery"}, C2},
		{"unknown arm64 board, 2 cores above 1 GHz",
			ResourceFacts{Architecture: "arm64", CPUCount: 2, CPUFrequencyMHz: 1200, TotalMemoryBytes: 256 * mib, BoardName: "Mystery"}, C3},
		{"unknown architecture with many cores stays conservative",
			ResourceFacts{Architecture: "tile", CPUCount: 36, CPUFrequencyMHz: 1200, TotalMemoryBytes: 2048 * mib, BoardName: "Mystery"}, C2},
		{"board table supplies the nominal frequency when the print shows a scaled-down one",
			ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 300, TotalMemoryBytes: 1024 * mib, BoardName: "RB5009UG+S+"}, C3},
		{"board table supplies the architecture when the print lacks it",
			ResourceFacts{CPUCount: 1, TotalMemoryBytes: 512 * mib, BoardName: "crs326-24g-2s+"}, C2},
		{"only a board name, known as a 64 MiB MIPS unit",
			ResourceFacts{BoardName: "hAP lite"}, C1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.facts); got != tt.want {
				t.Fatalf("Classify(%+v) = %s, want %s", tt.facts, got, tt.want)
			}
		})
	}
}

func TestParseResourceFacts(t *testing.T) {
	f := ParseResourceFacts(map[string]string{
		"architecture-name": " arm64 ",
		"cpu-count":         "4",
		"cpu-frequency":     "1400",
		"total-memory":      "1073741824",
		"board-name":        "RB5009UG+S+",
	})
	want := ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 1400, TotalMemoryBytes: 1024 * mib, BoardName: "RB5009UG+S+"}
	if f != want {
		t.Fatalf("got %+v, want %+v", f, want)
	}
	if !ParseResourceFacts(map[string]string{"cpu-count": "x", "total-memory": "-1"}).Empty() {
		t.Fatal("malformed numbers must parse as empty facts")
	}
	if !ParseResourceFacts(nil).Empty() {
		t.Fatal("nil map must be empty facts")
	}
}

func TestParseClassAndLabels(t *testing.T) {
	for in, want := range map[string]Class{"c1": C1, "C2": C2, " c3 ": C3, "C0": C0Unknown} {
		got, ok := ParseClass(in)
		if !ok || got != want {
			t.Fatalf("ParseClass(%q) = %s,%v want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "C4", "weak", "1"} {
		if _, ok := ParseClass(in); ok {
			t.Fatalf("ParseClass(%q) must fail", in)
		}
	}
	if Class("").String() != "C0" || Class("").Label() != "unprobed" || C1.Label() != "weak" || C3.Label() != "capable" {
		t.Fatal("zero value is C0/unprobed; labels follow the class")
	}
	if C0Unknown.Budgeted() != C1 || Class("bogus").Budgeted() != C1 || C3.Budgeted() != C3 {
		t.Fatal("C0 and invalid classes are budgeted as C1")
	}
	if C0Unknown.Rank() != 1 || C1.Rank() != 1 || C2.Rank() != 2 || C3.Rank() != 3 {
		t.Fatal("rank order")
	}
}

func TestEffectiveOverrideOnlyGentler(t *testing.T) {
	tests := []struct {
		probed, override, want Class
		clamped                bool
	}{
		{C3, "", C3, false},
		{C3, C1, C1, false},
		{C3, C2, C2, false},
		{C3, C3, C3, false},
		{C2, C1, C1, false},
		{C2, C3, C2, true}, // more capable than probed: ignored and reported
		{C1, C2, C1, true},
		{C1, C3, C1, true},
		{C0Unknown, C1, C1, false}, // unprobed is budgeted as C1
		{C0Unknown, C2, C1, true},
		{C0Unknown, "", C1, false},
		{C2, "C0", C2, false}, // C0 is not an override
		{C2, "nonsense", C2, false},
		{"", C3, C1, true},
	}
	for _, tt := range tests {
		got, clamped := Effective(tt.probed, tt.override)
		if got != tt.want || clamped != tt.clamped {
			t.Errorf("Effective(%q, %q) = %s,%v want %s,%v", tt.probed, tt.override, got, clamped, tt.want, tt.clamped)
		}
	}
}

func TestLookupBoardIsCaseAndSpaceInsensitive(t *testing.T) {
	a, ok := LookupBoard("  CRS354-48G-4S+2Q+ ")
	b, ok2 := LookupBoard("crs354-48g-4s+2q+")
	if !ok || !ok2 || a != b || a.NominalMHz != 650 || a.Architecture != "mipsbe" {
		t.Fatalf("lookup: %+v %v / %+v %v", a, ok, b, ok2)
	}
	if _, ok := LookupBoard("not a board"); ok {
		t.Fatal("unknown board must not match")
	}
	if chr, ok := LookupBoard("CHR"); !ok || chr.Cores != 0 || chr.NominalMHz != 0 {
		t.Fatal("CHR has no fixed cores or frequency")
	}
}

func TestPhaseIsStable(t *testing.T) {
	if Phase("dev-a") != Phase("dev-a") {
		t.Fatal("phase must be deterministic")
	}
	if Phase("dev-a") == Phase("dev-b") {
		t.Fatal("different ids should (almost surely) differ")
	}
}
