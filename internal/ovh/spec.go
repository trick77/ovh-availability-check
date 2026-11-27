package ovh

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Disk is one group of identical disks in a storage spec: "4x2000sa" is 4 disks of 2000 GB, hdd.
type Disk struct {
	Count  int
	SizeGB int
	Kind   string // nvme, ssd or hdd
}

func (d Disk) String() string {
	return fmt.Sprintf("%d×%s %s", d.Count, size(d.SizeGB), strings.ToUpper(d.Kind))
}

func size(gb int) string {
	if gb >= 1000 && gb%1000 == 0 {
		return strconv.Itoa(gb/1000) + "TB"
	}
	if gb >= 1000 {
		return strconv.FormatFloat(float64(gb)/1000, 'f', 2, 64) + "TB"
	}
	return strconv.Itoa(gb) + "GB"
}

var (
	ramRE = regexp.MustCompile(`^ram-(\d+)g-`)
	// diskRE covers every spec the API listed (2026-11): "2x450nvme", "4x2000sa", "12x3840sas", "2x960ssd",
	// "16x24000-hdd-sas", "2x960nvme-pcie-gen5". "sas" before "sa": the alternation takes the first that fits.
	diskRE = regexp.MustCompile(`(\d+)x(\d+)(?:-hdd)?-?(nvme|ssd|sas|sa)`)
)

// ParseRAM reads the GB out of a memory spec ("ram-64g-ecc-2133" → 64); 0 when it has none.
func ParseRAM(memory string) int {
	m := ramRE.FindStringSubmatch(memory)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ParseDisks reads every disk group out of a storage spec. Hybrid specs ("hybridsoftraid-2x4000sa-2x450nvme")
// give one group per kind; "noraid-0" gives none.
func ParseDisks(storage string) []Disk {
	var out []Disk
	for _, m := range diskRE.FindAllStringSubmatch(storage, -1) {
		count, _ := strconv.Atoi(m[1])
		sz, _ := strconv.Atoi(m[2])
		kind := m[3]
		if kind == "sa" || kind == "sas" {
			kind = "hdd"
		}
		out = append(out, Disk{Count: count, SizeGB: sz, Kind: kind})
	}
	return out
}
