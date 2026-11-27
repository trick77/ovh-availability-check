package watch

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/trick77/ovh-availability-check/internal/ovh"
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
// nil (catalog not fetched yet): every variant is then unpriced and unnamed.
func Build(av []ovh.Availability, cat *ovh.Catalog, dcs []string) []Variant {
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

// Matches tells whether v meets w, stock aside.
func (w Want) Matches(v Variant) bool {
	if w.RAMMin > 0 && v.RAM < w.RAMMin {
		return false
	}
	if w.RAMMax > 0 && (v.RAM == 0 || v.RAM > w.RAMMax) {
		return false
	}
	kind := strings.ToLower(w.Disk)
	if kind != "" && !slices.ContainsFunc(v.Disks, func(d ovh.Disk) bool { return d.Kind == kind }) {
		return false
	}
	if w.DiskMin > 0 {
		var gb int
		for _, d := range v.Disks {
			if kind == "" || d.Kind == kind {
				gb += d.Count * d.SizeGB
			}
		}
		if float64(gb) < w.DiskMin*1000 {
			return false
		}
	}
	if w.CPU != "" && !strings.Contains(strings.ToLower(v.CPU), strings.ToLower(w.CPU)) {
		return false
	}
	// A wish with a price limit never takes a variant whose price is unknown.
	if w.PriceMax > 0 && (!v.Priced || v.Rent > w.PriceMax) {
		return false
	}
	return true
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
