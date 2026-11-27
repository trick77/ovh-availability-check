package main

import (
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/kit/httpx"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/watch"
)

//go:embed page.html
var pageHTML string

// fonts are the family's faces, as ../trakk ships them.
//
//go:embed fonts/*.woff2
var fonts embed.FS

// fontFiles are the fonts by file name; fontVersion goes into their URLs: they are cached for a year, a changed file
// needs a new URL.
var fontFiles, fontVersion = func() (map[string][]byte, string) {
	files := map[string][]byte{}
	h := sha256.New()
	ents, _ := fonts.ReadDir("fonts")
	for _, e := range ents {
		b, _ := fonts.ReadFile("fonts/" + e.Name())
		files[e.Name()] = b
		h.Write(b)
	}
	return files, hex.EncodeToString(h.Sum(nil))[:10]
}()

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"ago":    ago,
	"abs":    abs,
	"rowsOf": func(rs []row, p *pageData) rowsArg { return rowsArg{Rows: rs, Page: p} },
}).Parse(pageHTML))

// rowsArg is what the "rows" template takes.
type rowsArg struct {
	Rows []row
	Page *pageData
}

const (
	maxChanges      = 30
	maxPriceChanges = 20
)

type pageData struct {
	Title       string
	FontVersion string
	PolledAt    time.Time
	PollErr     string
	CatalogAt   time.Time
	CatErr      string
	Interval    string
	ShowAll     bool
	Datacenters []string
	Channels    []string
	Since       time.Time
	RefCPU      string
	Ref         *ovh.CPUInfo
	Tiles       []tile
	Wants       []wantRow
	Wanted      []row
	All         []row
	AllCount    int
	History     []histVariant
	HistW       int // the history's x axis, in minutes
	Quiet       []string
	Axis        []string
	Changes     []changeRow
	Prices      []priceRow
	MorePrices  bool
	Pushes      []watch.Push
}

// cell is a table cell worked out in Go, so the template only prints: text, a CSS class, a tooltip.
type cell struct {
	Text, Class, Title string
}

type tile struct {
	Label, Value, Unit, Sub, SubClass, Title string
	Highlight                                bool
}

// wantRow is one wish: how many watched variants it takes at all, and how many are in stock. Zero at all is the
// silent case a typo or a renamed spec makes, so the page flags it.
type wantRow struct {
	Label   string
	Rule    string
	All     int
	InStock int
	Last    cell
}

// row is a variant in a table.
type row struct {
	watch.Variant
	CPUName string
	Cores   string
	Multi   cell
	Single  cell
	Price   cell // the change against the previous price
	Setup   cell
	Last    cell
	DCs     []cell
	Out     bool
}

type histVariant struct {
	Name, Desc string
	Was        cell
	Rows       []histRow
}

type histRow struct {
	DC    string
	Rects []rect
}

// rect is a segment of a history bar, in minutes on a 30-day axis.
type rect struct {
	X, W  int
	Class string
	Title string
}

type changeRow struct {
	At    time.Time
	Kind  string // in, out, price
	Text  string
	DC    string
	Extra string
}

type priceRow struct {
	At                time.Time
	Name, Config      string
	Rent, Setup       cell
	OldRent, OldSetup string // set when that part changed
}

func (w *watcher) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.page)
	mux.HandleFunc("GET /fonts/{name}", serveFont)
	httpx.Health(mux, slog.Default())
	return mux
}

func serveFont(rw http.ResponseWriter, r *http.Request) {
	b, ok := fontFiles[r.PathValue("name")]
	if !ok {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "font/woff2")
	rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = rw.Write(b)
}

