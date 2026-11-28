package watch

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/trick77/ovh-restock/internal/ovh"
)

// Variant is one server configuration (an fqn) with its stock in the watched datacenters.
type Variant struct {
	FQN, PlanCode string
	Name, CPU     string
	RAM           int
	Disks         []ovh.Disk
	Rent, Setup   float64
	Priced        bool
	OrderURL      string
	// Stock is the availability value per watched datacenter the variant is listed in.
	Stock map[string]string
	// Speed is the CPU's multithread figure against the reference CPU in percent (-39 is 39 % slower); Rated is
	// false without a reference or a PassMark figure.
	Speed int
	Rated bool
}

// InStock are the watched datacenters the variant can be ordered in, sorted.
func (v Variant) InStock() []string {
	var out []string
	for dc, a := range v.Stock {
		if ovh.InStock(a) {
			out = append(out, dc)
		}
	}
	slices.Sort(out)
	return out
}

// Summary is one line: "KS-2 · Intel Xeon-D 1540 · 64 GB · 2×450GB NVME · 26.99 €/mo (+18.99 setup)".
func (v Variant) Summary() string {
	parts := []string{cmp.Or(v.Name, v.PlanCode)}
	if v.CPU != "" {
		parts = append(parts, v.CPU)
	}
	parts = append(parts, fmt.Sprintf("%d GB", v.RAM))
	var disks []string
	for _, d := range v.Disks {
		disks = append(disks, d.String())
	}
	if len(disks) > 0 {
		parts = append(parts, strings.Join(disks, " + "))
	}
	parts = append(parts, v.PriceText())
	return strings.Join(parts, " · ")
}

// PriceText is "26.99 €/mo (+18.99 setup)", or "price unknown".
func (v Variant) PriceText() string {
	if !v.Priced {
		return "price unknown"
	}
	s := fmt.Sprintf("%.2f €/mo", v.Rent)
	if v.Setup > 0 {
		s += fmt.Sprintf(" (+%.2f setup)", v.Setup)
	}
	return s
}

// Build joins stock and catalog into the variants listed in any of dcs, cheapest first, unpriced last. cat may be
// nil (catalog not fetched yet): every variant is then unpriced and unnamed. ref is the reference CPU, "" for none.
func Build(av []ovh.Availability, cat *ovh.Catalog, dcs []string, ref string) []Variant {
	r, refOK := ovh.CPU(ref)
	refOK = refOK && r.Mark > 0
	var out []Variant
	for _, a := range av {
		stock := map[string]string{}
		for _, d := range a.Datacenters {
			if slices.Contains(dcs, d.Datacenter) {
				stock[d.Datacenter] = d.Availability
			}
		}
		if len(stock) == 0 {
			continue
		}
		v := Variant{FQN: a.FQN, PlanCode: a.PlanCode, RAM: ovh.ParseRAM(a.Memory), Disks: ovh.ParseDisks(a.Storage),
			Stock: stock}
		v.Name, v.CPU = cat.Name(a.PlanCode)
		v.Rent, v.Setup, v.Priced = cat.Price(a)
		v.OrderURL = cat.OrderURL(a.PlanCode, v.Name)
		if c, ok := ovh.CPU(v.CPU); ok && refOK && c.Mark > 0 {
			v.Speed, v.Rated = int(math.Round((float64(c.Mark)/float64(r.Mark)-1)*100)), true
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b Variant) int {
		if a.Priced != b.Priced {
			if a.Priced {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Rent, b.Rent), cmp.Compare(a.FQN, b.FQN))
	})
	return out
}

// miss is one criterion of a wish a variant fails. near: it fails narrowly, see nearPrice and friends.
type miss struct {
	near bool
	text string
}

// The margins of "just above" a wish: a variant that misses one criterion by no more than this is tracked and
// analysed like a wanted one (user: wanted + just above), never pushed.
const (
	nearPrice = 1.10 // rent up to 10 % over price_max_eur
	nearDisk  = 0.90 // raw disk down to 90 % of disk_min_tb
	// RAM: one size step, and sizes double (16, 32, 64 …).
)

