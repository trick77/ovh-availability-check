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

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/watch"
)

//go:embed page.html
var pageHTML string

// iconSVG is the app icon (server rack on trakk's chip, user); iconVersion goes into its URL, it
// is cached for a year like the fonts.
//
//go:embed icon.svg
var iconSVG []byte

var iconVersion = func() string {
	h := sha256.Sum256(iconSVG)
	return hex.EncodeToString(h[:5])
}()

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
	"code":   code,
	"sub":    func(a, b int) int { return a - b },
	"rowsOf": func(rs []row, p *pageData) rowsArg { return rowsArg{Rows: rs, Page: p} },
}).Parse(pageHTML))

// rowsArg is what the "rows" template takes.
type rowsArg struct {
	Rows []row
	Page *pageData
}

const (
	maxPriceChanges = 20
)

type pageData struct {
	Title       string
	FontVersion string
	IconVersion string
	PolledAt    time.Time
	PollErr     string
	CatalogAt   time.Time
	CatErr      string
	Interval    string
	ShowAll     bool
	Datacenters []string
	DCLabels    []cell // the table headers: short name, full name as tooltip
	Channels    []string
	ChannelsOff []string // switched off with "!", by scheme
	Since       time.Time
	RefCPU      string
	Ref         *ovh.CPUInfo
	Tiles       []tile
	Wants       []wantRow
	Wanted      []row
	InStock     []row // Wanted, then the just-above ones in stock: muted, never counted (never pushed either)
	All         []row
	AllCount    int
	History     []histVariant
	HistW       int    // the history's x axis, in minutes
	HistNow     int    // where now is on it
	HistNowPct  string // the same, as a CSS left offset
	Days        int    // 30 or 7 (?days=, remembered in a cookie)
	Quiet       []string
	Almost      []almostRow
	Axis        []string
	Activity    []activityRow
	Prices      []priceRow
	MorePrices  bool
}

// cell is a table cell worked out in Go, so the template only prints: text, a CSS class, a tooltip.
type cell struct {
	Text, Class, Title string
}

type tile struct {
	Label, Value, Unit, Sub, SubClass, Title string
	Parts                                    []cell // after Sub, each in its own colour
	Highlight, Predicted                     bool
}

// wantRow is one wish: how many watched variants it takes at all, and how many are in stock. Zero at all is the
// silent case a typo or a renamed spec makes, so the page flags it.
type wantRow struct {
	Label   string
	Rule    []cell // one pill per criterion
	All     int
	Near    int // just above it
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
	Near    string // the rule it misses, when just above a wish
}

type histVariant struct {
	Name, Code, Desc string
	Near             string // the rule it misses, when just above a wish
	Was              cell
	Pattern          patternLine
	Rows             []histRow
	NoForecast       bool // no predicted window: the bar's future is hatched, without text (user)
}

type patternLine struct {
	None                    bool
	Text, Next, Hits, Title string
}

