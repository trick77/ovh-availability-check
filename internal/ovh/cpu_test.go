package ovh

import "testing"

func TestCPU(t *testing.T) {
	// The catalogs' spellings of one chip find the same entry.
	for _, pair := range [][2]string{
		{"Intel Xeon-E3 1230 v6", "Intel Xeon E3-1230 v6"},
		{"Intel i7-6700k", "Intel Core i7 6700K"},
		{"AMD EPYC TURIN 9455", "AMD EPYC 9455"},
		{"2x Intel Xeon Silver 4214R", "Dual Intel Xeon Silver 4214R"},
	} {
		if cpuKey(pair[0]) != cpuKey(pair[1]) {
			t.Errorf("%q and %q differ: %q, %q", pair[0], pair[1], cpuKey(pair[0]), cpuKey(pair[1]))
		}
	}
	// Every CPU of the fixture catalog is known.
	_, cat := fixtures(t)
	for code := range cat.plans {
		if _, cpu := cat.Name(code); cpu != "" {
			if c, ok := CPU(cpu); !ok || c.Cores == 0 || c.Threads < c.Cores || c.Single == 0 {
				t.Errorf("%s: %q not in cpus.json: %+v", code, cpu, c)
			}
		}
	}
	if _, ok := CPU("Intel Pentium 4"); ok {
		t.Error("unknown CPU found")
	}
}