// misses are the criteria of w that v fails, stock aside.
func (w Want) misses(v Variant) []miss {
	var out []miss
	// A product the wish doesn't name is never near: the user asked for that one.
	if len(w.Plans) > 0 && !slices.ContainsFunc(w.Plans, func(p string) bool { return PlanIs(v, p) }) {
		out = append(out, miss{false, "product"})
	}
	if w.RAMMin > 0 && v.RAM < w.RAMMin {
		out = append(out, miss{v.RAM > 0 && v.RAM*2 >= w.RAMMin, fmt.Sprintf("%d GB, wants ≥ %d", v.RAM, w.RAMMin)})
	}
	if w.RAMMax > 0 && (v.RAM == 0 || v.RAM > w.RAMMax) {
		out = append(out, miss{v.RAM > 0 && v.RAM <= w.RAMMax*2, fmt.Sprintf("%d GB, wants ≤ %d", v.RAM, w.RAMMax)})
	}
	kind := strings.ToLower(w.Disk)
	if kind != "" && !slices.ContainsFunc(v.Disks, func(d ovh.Disk) bool { return d.Kind == kind }) {
		out = append(out, miss{false, "no " + strings.ToUpper(kind)})
	}
	if w.DiskMin > 0 {
		var gb int
		for _, d := range v.Disks {
			if kind == "" || d.Kind == kind {
				gb += d.Count * d.SizeGB
			}
		}
		if want := w.DiskMin * 1000; float64(gb) < want {
			out = append(out, miss{float64(gb) >= want*nearDisk,
				fmt.Sprintf("disk %.1f TB, wants ≥ %g", float64(gb)/1000, w.DiskMin)})
		}
	}
	if w.CPU != "" && !strings.Contains(strings.ToLower(v.CPU), strings.ToLower(w.CPU)) {
		out = append(out, miss{false, "CPU"})
	}
	// Unrated (no reference, no PassMark figure) never matches, like an unknown price.
	if w.CPUMinPct != nil && (!v.Rated || v.Speed < *w.CPUMinPct) {
		out = append(out, miss{false, "CPU too slow"})
	}
	// A wish with a price limit never takes a variant whose price is unknown.
	if w.PriceMax > 0 && (!v.Priced || v.Rent > w.PriceMax) {
		out = append(out, miss{v.Priced && v.Rent <= w.PriceMax*nearPrice,
			fmt.Sprintf("+%.2f € over %g", v.Rent-w.PriceMax, w.PriceMax)})
	}
	return out
}

// PlanIs tells whether p names v's product: its planCode ("27skle03-v1") or catalog name ("KS-LE-3"), case,
// dashes and spaces aside, exactly: "KS-5" is not "KS-5-A".
func PlanIs(v Variant, p string) bool {
	n := normPlan(p)
	return n != "" && (n == normPlan(v.PlanCode) || n == normPlan(v.Name))
}