// almostRow is a server new this week that is just above a wish.
type almostRow struct {
	row
	Why    string
	Listed time.Time
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

type priceRow struct {
	At                 time.Time
	Name, Code, Config string
	Rent, Setup        cell
	OldRent, OldSetup  string // set when that part changed
}

func (w *watcher) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.page)
	mux.HandleFunc("GET /fonts/{name}", serveFont)
	if w.hub != nil {
		mux.HandleFunc("GET /events", w.events)
	}
	mux.HandleFunc("GET /icon.svg", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "image/svg+xml")
		rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = rw.Write(iconSVG)
	})
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
		FontVersion: fontVersion, IconVersion: iconVersion,
		PolledAt: w.polledAt, PollErr: w.pollErr, CatalogAt: w.catalogAt, CatErr: w.catErr,
		Interval:    short(time.Duration(w.cfg.Interval)),
		ShowAll:     r.URL.Query().Get("all") == "1",
		Datacenters: w.cfg.Datacenters, Channels: w.channels.Labels(), ChannelsOff: w.off,
		AllCount: len(vs),
		Days:     30,
	}
	w.mu.Unlock()
	d.Days = histDays(rw, r)
	preds := w.predict.All()
	for _, dc := range d.Datacenters {
		d.DCLabels = append(d.DCLabels, cell{Text: shortDC(dc), Title: dc})
	}
	now := time.Now()
	d.Since = state.Since
	if c, ok := ovh.CPU(w.cfg.ReferenceCPU); ok && c.Mark > 0 {
		d.Ref, d.RefCPU = &c, shortCPU(w.cfg.ReferenceCPU)
	}

	var wanted, scope []watch.Variant
	var above []row
	for _, v := range vs {
		want := w.cfg.Wanted(v)
		why, near := w.cfg.NearMiss(v)
		if want {
			wanted = append(wanted, v)
			if len(v.InStock()) > 0 {
				d.Wanted = append(d.Wanted, d.row(v, state, now))
			}
		}
		if want || near {
			scope = append(scope, v)
		}
		if near && len(v.InStock()) > 0 {
			r := d.row(v, state, now)
			r.Near = why
			above = append(above, r)
		}
	}
	d.InStock = append(slices.Clip(d.Wanted), above...)
	if d.ShowAll {
		for _, v := range vs {
			d.All = append(d.All, d.row(v, state, now))
		}
	}
	for i, want := range w.cfg.Want {
		var unknown []string
		if !d.CatalogAt.IsZero() { // names come with the catalog
			unknown = want.UnknownPlans(vs)
		}
		wr := wantRow{Label: want.Label(i), Rule: rule(want, unknown)}
		var matched []watch.Variant
		for _, v := range vs {
			if want.Matches(v) {
				matched = append(matched, v)
				wr.All++
				if len(v.InStock()) > 0 {
					wr.InStock++
				}
			} else if _, near := want.Near(v); near {
				wr.Near++
			}
		}
		wr.Last = lastCell(matched, state, now)
		d.Wants = append(d.Wants, wr)
	}
	window := time.Duration(d.Days) * 24 * time.Hour
	d.History, d.Quiet, d.Axis, d.HistW = history(scope, w.cfg, preds, state, now, window)
	d.HistNow = int(window / time.Minute)
	if d.HistW > 0 {
		d.HistNowPct = fmt.Sprintf("%.3f%%", float64(d.HistNow)*100/float64(d.HistW))
	}
	d.Almost = d.almost(scope, w.cfg, state, now)
	d.Activity = activity(scope, w.cfg, state, now)
	d.Prices, d.MorePrices = priceChanges(vs, w.cfg, state, d.ShowAll)
	d.Tiles = tiles(d, wanted, scope, preds, state, now)

	d.Title = "OVH restock"
	if n := len(d.Wanted); n > 0 {
		d.Title += fmt.Sprintf("%s%d in stock", sep, n)
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(rw, &d); err != nil {
		slog.Warn("page", "err", err)
	}
}

// histDays is the history's range, 30 or 7 days: ?days= picks it and a cookie keeps it for the next visit.
// No Path on the cookie: behind a proxy the page may sit under a sub-path, the browser defaults to the request's.
func histDays(rw http.ResponseWriter, r *http.Request) int {
	v := r.URL.Query().Get("days")
	if v == "7" || v == "30" {
		// Secure only over https (direct or via the proxy): a plain-http LAN visit would drop the cookie.
		secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
		http.SetCookie(rw, &http.Cookie{Name: "days", Value: v, MaxAge: 365 * 24 * 3600, HttpOnly: true, //nolint:gosec // G124: Secure per request, set above
			Secure: secure, SameSite: http.SameSiteLaxMode})
	} else if c, err := r.Cookie("days"); err == nil {
		v = c.Value
	}
	if v == "7" {
		return 7
	}
	return 30
}

