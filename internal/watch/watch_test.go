package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/trick77/ovh-restock/internal/ovh"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	ok := `{"datacenters":["gra"],"interval":"5m","want":[{"ram_max_gb":64,"price_max_eur":30}]}`
	c, err := LoadConfig(writeConfig(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.String() != "5m0s" || c.Want[0].RAMMax != 64 {
		t.Errorf("got %+v", c)
	}
	for name, body := range map[string]string{
		"unknown key":   `{"datacenters":["gra"],"want":[{"ram_max":64}]}`,
		"empty want":    `{"datacenters":["gra"],"want":[{"name":"all"}]}`,
		"no want":       `{"datacenters":["gra"],"want":[]}`,
		"no dc":         `{"want":[{"ram_max_gb":64}]}`,
		"bad disk":      `{"datacenters":["gra"],"want":[{"disk":"floppy"}]}`,
		"short poll":    `{"datacenters":["gra"],"interval":"10s","want":[{"ram_max_gb":64}]}`,
		"ram min > max": `{"datacenters":["gra"],"want":[{"ram_min_gb":128,"ram_max_gb":64}]}`,
		"unknown cpu":   `{"datacenters":["gra"],"want":[{"ram_max_gb":64}],"reference_cpu":"Pentium 4"}`,
	} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

var ks2 = Variant{
	FQN: "24sk202.ram-64g-ecc-2133.softraid-2x450nvme", PlanCode: "24sk202", Name: "KS-2", CPU: "Intel Xeon-D 1540",
	RAM: 64, Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Rent: 26.99, Setup: 18.99, Priced: true,
	Stock:    map[string]string{"gra": "72H", "rbx": "unavailable", "sbg": "1H-low"},
	OrderURL: "https://eco.ovhcloud.com/fr/kimsufi/ks-2/",
}

func TestMatches(t *testing.T) {
	unpriced := ks2
	unpriced.Priced, unpriced.Rent = false, 0
	hybrid := ks2
	hybrid.Disks = []ovh.Disk{{Count: 2, SizeGB: 4000, Kind: "hdd"}, {Count: 2, SizeGB: 450, Kind: "nvme"}}
	for _, tc := range []struct {
		name string
		w    Want
		v    Variant
		want bool
	}{
		{"ram max", Want{RAMMax: 64}, ks2, true},
		{"ram max below", Want{RAMMax: 32}, ks2, false},
		{"ram min", Want{RAMMin: 128}, ks2, false},
		{"disk kind", Want{Disk: "nvme"}, ks2, true},
		{"disk kind case", Want{Disk: "NVMe"}, ks2, true},
		{"disk kind other", Want{Disk: "hdd"}, ks2, false},
		{"disk size raw", Want{DiskMin: 0.9}, ks2, true},
		{"disk size too big", Want{DiskMin: 1}, ks2, false},
		{"hybrid counts own kind", Want{Disk: "nvme", DiskMin: 1}, hybrid, false},
		{"hybrid counts all", Want{DiskMin: 8}, hybrid, true},
		{"cpu", Want{CPU: "xeon"}, ks2, true},
		{"cpu other", Want{CPU: "epyc"}, ks2, false},
		{"price", Want{PriceMax: 30}, ks2, true},
		{"price over", Want{PriceMax: 25}, ks2, false},
		{"price unknown never matches", Want{PriceMax: 1000}, unpriced, false},
		{"no price wish takes unpriced", Want{RAMMax: 64}, unpriced, true},
	} {
		if got := tc.w.Matches(tc.v); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

func TestNear(t *testing.T) {
	pct := func(n int) *int { return &n }
	slow := ks2
	slow.Speed, slow.Rated = -39, true
	for _, tc := range []struct {
		name string
		w    Want
		v    Variant
		near bool
		why  string
	}{
		{"price just over", Want{PriceMax: 25}, ks2, true, "+1.99 € over 25"},
		{"price far over", Want{PriceMax: 20}, ks2, false, ""},
		{"ram one step under", Want{RAMMin: 128}, ks2, true, "64 GB, wants ≥ 128"},
		{"ram two steps under", Want{RAMMin: 256}, ks2, false, ""},
		{"ram one step over", Want{RAMMax: 32}, ks2, true, "64 GB, wants ≤ 32"},
		{"disk 10 % short", Want{DiskMin: 0.95}, ks2, true, "disk 0.9 TB, wants ≥ 0.95"},
		{"disk far short", Want{DiskMin: 2}, ks2, false, ""},
		{"disk kind never near", Want{Disk: "hdd"}, ks2, false, ""},
		{"cpu never near", Want{CPU: "epyc"}, ks2, false, ""},
		{"two misses", Want{PriceMax: 25, RAMMin: 128}, ks2, false, ""},
		{"matches is not near", Want{PriceMax: 30}, ks2, false, ""},
		{"too slow never near", Want{CPUMinPct: pct(-10)}, slow, false, ""},
	} {
		why, near := tc.w.Near(tc.v)
		if near != tc.near || why != tc.why {
			t.Errorf("%s: %v %q", tc.name, near, why)
		}
	}
}

func TestCPUMinPct(t *testing.T) {
	pct := func(n int) *int { return &n }
	v := ks2
	v.Speed, v.Rated = -39, true // KS-1's Xeon-D 1520 against the E3-1230 v6
	if (Want{CPUMinPct: pct(-10)}).Matches(v) {
		t.Error("39 % slower passed -10")
	}
	v.Speed = -5
	if !(Want{CPUMinPct: pct(-10)}).Matches(v) {
		t.Error("5 % slower failed -10")
	}
	v.Rated = false
	if (Want{CPUMinPct: pct(-50)}).Matches(v) {
		t.Error("unrated passed")
	}
	if _, err := LoadConfig(writeConfig(t, `{"datacenters":["gra"],"want":[{"cpu_min_pct":-10}]}`)); err == nil {
		t.Error("cpu_min_pct without reference_cpu accepted")
	}
	if _, err := LoadConfig(writeConfig(t,
		`{"datacenters":["gra"],"reference_cpu":"Intel Xeon E3-1230 v6","want":[{"cpu_min_pct":0}]}`)); err != nil {
		t.Errorf("cpu_min_pct 0 refused: %v", err)
	}
}

func TestBuildRates(t *testing.T) {
	av := []ovh.Availability{{FQN: "a", PlanCode: "a", Memory: "ram-32g-x", Storage: "softraid-2x450nvme",
		Datacenters: []struct {
			Datacenter   string `json:"datacenter"`
			Availability string `json:"availability"`
		}{{"gra", "1H-low"}}}}
	// No catalog: no CPU, unrated even with a reference.
	if vs := Build(av, nil, []string{"gra"}, "Intel Xeon E3-1230 v6"); vs[0].Rated {
		t.Errorf("rated without a CPU: %+v", vs[0])
	}
}

func TestInScope(t *testing.T) {
	c := Config{Want: []Want{{PriceMax: 25}}}
	if !c.InScope(ks2) {
		t.Error("just above not in scope")
	}
	if why, ok := c.NearMiss(ks2); !ok || why != "+1.99 € over 25" {
		t.Errorf("near miss %q", why)
	}
	c.Want[0].PriceMax = 30
	if _, ok := c.NearMiss(ks2); ok {
		t.Error("wanted reported as near")
	}
	c.Want[0].PriceMax = 10
	if c.InScope(ks2) {
		t.Error("far over in scope")
	}
}

func TestBuild(t *testing.T) {
	av := []ovh.Availability{
		{FQN: "a.ram-32g-x.softraid-2x450nvme", PlanCode: "a", Memory: "ram-32g-x", Storage: "softraid-2x450nvme"},
		{FQN: "b.ram-64g-x.softraid-2x450nvme", PlanCode: "b", Memory: "ram-64g-x", Storage: "softraid-2x450nvme"},
	}
	av[0].Datacenters = append(av[0].Datacenters, struct {
		Datacenter   string `json:"datacenter"`
		Availability string `json:"availability"`
	}{"gra", "72H"})
	av[1].Datacenters = append(av[1].Datacenters, struct {
		Datacenter   string `json:"datacenter"`
		Availability string `json:"availability"`
	}{"syd", "72H"})
	vs := Build(av, nil, []string{"gra"}, "")
	if len(vs) != 1 || vs[0].RAM != 32 || vs[0].Priced || vs[0].Stock["gra"] != "72H" {
		t.Fatalf("got %+v", vs)
	}
}

func TestEdges(t *testing.T) {
	c := Config{Datacenters: []string{"gra", "rbx", "sbg"}, Want: []Want{{RAMMax: 64}}}
	vs := []Variant{ks2}

	cur := Current(c, vs)
	fresh := State{}.New(cur)
	want := []Pair{{ks2.FQN, "gra"}, {ks2.FQN, "sbg"}}
	if !reflect.DeepEqual(fresh, want) {
		t.Fatalf("fresh %v", fresh)
	}

	// A failed push records nothing: the next poll tries again.
	s := State{}.Next(cur, fresh, false)
	if len(s.New(cur)) != 2 {
		t.Errorf("failed push recorded: %v", s)
	}

	// Sent: the same stock fires nothing again.
	s = State{}.Next(cur, fresh, true)
	if n := s.New(Current(c, vs)); len(n) != 0 {
		t.Errorf("repeat: %v", n)
	}

	// gra sells out: it drops from the state silently, and fires again when it returns.
	out := ks2
	out.Stock = map[string]string{"gra": "unavailable", "sbg": "1H-low"}
	cur2 := Current(c, []Variant{out})
	s = s.Next(cur2, s.New(cur2), true)
	if len(s.Notified) != 1 {
		t.Errorf("sold out kept: %v", s.Notified)
	}
	if n := s.New(Current(c, vs)); !reflect.DeepEqual(n, []Pair{{ks2.FQN, "gra"}}) {
		t.Errorf("return: %v", n)
	}

	// A wish added later fires for stock already there, because the state holds pushes, not snapshots.
	c2 := Config{Datacenters: c.Datacenters, Want: []Want{{RAMMax: 32}}}
	s = State{}.Next(Current(c2, vs), nil, true)
	if n := s.New(Current(c, vs)); len(n) != 2 {
		t.Errorf("new wish: %v", n)
	}
}

func TestStateSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if s, err := LoadState(p); err != nil || len(s.Notified) != 0 {
		t.Fatalf("missing file: %v %v", s, err)
	}
	in := State{Notified: []string{"a@gra"}}
	if err := in.Save(p); err != nil {
		t.Fatal(err)
	}
	out, err := LoadState(p)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Errorf("got %v %v", out, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Errorf("temp file left: %v", ents)
	}
}

func TestMessage(t *testing.T) {
	title, body := Message([]Variant{ks2}, []Pair{{ks2.FQN, "sbg"}, {ks2.FQN, "gra"}})
	if title != "OVH: 1 server in stock" {
		t.Errorf("title %q", title)
	}
	wantBody := "KS-2 · Intel Xeon-D 1540 · 64 GB · 2×450GB NVME · 26.99 €/mo (+18.99 setup) · gra, sbg\n" +
		"https://eco.ovhcloud.com/fr/kimsufi/ks-2/"
	if body != wantBody {
		t.Errorf("body\n%s\nwant\n%s", body, wantBody)
	}

	var vs []Variant
	var fresh []Pair
	for i := range 20 {
		v := ks2
		v.FQN = strings.Repeat("x", i+1)
		vs = append(vs, v)
		fresh = append(fresh, Pair{v.FQN, "gra"})
	}
	title, body = Message(vs, fresh)
	// Each entry is ~120 characters with its link: 7 fit under Pushover's 1024.
	if title != "OVH: 20 servers in stock" || !strings.HasSuffix(body, "… and 13 more") ||
		utf8.RuneCountInString(body) > 1024 {
		t.Errorf("cap: %q / %d chars / %q", title, utf8.RuneCountInString(body), body[len(body)-20:])
	}
	// One entry over the budget still goes out: a push without a server says nothing.
	long := ks2
	long.Name = strings.Repeat("L", 1200)
	if _, body := Message([]Variant{long}, []Pair{{long.FQN, "gra"}}); !strings.Contains(body, long.Name) {
		t.Error("single long entry dropped")
	}
}
