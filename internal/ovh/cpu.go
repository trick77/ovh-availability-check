package ovh

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
)

// cpus.json is PassMark's data (cpubenchmark.net) for the CPUs the catalogs name, looked up by hand: OVH publishes no
// core count or benchmark. A CPU OVH adds later is missing until it is added there.
//
//go:embed cpus.json
var cpusJSON []byte

// CPUInfo is one CPU's PassMark data. Mark is the multithread CPU Mark, Single the single thread rating; 0 when
// PassMark has no figure.
type CPUInfo struct {
	Cores    int    `json:"cores"`
	Threads  int    `json:"threads"`
	Mark     int    `json:"mark"`
	Single   int    `json:"single"`
	PassMark string `json:"passmark"`
	Note     string `json:"note,omitempty"`
}

var cpus = func() map[string]CPUInfo {
	var raw map[string]CPUInfo
	if err := json.Unmarshal(cpusJSON, &raw); err != nil {
		panic("cpus.json: " + err.Error())
	}
	out := make(map[string]CPUInfo, len(raw))
	for name, c := range raw {
		out[cpuKey(name)] = c
	}
	return out
}()

// CPU looks a CPU up by the name an invoice gives it. The catalogs spell one chip several ways ("Xeon-E3 1230 v6",
// "Xeon E3-1230 v6", "Intel i7-6700k", "AMD EPYC TURIN 9455"): vendor and family words, case and punctuation don't
// count.
func CPU(name string) (CPUInfo, bool) {
	c, ok := cpus[cpuKey(name)]
	return c, ok && c.Mark > 0
}

// CPUNames are the names CPU finds, for an error message.
func CPUNames() []string {
	var out []string
	var raw map[string]CPUInfo
	_ = json.Unmarshal(cpusJSON, &raw)
	for name, c := range raw {
		if c.Mark > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// cpuKey normalizes a CPU name: "2x Intel Xeon Silver 4214R" and "Dual Intel Xeon Silver 4214R" → "dualxeonsilver4214r".
func cpuKey(name string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == ' ' || r == '-' }) {
		switch w {
		case "intel", "amd", "core", "turin", "cpu", "processor":
			continue
		case "2x":
			w = "dual"
		}
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
