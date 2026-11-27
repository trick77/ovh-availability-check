package main

import (
	_ "embed"
	"html/template"
	"log/slog"
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

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
	"instock": ovh.InStock,
	"join":    strings.Join,
	"dict2": func(vs []watch.Variant, dcs []string) rows {
		return rows{Variants: vs, DCs: dcs}
	},
}).Parse(pageHTML))

// rows is what the "rows" template takes: a table of variants with a column per datacenter.
type rows struct {
	Variants []watch.Variant
	DCs      []string
}

// wantRow is one wish on the page: how many watched variants it takes at all, and how many are in stock. Zero at all
// is the silent case a typo or a renamed spec makes, so the page flags it.
type wantRow struct {
	Label   string
	Rule    string
	All     int
	InStock int
}

type pageData struct {
	Refresh     int
	PolledAt    time.Time
	PollErr     string
	CatalogAt   time.Time
	CatErr      string
	Interval    string
	ShowAll     bool
	Datacenters []string
	Channels    []string
	Wants       []wantRow
	Wanted      []watch.Variant
	All         []watch.Variant
	Pushes      []push
}

func (w *watcher) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.page)
	httpx.Health(mux, slog.Default())
	return mux
}

func (w *watcher) page(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	d := pageData{
		Refresh: 60, PolledAt: w.polledAt, PollErr: w.pollErr, CatalogAt: w.catalogAt, CatErr: w.catErr,
		Interval:    short(time.Duration(w.cfg.Interval)),
		ShowAll:     r.URL.Query().Get("all") == "1",
		Datacenters: w.cfg.Datacenters, Channels: w.channels.Labels(),
		All: w.variants, Pushes: slices.Clone(w.pushes),
	}
	w.mu.Unlock()

	for i, want := range w.cfg.Want {
		row := wantRow{Label: want.Label(i), Rule: rule(want)}
		for _, v := range d.All {
			if want.Matches(v) {
				row.All++
				if len(v.InStock()) > 0 {
					row.InStock++
				}
			}
		}
		d.Wants = append(d.Wants, row)
	}
	for _, v := range d.All {
		if len(v.InStock()) > 0 && w.cfg.Wanted(v) {
			d.Wanted = append(d.Wanted, v)
		}
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(rw, d); err != nil {
		slog.Warn("page", "err", err)
	}
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

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
