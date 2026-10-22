package devclass

import "strings"

// BoardSpec is the nominal hardware of a RouterBOARD model: what the
// datasheet says, not what the device reports under frequency scaling.
// A zero field means "unknown, take the probed value".
type BoardSpec struct {
	Architecture string
	Cores        int
	NominalMHz   int
	MemoryBytes  int64
}

// boards maps a lower-cased board-name to its nominal spec. Fed from the
// fleet's anonymised /system/resource prints and the product pages; extend
// it as new models show up. Keys are matched after trimming and lower-casing
// the reported board-name.
var boards = map[string]BoardSpec{
	// Switches (CRS3xx: Marvell 98DX, one 800 MHz ARM core; CRS354: MIPS).
	"crs354-48g-4s+2q+": {Architecture: "mipsbe", Cores: 1, NominalMHz: 650, MemoryBytes: 64 * mib},
	"crs354-48p-4s+2q+": {Architecture: "mipsbe", Cores: 1, NominalMHz: 650, MemoryBytes: 64 * mib},
	"crs326-24g-2s+":    {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs326-24s+2q+":    {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs328-24p-4s+":    {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs328-4c-20s-4s+": {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs305-1g-4s+":     {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs310-8g+2s+":     {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 256 * mib},
	"crs310-1g-5s-4s+":  {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 256 * mib},
	"crs309-1g-8s+":     {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 512 * mib},
	"crs317-1g-16s+":    {Architecture: "arm", Cores: 1, NominalMHz: 800, MemoryBytes: 1024 * mib},

	// Routers.
	"rb5009ug+s+":         {Architecture: "arm64", Cores: 4, NominalMHz: 1400, MemoryBytes: 1024 * mib},
	"rb5009upr+s+":        {Architecture: "arm64", Cores: 4, NominalMHz: 1400, MemoryBytes: 1024 * mib},
	"ccr2004-16g-2s+":     {Architecture: "arm64", Cores: 4, NominalMHz: 1700, MemoryBytes: 4096 * mib},
	"ccr2004-1g-12s+2xs":  {Architecture: "arm64", Cores: 4, NominalMHz: 1700, MemoryBytes: 4096 * mib},
	"ccr2116-12g-4s+":     {Architecture: "arm64", Cores: 16, NominalMHz: 2000, MemoryBytes: 16384 * mib},
	"ccr2216-1g-12xs-2xq": {Architecture: "arm64", Cores: 16, NominalMHz: 2000, MemoryBytes: 16384 * mib},
	"rb4011igs+":          {Architecture: "arm", Cores: 4, NominalMHz: 1400, MemoryBytes: 1024 * mib},
	"rb4011igs+5hacq2hnd": {Architecture: "arm", Cores: 4, NominalMHz: 1400, MemoryBytes: 1024 * mib},
	"hex s":               {Architecture: "mmips", Cores: 4, NominalMHz: 880, MemoryBytes: 256 * mib},
	"hex":                 {Architecture: "mmips", Cores: 4, NominalMHz: 880, MemoryBytes: 256 * mib},

	// Access points (IPQ-6010: four Cortex-A53 cores at 864 MHz).
	"cap ax":  {Architecture: "arm64", Cores: 4, NominalMHz: 864, MemoryBytes: 256 * mib},
	"wap ax":  {Architecture: "arm64", Cores: 4, NominalMHz: 864, MemoryBytes: 256 * mib},
	"hap ax2": {Architecture: "arm64", Cores: 4, NominalMHz: 864, MemoryBytes: 1024 * mib},
	"hap ax3": {Architecture: "arm64", Cores: 4, NominalMHz: 1800, MemoryBytes: 1024 * mib},
	"cap ac":  {Architecture: "arm", Cores: 4, NominalMHz: 716, MemoryBytes: 128 * mib},
	"wap ac":  {Architecture: "arm", Cores: 4, NominalMHz: 716, MemoryBytes: 128 * mib},

	// Small MIPS units (QCA953x class).
	"hap lite":     {Architecture: "smips", Cores: 1, NominalMHz: 650, MemoryBytes: 32 * mib},
	"hap lite tc":  {Architecture: "smips", Cores: 1, NominalMHz: 650, MemoryBytes: 32 * mib},
	"map lite":     {Architecture: "smips", Cores: 1, NominalMHz: 650, MemoryBytes: 64 * mib},
	"wap r-2nd":    {Architecture: "smips", Cores: 1, NominalMHz: 650, MemoryBytes: 64 * mib},
	"powerbox pro": {Architecture: "mipsbe", Cores: 1, NominalMHz: 720, MemoryBytes: 128 * mib},
	"powerbox":     {Architecture: "smips", Cores: 1, NominalMHz: 650, MemoryBytes: 64 * mib},

	// Cloud Hosted Router: cores and frequency belong to the host, so only
	// the architecture is fixed; the probed values are used for the rest.
	"chr": {Architecture: "x86_64"},
}

// LookupBoard returns the nominal spec of a board-name as RouterOS reports
// it ("CRS354-48G-4S+2Q+", "cAP ax", "CHR"). ok is false for unknown boards.
func LookupBoard(boardName string) (BoardSpec, bool) {
	spec, ok := boards[strings.ToLower(strings.TrimSpace(boardName))]
	return spec, ok
}