func normPlan(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// UnknownPlans are w's plans no variant of vs has: a typo would wait forever. Meaningful only with a catalog
// (without one variants have no names).
func (w Want) UnknownPlans(vs []Variant) []string {
	var out []string
	for _, p := range w.Plans {
		if !slices.ContainsFunc(vs, func(v Variant) bool { return PlanIs(v, p) }) {
			out = append(out, p)
		}
	}
	return out
}

// Matches tells whether v meets w, stock aside.
func (w Want) Matches(v Variant) bool { return len(w.misses(v)) == 0 }

// Near tells whether v is just above w: it fails exactly one criterion, narrowly. why says which ("+1.99 € over
// 30"). A product, CPU or disk-kind mismatch is never near.
func (w Want) Near(v Variant) (why string, ok bool) {
	m := w.misses(v)
	if len(m) != 1 || !m[0].near {
		return "", false
	}
	return m[0].text, true
}

// NearMiss is the first wish v is just above, when no wish takes it.
func (c Config) NearMiss(v Variant) (why string, ok bool) {
	if c.Wanted(v) {
		return "", false
	}
	for _, w := range c.Want {
		if why, ok := w.Near(v); ok {
			return why, true
		}
	}
	return "", false
}

// InScope tells whether v is wanted or just above a wish: what the page's history, the log and the prediction look
// at. The state keeps every pair's history either way.
func (c Config) InScope(v Variant) bool {
	_, near := c.NearMiss(v)
	return near || c.Wanted(v)
}

// Wanted tells whether any wish of c takes v.
func (c Config) Wanted(v Variant) bool {
	return slices.ContainsFunc(c.Want, func(w Want) bool { return w.Matches(v) })
}

// Pair is a variant in stock in one datacenter: what a push is about.
type Pair struct {
	FQN, DC string
}

func (p Pair) key() string { return Key(p.FQN, p.DC) }

// Current are the pairs in stock that some wish takes.
func Current(c Config, vs []Variant) map[string]Pair {
	out := map[string]Pair{}
	for _, v := range vs {
		if !c.Wanted(v) {
			continue
		}
		for _, dc := range v.InStock() {
			p := Pair{v.FQN, dc}
			out[p.key()] = p
		}
	}
	return out
}

// Items are fresh's servers as the page shows a restock event, cheapest first.
func Items(vs []Variant, fresh []Pair) []Item {
	dcs := map[string][]string{}
	for _, p := range fresh {
		dcs[p.FQN] = append(dcs[p.FQN], p.DC)
	}
	var out []Item
	for _, v := range vs { // vs is cheapest first
		d, ok := dcs[v.FQN]
		if !ok {
			continue
		}
		slices.Sort(d)
		it := Item{FQN: v.FQN, Name: v.Name, PlanCode: v.PlanCode, CPU: v.CPU, RAM: v.RAM, Rent: v.Rent, Priced: v.Priced,
			OrderURL: v.OrderURL}
		for _, x := range v.Disks {
			it.Disks = append(it.Disks, x.String())
		}
		for _, dc := range d {
			it.DCs = append(it.DCs, ItemDC{DC: dc, Slow: v.Stock[dc] == "72H"})
		}
		out = append(out, it)
	}
	return out
}

// maxBody caps a push's body in characters: the first run after a wide wish can find dozens, and Pushover refuses
// a message over 1024 characters (shoutrrr doesn't cut it). A refused push is retried every poll.
const maxBody = 1000

// Message is one push for every fresh pair: per variant a line with its datacenters behind it, then its order link.
func Message(vs []Variant, fresh []Pair) (title, body string) {
	dcs := map[string][]string{}
	for _, p := range fresh {
		dcs[p.FQN] = append(dcs[p.FQN], p.DC)
	}
	var entries []string
	for _, v := range vs { // vs is cheapest first
		d, ok := dcs[v.FQN]
		if !ok {
			continue
		}
		slices.Sort(d)
		e := v.Summary() + " · " + strings.Join(d, ", ")
		if v.OrderURL != "" {
			e += "\n" + v.OrderURL
		}
		entries = append(entries, e)
	}
	n := len(entries)
	title = "OVH: 1 server in stock"
	if n != 1 {
		title = fmt.Sprintf("OVH: %d servers in stock", n)
	}
	// Whole entries while they fit, room left for the "… and N more" line.
	const more = len("\n\n… and 9999 more")
	size, kept := 0, 0
	for i, e := range entries {
		add := utf8.RuneCountInString(e)
		if i > 0 {
			add += 2
		}
		limit := maxBody
		if i < n-1 {
			limit -= more
		}
		if kept > 0 && size+add > limit {
			break
		}
		size += add
		kept++
	}
	if kept < n {
		entries = append(entries[:kept], fmt.Sprintf("… and %d more", n-kept))
	}
	return title, strings.Join(entries, "\n\n")
}