func (d *pageData) row(v watch.Variant, s watch.State, now time.Time) row {
	r := row{Variant: v, CPUName: shortCPU(v.CPU), Out: len(v.InStock()) == 0}
	if v.CPU != "" {
		c, ok := ovh.CPU(v.CPU)
		if ok && c.Cores > 0 {
			r.Cores = fmt.Sprintf("%dC/%dT", c.Cores, c.Threads)
		}
		switch {
		case d.Ref == nil:
		case !ok:
			r.Multi = cell{Text: "?", Class: "x eq", Title: "not in cpus.json: " + v.CPU}
		case c.Mark == 0:
			r.Multi = cell{Text: "?", Class: "x eq", Title: "no PassMark figure: " + c.Note}
		default:
			r.Multi = pct(c.Mark, d.Ref.Mark, "CPU Mark")
			r.Single = pct(c.Single, d.Ref.Single, "single thread")
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
	// Setup: an arrow only, the old fee in the tooltip; the setup line sits under the rent and must stay narrow.
	if cur.Setup != prev.Setup {
		setup = cell{Text: "↓", Class: "pd down",
			Title: "setup was " + money(prev.Setup) + " until " + cur.At.Local().Format("02.01.2006")}
		if cur.Setup > prev.Setup {
			setup.Text, setup.Class = "↑", "pd up"
		}
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
		return cell{Text: a, Class: "pill slow", Title: "72H: orderable, delivery within 72 h"}
	case strings.HasPrefix(a, "1H"):
		// "1H-low"/"1H-high" is stock depth; the full value is in the tooltip, the column stays narrow.
		return cell{Text: "1H", Class: "pill on", Title: a + ": orderable"}
	case ovh.InStock(a):
		return cell{Text: a, Class: "pill on", Title: "orderable"}
	}
	return cell{Text: a, Class: "pill"}
}

// history is a block per in-scope variant (wanted or just above a wish) with stock in the window or a known
// pattern, a bar per DC it is listed in; the bar runs a quarter of the window past now, for predicted windows.
func history(scope []watch.Variant, c watch.Config, preds map[string]predict.Prediction, s watch.State,
	now time.Time, window time.Duration) (hv []histVariant, quiet, axis []string, width int) {
	start := now.Add(-window)
	end := now.Add(window / 4)
	mins := func(t time.Time) int { return int(t.Sub(start) / time.Minute) }
	total, past := mins(end), mins(now)
	minW := total / 1200 // about a pixel on a wide screen: a single poll must show
	clip := func(from, to time.Time, class, title string) (rect, bool) {
		x0, x1 := max(mins(from), 0), min(mins(to), past)
		if x1 < 0 || x0 > past {
			return rect{}, false
		}
		return rect{X: x0, W: max(x1-x0, minW), Class: class, Title: title}, true
	}
	var active, idle []histVariant
	for _, v := range scope {
		h := histVariant{Name: cmp.Or(v.Name, v.PlanCode), Code: code(v.Name, v.PlanCode), Desc: desc(v)}
		h.Near, _ = c.NearMiss(v)
		p, hasP := preds[v.FQN]
		h.Pattern = patternCell(p, hasP, now)
		var future []rect
		if hasP && p.Pattern && !p.Next.From.IsZero() {
			x0, x1 := max(mins(p.Next.From), past), min(mins(p.Next.To), total)
			if x1 > x0 {
				future = append(future, rect{X: x0, W: max(x1-x0, minW), Class: "h-next",
					Title: "predicted " + abs(p.Next.From) + " – " + p.Next.To.Local().Format("15:04")})
			}
		}
		// From the prediction, not the drawn window: a next window past the bar's end (7-day view) is still one.
		h.NoForecast = !hasP || !p.Pattern
		if ps := s.Prices[v.FQN]; len(ps) >= 2 && math.Abs(ps[len(ps)-1].Rent-ps[len(ps)-2].Rent) >= 0.005 {
			h.Was = cell{Text: "was " + money(ps[len(ps)-2].Rent) + " until " + ps[len(ps)-1].At.Local().Format("02.01."),
				Class: "was"}
		}
		// Sold out is drawn from when the variant was tracked on: before, only in-scope pairs had history (until
		// 0.4). That is no data, not sold out.
		tracked := s.TrackedFrom(v.FQN)
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
		for dc := range s.Spans[v.FQN] {
			if !slices.Contains(dcs, dc) {
				dcs = append(dcs, dc)
			}
		}
		slices.Sort(dcs)
		for _, dc := range dcs {
			hr := histRow{DC: shortDC(dc), Rects: base}
			for _, sp := range s.Spans[v.FQN][dc] {
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
			hr.Rects = append(slices.Clip(hr.Rects), future...)
			h.Rows = append(h.Rows, hr)
		}
		if stocked || (hasP && p.Pattern) {
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
	// Six labels, evenly spread: the window in quarters, then the quarter past now the predictions get.
	for i := range 6 {
		axis = append(axis, start.Add(window*time.Duration(i)/4).Local().Format("02.01."))
	}
	return active, quiet, axis, total
}

// patternCell is the line under a history block's name: the pattern, the next window and the hits so far; "none
// found" after a call without one; nothing below the gate (no call was made).
func patternCell(p predict.Prediction, ok bool, now time.Time) patternLine {
	switch {
	case !ok:
		return patternLine{}
	case !p.Pattern:
		// "so far": the gate lets a call through after 14 days, not 90.
		return patternLine{None: true, Text: "none found so far", Title: "asked " + abs(p.Asked)}
	}
	l := patternLine{Text: p.Summary, Title: "asked " + abs(p.Asked) + " (" + p.Model + ")"}
	if !p.Next.From.IsZero() {
		l.Next = "next likely " + when(p.Next, now)
	}
	if p.Tries > 0 {
		l.Hits = fmt.Sprintf("%d of %d windows hit", p.Hits, p.Tries)
	}
	return l
}

// when is a window for people: "Thu 28.11. 05:30–07:00", "now until 07:00", or for a long one "Sat 30.11. – Mon 02.12.".
func when(w predict.Window, now time.Time) string {
	from, to := w.From.Local(), w.To.Local()
	switch {
	case !from.After(now):
		return "now until " + to.Format("15:04")
	case to.Sub(from) > 24*time.Hour:
		return from.Format("Mon 02.01.") + " – " + to.Format("Mon 02.01.")
	}
	return from.Format("Mon 02.01. 15:04") + "–" + to.Format("15:04")
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

// inScope is scope as a filter for watch.State.Changes: the state keeps every pair, the page shows these.
func inScope(scope []watch.Variant) func(string) bool {
	in := make(map[string]bool, len(scope))
	for _, v := range scope {
		in[v.FQN] = true
	}
	return func(fqn string) bool { return in[fqn] }
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
			r := priceRow{At: q.At, Name: cmp.Or(v.Name, v.PlanCode), Code: code(v.Name, v.PlanCode), Config: config(v),
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

func tiles(d pageData, wanted, scope []watch.Variant, preds map[string]predict.Prediction, s watch.State,
	now time.Time) []tile {
	inStock := tile{Label: "Wanted in stock", Value: strconv.Itoa(len(d.Wanted)), Highlight: len(d.Wanted) > 0}
	var where []string
	for _, r := range d.Wanted {
		where = append(where, cmp.Or(r.Name, r.PlanCode)+" in "+shortDCs(r.InStock()))
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
			// Coloured like the table's columns: the figures as cells, not folded into the grey text.
			if r.Multi.Text != "" && r.Single.Text != "" {
				cheap.Parts = []cell{{Text: " · multi "}, r.Multi, {Text: " · single "}, r.Single}
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
			since, sinceKey = e.Since, cmp.Or(v.Name, v.PlanCode)+" in "+shortDCs(dcs)
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

	// A known pattern's coming window takes the third tile: it says more than when stock was last seen.
	var next predict.Window
	var nextName string
	var nextP predict.Prediction
	for _, v := range scope {
		if p, ok := preds[v.FQN]; ok && p.Pattern && !p.Next.From.IsZero() && (next.From.IsZero() || p.Next.From.Before(next.From)) {
			next, nextName, nextP = p.Next, cmp.Or(v.Name, v.PlanCode), p
		}
	}
	if !next.From.IsZero() {
		streak = tile{Label: "Next likely", Predicted: true, Title: abs(next.From) + " – " + abs(next.To)}
		lf, lt := next.From.Local(), next.To.Local()
		switch {
		case !lf.After(now):
			streak.Value, streak.Unit = "now", "until "+lt.Format("Mon 15:04")
		case lt.Sub(lf) > 24*time.Hour: // an interval's window spans days
			streak.Value, streak.Unit = lf.Format("Mon"), "– "+lt.Format("Mon")
		default:
			streak.Value, streak.Unit = lf.Format("Mon"), lf.Format("15:04")+"–"+lt.Format("15:04")
		}
		streak.Sub = nextName
		if nextP.Tries > 0 {
			streak.Sub += fmt.Sprintf(" · %d of %d windows hit", nextP.Hits, nextP.Tries)
		}
	}

	var n24 int
	var lens []time.Duration
	for _, c := range s.Changes(inScope(scope)) { // not d.Changes: that list is cut for the page
		if now.Sub(c.At) <= 24*time.Hour {
			n24++
		}
		if !c.In && now.Sub(c.At) <= watch.Window {
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

// almost are the servers new in the last 7 days that are just above a wish: priced for the first time this week,
// after the first day of tracking (the first catalog prices everything).
func (d *pageData) almost(scope []watch.Variant, c watch.Config, s watch.State, now time.Time) []almostRow {
	var out []almostRow
	for _, v := range scope {
		why, near := c.NearMiss(v)
		ps := s.Prices[v.FQN]
		if !near || len(ps) == 0 || now.Sub(ps[0].At) > 7*24*time.Hour || ps[0].At.Sub(s.Since) < 24*time.Hour {
			continue
		}
		out = append(out, almostRow{row: d.row(v, s, now), Why: why, Listed: ps[0].At})
	}
	return out
}

// rule spells a wish out, a pill per criterion: "KS-LE-3", "RAM ≤ 64 GB", "NVME", "≤ 30 €/mo". A plan in unknown
// (no watched variant has it) is flagged: a typo would wait forever.
func rule(w watch.Want, unknown []string) []cell {
	var p []cell
	add := func(s string) { p = append(p, cell{Text: s, Class: "pill"}) }
	for _, pl := range w.Plans {
		if slices.Contains(unknown, pl) {
			p = append(p, cell{Text: pl, Class: "pill bad", Title: "no watched variant has this product"})
			continue
		}
		add(pl)
	}
	if w.RAMMin > 0 {
		add("RAM ≥ " + itoa(w.RAMMin) + " GB")
	}
	if w.RAMMax > 0 {
		add("RAM ≤ " + itoa(w.RAMMax) + " GB")
	}
	if w.Disk != "" {
		add(strings.ToUpper(w.Disk))
	}
	if w.DiskMin > 0 {
		add("disk ≥ " + ftoa(w.DiskMin) + " TB")
	}
	if w.CPU != "" {
		add("CPU “" + w.CPU + "”")
	}
	if w.CPUMinPct != nil {
		add(fmt.Sprintf("CPU ≥ %+d %%", *w.CPUMinPct))
	}
	if w.PriceMax > 0 {
		add("≤ " + ftoa(w.PriceMax) + " €/mo")
	}
	return p
}

// code is the product code shown beside a name, "" when the name already is the code (no catalog name).
func code(name, plan string) string {
	if name == "" || name == plan {
		return ""
	}
	return plan
}

// shortDC drops a 3-AZ region's prefix, so seven DC columns fit beside the table: "eu-west-par-a" → "par-a".
func shortDC(dc string) string {
	if p := strings.Split(dc, "-"); len(p) >= 4 {
		return strings.Join(p[len(p)-2:], "-")
	}
	return dc
}

func shortDCs(dcs []string) string {
	out := make([]string, len(dcs))
	for i, dc := range dcs {
		out[i] = shortDC(dc)
	}
	return strings.Join(out, ", ")
}

// shortCPU drops the vendor and spells a socket count one way: "Intel Xeon-D 1540" → "Xeon-D 1540",
// "2x AMD Epyc 9354" and "Dual AMD EPYC 9965" → "2× Epyc 9354", "2× EPYC 9965".
func shortCPU(s string) string {
	count := ""
	for _, p := range []string{"2x ", "Dual "} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			count, s = "2× ", rest
		}
	}
	for _, p := range []string{"Intel ", "AMD ", "INTEL "} {
		s = strings.TrimPrefix(s, p)
	}
	return count + s
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