func (w *watcher) page(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	vs, state := w.variants, w.state
	d := pageData{
		FontVersion: fontVersion,
		PolledAt:    w.polledAt, PollErr: w.pollErr, CatalogAt: w.catalogAt, CatErr: w.catErr,
		Interval:    short(time.Duration(w.cfg.Interval)),
		ShowAll:     r.URL.Query().Get("all") == "1",
		Datacenters: w.cfg.Datacenters, Channels: w.channels.Labels(),
		AllCount: len(vs),
		HistW:    int(watch.Window / time.Minute),
	}
	w.mu.Unlock()
	now := time.Now()
	d.Since, d.Pushes = state.Since, state.Pushes
	if c, ok := ovh.CPU(w.cfg.ReferenceCPU); ok {
		d.Ref, d.RefCPU = &c, shortCPU(w.cfg.ReferenceCPU)
	}

	var wanted []watch.Variant
	for _, v := range vs {
		if w.cfg.Wanted(v) {
			wanted = append(wanted, v)
		}
	}
	for _, v := range wanted {
		if len(v.InStock()) > 0 {
			d.Wanted = append(d.Wanted, d.row(v, state, now))
		}
	}
	if d.ShowAll {
		for _, v := range vs {
			d.All = append(d.All, d.row(v, state, now))
		}
	}
	for i, want := range w.cfg.Want {
		wr := wantRow{Label: want.Label(i), Rule: rule(want)}
		var matched []watch.Variant
		for _, v := range vs {
			if want.Matches(v) {
				matched = append(matched, v)
				wr.All++
				if len(v.InStock()) > 0 {
					wr.InStock++
				}
			}
		}
		wr.Last = lastCell(matched, state, now)
		d.Wants = append(d.Wants, wr)
	}
	d.History, d.Quiet, d.Axis = history(wanted, state, now)
	d.Changes = changes(wanted, vs, state, now)
	d.Prices, d.MorePrices = priceChanges(vs, w.cfg, state, d.ShowAll)
	d.Tiles = tiles(d, wanted, state, now)

	d.Title = "OVH stock monitor"
	if n := len(d.Wanted); n > 0 {
		d.Title += fmt.Sprintf("%s%d in stock", sep, n)
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(rw, &d); err != nil {
		slog.Warn("page", "err", err)
	}
}

func (d *pageData) row(v watch.Variant, s watch.State, now time.Time) row {
	r := row{Variant: v, CPUName: shortCPU(v.CPU), Out: len(v.InStock()) == 0}
	if v.CPU != "" {
		if c, ok := ovh.CPU(v.CPU); ok {
			r.Cores = fmt.Sprintf("%dC/%dT", c.Cores, c.Threads)
			if d.Ref != nil {
				r.Multi = pct(c.Mark, d.Ref.Mark, "CPU Mark")
				r.Single = pct(c.Single, d.Ref.Single, "single thread")
			}
		} else if d.Ref != nil {
			r.Multi = cell{Text: "?", Class: "x eq", Title: "no PassMark figure in cpus.json: " + v.CPU}
		}
	}
	r.Price, r.Setup = priceCells(s.Prices[v.FQN])
	r.Last = lastCell([]watch.Variant{v}, s, now)
	for _, dc := range d.Datacenters {
		r.DCs = append(r.DCs, dcCell(v.Stock[dc]))
	}
	return r
}

// pct is a speed against the reference: "↑ 20 %", "↓ 40 %", "≈" within 5 %.
func pct(v, ref int, what string) cell {
	if v <= 0 || ref <= 0 {
		return cell{}
	}
	title := what + " " + thousands(v) + " vs " + thousands(ref)
	p := int(math.Round((float64(v)/float64(ref) - 1) * 100))
	switch {
	case p >= 5:
		return cell{Text: "↑ " + strconv.Itoa(p) + " %", Class: "x up", Title: title}
	case p <= -5:
		return cell{Text: "↓ " + strconv.Itoa(-p) + " %", Class: "x down", Title: title}
	}
	return cell{Text: "≈", Class: "x eq", Title: title}
}

// priceCells are the rent's and setup's change against the previous price point.
func priceCells(ps []watch.Price) (rent, setup cell) {
	n := len(ps)
	if n < 2 {
		return
	}
	cur, prev := ps[n-1], ps[n-2]
	if diff := cur.Rent - prev.Rent; math.Abs(diff) >= 0.005 {
		rent = cell{Text: "↓" + money(-diff), Class: "pd down",
			Title: "was " + money(prev.Rent) + " until " + cur.At.Local().Format("02.01.2006")}
		if diff > 0 {
			rent.Text, rent.Class = "↑"+money(diff), "pd up"
		}
	}
	if cur.Setup != prev.Setup {
		setup = cell{Text: "was " + money(prev.Setup), Class: "tag",
			Title: "setup was " + money(prev.Setup) + " until " + cur.At.Local().Format("02.01.2006")}
	}
	return rent, setup
}

// lastCell is when any DC of vs was last in stock: "now · 2h" for the current streak, "3d" ago, or "–".
func lastCell(vs []watch.Variant, s watch.State, now time.Time) cell {
	var last, since time.Time
	in := false
	for _, v := range vs {
		e, ok := s.Seen[v.FQN]
		if !ok {
			continue
		}
		if e.Last.After(last) {
			last = e.Last
		}
		if e.In && len(v.InStock()) > 0 {
			in = true
			if since.IsZero() || e.Since.Before(since) {
				since = e.Since
			}
		}
	}
	switch {
	case in:
		return cell{Text: "now · " + compact(now.Sub(since)), Class: "last now", Title: "since " + abs(since)}
	case !last.IsZero():
		return cell{Text: ago(last), Title: abs(last)}
	case !s.Since.IsZero():
		return cell{Text: "–", Class: "sub", Title: "not seen since " + abs(s.Since)}
	}
	return cell{Text: "–", Class: "sub"}
}

func dcCell(a string) cell {
	switch {
	case a == "":
		return cell{}
	case a == "unavailable":
		return cell{Text: "✕", Class: "na", Title: a}
	case a == "unknown":
		return cell{Text: "?", Class: "na", Title: a}
	case a == "comingSoon":
		return cell{Text: "soon", Class: "pill soon", Title: a}
	case a == "72H":
		return cell{Text: a, Class: "pill slow", Title: "orderable, delivery within 72 h"}
	case ovh.InStock(a):
		return cell{Text: a, Class: "pill on", Title: "orderable"}
	}
	return cell{Text: a, Class: "pill"}
}

// history is a block per wanted variant with stock in the window, a bar per DC it is listed in.
func history(wanted []watch.Variant, s watch.State, now time.Time) (hv []histVariant, quiet, axis []string) {
	start := now.Add(-watch.Window)
	mins := func(t time.Time) int { return int(t.Sub(start) / time.Minute) }
	total := mins(now)
	minW := total / 1200 // about a pixel on a wide screen: a single poll must show
	clip := func(from, to time.Time, class, title string) (rect, bool) {
		x0, x1 := max(mins(from), 0), min(mins(to), total)
		if x1 < 0 || x0 > total {
			return rect{}, false
		}
		return rect{X: x0, W: max(x1-x0, minW), Class: class, Title: title}, true
	}
	var active, idle []histVariant
	for _, v := range wanted {
		h := histVariant{Name: cmp.Or(v.Name, v.PlanCode), Desc: desc(v)}
		if ps := s.Prices[v.FQN]; len(ps) >= 2 && math.Abs(ps[len(ps)-1].Rent-ps[len(ps)-2].Rent) >= 0.005 {
			h.Was = cell{Text: "was " + money(ps[len(ps)-2].Rent) + " until " + ps[len(ps)-1].At.Local().Format("02.01."),
				Class: "was"}
		}
		// Sold out is drawn from the variant's first span on: before it, it may not have been wanted, and nobody
		// tracked it. That is no data, not sold out.
		var tracked time.Time
		for k, sp := range s.Spans {
			if strings.HasPrefix(k, v.FQN+"@") && len(sp) > 0 && (tracked.IsZero() || sp[0].From.Before(tracked)) {
				tracked = sp[0].From
			}
		}
		var base []rect
		for _, p := range s.Polled {
			if p.To.Before(tracked) {
				continue
			}
			if r, ok := clip(maxTime(p.From, tracked), p.To, "h-out", ""); ok {
				base = append(base, r)
			}
		}
		stocked := false
		// The DCs it is listed in now, plus any it had stock in: OVH drops a DC from the list at times.
		dcs := slices.Collect(maps.Keys(v.Stock))
		for k := range s.Spans {
			if dc, ok := strings.CutPrefix(k, v.FQN+"@"); ok && !slices.Contains(dcs, dc) {
				dcs = append(dcs, dc)
			}
		}
		slices.Sort(dcs)
		for _, dc := range dcs {
			hr := histRow{DC: dc, Rects: base}
			for _, sp := range s.Spans[watch.Key(v.FQN, dc)] {
				class, kind := "h-in", "1H"
				if sp.Slow {
					class, kind = "h-slow", "72H"
				}
				title := fmt.Sprintf("%s %s – %s (%s)", kind, abs(sp.From), abs(sp.To), compact(sp.To.Sub(sp.From)))
				if r, ok := clip(sp.From, sp.To, class, title); ok {
					hr.Rects = append(slices.Clip(hr.Rects), r)
					stocked = true
				}
			}
			h.Rows = append(h.Rows, hr)
		}
		if stocked {
			active = append(active, h)
		} else {
			idle = append(idle, h)
		}
	}
	for _, h := range idle {
		if !slices.Contains(quiet, h.Name) {
			quiet = append(quiet, h.Name)
		}
	}
	for i := range 5 {
		axis = append(axis, start.Add(watch.Window*time.Duration(i)/4).Local().Format("02.01."))
	}
	return active, quiet, axis
}

func desc(v watch.Variant) string {
	parts := []string{}
	if v.CPU != "" {
		parts = append(parts, shortCPU(v.CPU))
	}
	parts = append(parts, fmt.Sprintf("%d GB", v.RAM))
	var disks []string
	for _, d := range v.Disks {
		disks = append(disks, d.String())
	}
	if len(disks) > 0 {
		parts = append(parts, strings.Join(disks, " + "))
	}
	if v.Priced {
		parts = append(parts, money(v.Rent)+" €")
	}
	return strings.Join(parts, sep)
}

// sep is the middot in titles and headings, with an en space (U+2002) each side: plain spaces read cramped there.
const sep = " · "

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// config is the short config line for the logs: "32 GB · 2×450GB NVME".
func config(v watch.Variant) string {
	parts := []string{fmt.Sprintf("%d GB", v.RAM)}
	for _, d := range v.Disks {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, " · ")
}

func changes(wanted, all []watch.Variant, s watch.State, now time.Time) []changeRow {
	byFQN := map[string]watch.Variant{}
	for _, v := range all {
		byFQN[v.FQN] = v
	}
	label := func(fqn string) string {
		v, ok := byFQN[fqn]
		if !ok {
			return fqn
		}
		l := cmp.Or(v.Name, v.PlanCode) + " · " + config(v)
		if v.Priced {
			l += " · " + money(v.Rent) + " €"
		}
		return l
	}
	var out []changeRow
	for _, c := range s.Changes() {
		r := changeRow{At: c.At, Kind: "out", Text: label(c.FQN), DC: c.DC}
		if c.In {
			r.Kind = "in"
			if c.Slow {
				r.Extra = "72H"
			}
		} else {
			r.Extra = "after " + compact(c.For)
		}
		out = append(out, r)
	}
	for _, v := range wanted {
		ps := s.Prices[v.FQN]
		for i := 1; i < len(ps); i++ {
			// A setup-only change is in the price table; here it would read "18.99 € → 18.99 €".
			if now.Sub(ps[i].At) > watch.Window || ps[i].Rent == ps[i-1].Rent {
				continue
			}
			out = append(out, changeRow{At: ps[i].At, Kind: "price",
				Text: cmp.Or(v.Name, v.PlanCode) + " · " + config(v) + " · " + money(ps[i-1].Rent) + " € → " + money(ps[i].Rent) + " €"})
		}
	}
	slices.SortStableFunc(out, func(a, b changeRow) int { return b.At.Compare(a.At) })
	return out[:min(len(out), maxChanges)]
}

// priceChanges lists every price change, wanted variants first, newest first; all lists every variant's.
func priceChanges(vs []watch.Variant, c watch.Config, s watch.State, all bool) ([]priceRow, bool) {
	type item struct {
		r      priceRow
		wanted bool
	}
	var items []item
	for _, v := range vs {
		ps := s.Prices[v.FQN]
		for i := 1; i < len(ps); i++ {
			p, q := ps[i-1], ps[i]
			r := priceRow{At: q.At, Name: cmp.Or(v.Name, v.PlanCode), Config: config(v),
				Rent: cell{Text: money(q.Rent), Class: "sub"}, Setup: cell{Text: money(q.Setup), Class: "sub"}}
			if q.Rent != p.Rent {
				r.OldRent, r.Rent = money(p.Rent), moved(q.Rent, p.Rent)
			}
			if q.Setup != p.Setup {
				r.OldSetup, r.Setup = money(p.Setup), moved(q.Setup, p.Setup)
			}
			items = append(items, item{r, c.Wanted(v)})
		}
	}
	slices.SortStableFunc(items, func(a, b item) int {
		if a.wanted != b.wanted {
			if a.wanted {
				return -1
			}
			return 1
		}
		return b.r.At.Compare(a.r.At)
	})
	n := len(items)
	if !all {
		items = items[:min(n, maxPriceChanges)]
	}
	out := make([]priceRow, len(items))
	for i, it := range items {
		out[i] = it.r
	}
	return out, n > len(out)
}

func moved(cur, prev float64) cell {
	if cur < prev {
		return cell{Text: money(cur), Class: "x up"}
	}
	return cell{Text: money(cur), Class: "x down"}
}

func tiles(d pageData, wanted []watch.Variant, s watch.State, now time.Time) []tile {
	inStock := tile{Label: "Wanted in stock", Value: strconv.Itoa(len(d.Wanted)), Highlight: len(d.Wanted) > 0}
	var where []string
	for _, r := range d.Wanted {
		where = append(where, cmp.Or(r.Name, r.PlanCode)+" in "+strings.Join(r.InStock(), ", "))
	}
	if len(where) > 3 {
		where = append(where[:3], fmt.Sprintf("%d more", len(where)-3))
	}
	inStock.Sub = strings.Join(where, " · ")
	if len(where) == 0 {
		inStock.Sub = "nothing right now"
	} else {
		inStock.SubClass = "ok"
	}

	cheap := tile{Label: "Cheapest wanted", Value: "–", Sub: "nothing priced in stock"}
	for _, r := range d.Wanted {
		if r.Priced {
			cheap.Value, cheap.Unit = money(r.Rent), "€/mo"
			cheap.Sub = cmp.Or(r.Name, r.PlanCode)
			if r.Multi.Text != "" && r.Single.Text != "" {
				cheap.Sub += " · multi " + r.Multi.Text + " · single " + r.Single.Text
			}
			break
		}
	}

	// The newest streak among wanted pairs in stock, or the last time any was.
	streak := tile{Label: "Last wanted stock", Value: "never"}
	var since, last time.Time
	var sinceKey string
	for _, v := range wanted {
		e, ok := s.Seen[v.FQN]
		if !ok {
			continue
		}
		if dcs := v.InStock(); e.In && len(dcs) > 0 && e.Since.After(since) {
			since, sinceKey = e.Since, cmp.Or(v.Name, v.PlanCode)+" in "+strings.Join(dcs, ", ")
		}
		if e.Last.After(last) {
			last = e.Last
		}
	}
	switch {
	case !since.IsZero():
		streak.Label = "In stock since"
		streak.Value, streak.Unit = splitDur(now.Sub(since))
		when := since.Local().Format("15:04")
		if y, m, dd := since.Local().Date(); y != now.Year() || m != now.Month() || dd != now.Day() {
			when = since.Local().Format("02.01. 15:04")
		}
		streak.Sub, streak.Title = sinceKey+", "+when, abs(since)
	case !last.IsZero():
		streak.Value, streak.Unit = splitDur(now.Sub(last))
		streak.Unit += " ago"
		streak.Title = abs(last)
	default:
		if !s.Since.IsZero() {
			streak.Sub = "tracking since " + s.Since.Local().Format("02.01.2006")
		}
	}

	var n24 int
	var lens []time.Duration
	for _, c := range s.Changes() { // not d.Changes: that list is cut for the page
		if now.Sub(c.At) <= 24*time.Hour {
			n24++
		}
		if !c.In {
			lens = append(lens, c.For)
		}
	}
	moves := tile{Label: "Stock changes 24 h", Value: strconv.Itoa(n24)}
	if len(lens) > 0 {
		slices.Sort(lens)
		moves.Sub = "in stock " + compact(lens[len(lens)/2]) + " (30 d median)"
	}
	return []tile{inStock, cheap, streak, moves}
}

// rule spells a wish out: "RAM ≤ 64 GB, NVME, ≤ 30 €/mo".
func rule(w watch.Want) string {
	var p []string
	if w.RAMMin > 0 {
		p = append(p, "RAM ≥ "+itoa(w.RAMMin)+" GB")
	}
	if w.RAMMax > 0 {
		p = append(p, "RAM ≤ "+itoa(w.RAMMax)+" GB")
	}
	if w.Disk != "" {
		p = append(p, strings.ToUpper(w.Disk))
	}
	if w.DiskMin > 0 {
		p = append(p, "disk ≥ "+ftoa(w.DiskMin)+" TB")
	}
	if w.CPU != "" {
		p = append(p, "CPU “"+w.CPU+"”")
	}
	if w.PriceMax > 0 {
		p = append(p, "≤ "+ftoa(w.PriceMax)+" €/mo")
	}
	return strings.Join(p, ", ")
}

// shortCPU drops the vendor: "Intel Xeon-D 1540" → "Xeon-D 1540".
func shortCPU(s string) string {
	for _, p := range []string{"Intel ", "AMD ", "INTEL "} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// short drops a duration's zero tails: "2m0s" → "2m", "1h0m0s" → "1h", "10m0s" → "10m".
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// compact is a duration for people: "<1m", "42m", "5h", "1h 40m", "3d".
func compact(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 10*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return fmt.Sprintf("%dh %dm", h, m)
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// splitDur is a tile's big number and unit: 47 "min", 5 "h", 3 "d".
func splitDur(d time.Duration) (string, string) {
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(int(d/time.Minute), 0)), "min"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d / time.Hour)), "h"
	}
	return strconv.Itoa(int(d / (24 * time.Hour))), "d"
}

// ago is "42m ago", or "never".
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return compact(time.Since(t)) + " ago"
}

// abs is the time in TZ, for tooltips.
func abs(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func money(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// thousands groups with ' as the family does: 15'234.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "'" + s[i:]
	}
	return s
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
