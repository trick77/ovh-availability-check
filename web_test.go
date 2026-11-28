package main

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/watch"
)

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Minute: "2m", 10 * time.Minute: "10m", time.Hour: "1h", 90 * time.Second: "1m30s",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestPage(t *testing.T) {
	ch, err := notify.NewChannels([]string{"ntfy://user:s3cret@ntfy.example.com/topic"})
	if err != nil {
		t.Fatal(err)
	}
	ks2 := watch.Variant{FQN: "24sk202.x", PlanCode: "24sk202", Name: "KS-2", CPU: "Intel Xeon-D 1540", RAM: 64,
		Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Rent: 26.99, Priced: true,
		Stock: map[string]string{"gra": "72H"}, OrderURL: "https://eco.ovhcloud.com/fr/kimsufi/ks-2/"}
	gone := ks2
	gone.FQN, gone.Name, gone.Stock = "gone", "KS-GONE", map[string]string{"gra": "unavailable"}
	cfg := watch.Config{Datacenters: []string{"gra"}, Interval: watch.Duration(2 * time.Minute),
		Want: []watch.Want{{Name: "cheap", PriceMax: 30}, {Name: "typo", CPU: "pentium"},
			{Name: "typo plan", Plans: []string{"KS-9"}}},
		ReferenceCPU: "Intel Xeon E3-1230 v6"}
	// Three hours of history: KS-2 came into gra an hour ago, after a price cut.
	now := time.Now()
	vs := []watch.Variant{ks2, gone}
	before := []watch.Variant{gone}
	var s watch.State
	for t := now.Add(-3 * time.Hour); t.Before(now.Add(-time.Hour)); t = t.Add(2 * time.Minute) {
		s = s.Observe(before, t, 2*time.Minute)
	}
	old := ks2
	old.Rent = 28.99
	s = s.ObservePrices([]watch.Variant{old}, now.Add(-3*time.Hour))
	s = s.ObservePrices(vs, now.Add(-2*time.Hour))
	for t := now.Add(-time.Hour); !t.After(now); t = t.Add(2 * time.Minute) {
		s = s.Observe(vs, t, 2*time.Minute)
	}
	s = s.AddEvent(watch.Event{At: now.Add(-time.Hour), Kind: watch.KindRestock, Title: "OVH: 1 server in stock",
		Body: "KS-2 · gra\nhttps://eco.ovhcloud.com/fr/kimsufi/ks-2/", Delivery: "failed on channel 1 (ntfy)", Retry: true,
		Items: watch.Items(vs, []watch.Pair{{FQN: ks2.FQN, DC: "gra"}})})
	// An imported push: text only, its link shown short.
	s = s.AddEvent(watch.Event{At: now.Add(-2 * time.Hour), Kind: watch.KindRestock, Title: "OVH: 1 server in stock",
		Body: "KS-1 · rbx\nhttps://eco.ovhcloud.com/fr/kimsufi/ks-1/", Delivery: watch.NoChannel})
	w := &watcher{cfg: cfg, channels: ch, off: []string{"smtp"}, variants: vs, polledAt: now, catalogAt: now, state: s}
	get := func(path string) string {
		rec := httptest.NewRecorder()
		w.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	body := get("/")
	for _, want := range []string{"KS-2", "26.99", "every 2m", `class="zero"`, "Show all 2 variants",
		"failed on channel 1 (ntfy)", "<title>OVH restock · 1 in stock</title>",
		"8C/16T", `<span class="x `, // cores and the speed against the reference
		`class="pill slow"`, "now · 1h", // 72H in gra, in stock for an hour
		`class="h-slow"`, `class="h-out"`, // history: in stock and sold out
		`<span class="kind in">in</span>`, `<span class="kind price">price</span>`,
		"↓2.00", "28.99", // the price cut
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	_, section, _ := strings.Cut(body, "<h2>Wanted and in stock</h2>")
	section, _, _ = strings.Cut(section, "<h2>Wishes</h2>")
	if strings.Contains(section, "KS-GONE") {
		t.Error("/ lists a sold-out variant under wanted")
	}
	if strings.Contains(body, "s3cret") || strings.Contains(body, "ntfy.example.com") {
		t.Error("page leaks the notify URL")
	}
	for _, want := range []string{"<h2>Activity</h2>", `notify <span class="pill">channel 1 (ntfy)</span>`, "<s>smtp</s> off",
		"Last 30 days", `href="?days=7"`, "Just above", "multi </span><span class=\"x up\"", // the tile's figures coloured like the table
		`<span class="code">24sk202</span>`, // the product code beside the name
		`<span class="pill bad" title="no watched variant has this product">KS-9</span>`,
		`<span class="pill">≤ 30 €/mo</span>`,
		"failed on channel 1 (ntfy) · retrying", `<span class="pill faint"`, // deliveries
		`<span class="pill slow" title="gra: 72H">gra</span>`, // the restock's DC in the log
		"<noscript><meta http-equiv=\"refresh\" content=\"60\"></noscript>", `new EventSource("events")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	// A push is no line of its own: the imported one about KS-1 (no "in" to sit on) shows nowhere.
	for _, gone := range []string{"<h2>Updates</h2>", "<h2>Stock changes</h2>", "<details", "no notification channel", "KS-1"} {
		if strings.Contains(body, gone) {
			t.Errorf("/ still has %q", gone)
		}
	}
	// Activity: KS-2's restock is one "in" line carrying its push's delivery, not a second line for the push; no cart.
	_, act, _ := strings.Cut(body, "<h2>Activity</h2>")
	act, _, _ = strings.Cut(act, "</section>")
	if n := strings.Count(act, "failed on channel 1 (ntfy) · retrying"); n != 1 {
		t.Errorf("delivery shown %d times in Activity", n)
	}
	if strings.Contains(act, `aria-label="order"`) {
		t.Error("cart in Activity")
	}
	// Wanted: the cart right after the name; code and cores on the second line.
	_, wanted, _ := strings.Cut(body, "<h2>Wanted and in stock</h2>")
	wanted, _, _ = strings.Cut(wanted, "<h2>Wishes</h2>")
	for _, want := range []string{`KS-2</span><a class="ord" href="https://eco.ovhcloud.com/fr/kimsufi/ks-2/"`,
		`<span class="l2"><span class="code">24sk202</span></span>`, `<span class="l2 ct">8C/16T</span>`} {
		if !strings.Contains(wanted, want) {
			t.Errorf("wanted lacks %q", want)
		}
	}
	if strings.Contains(body, `<a class="name"`) || strings.Contains(body, "order ↗") {
		t.Error("/ still has the hidden name link or the order text")
	}
	if strings.Contains(body, ">https://") {
		t.Error("/ shows a full link")
	}
	if strings.Contains(body, "Recent pushes") || strings.Contains(body, "push: ") {
		t.Error("/ still says push")
	}
	if seven := get("/?days=7"); !strings.Contains(seven, "Last 7 days") || !strings.Contains(seven, "<b>7 days</b>") {
		t.Error("?days=7 not the 7-day view")
	}
	// The range sticks: ?days= sets a cookie, a plain visit reads it, ?days=30 switches back.
	cookieGet := func(path, days string) (string, []*http.Cookie) {
		req := httptest.NewRequest("GET", path, nil)
		if days != "" {
			req.AddCookie(&http.Cookie{Name: "days", Value: days})
		}
		rec := httptest.NewRecorder()
		w.routes().ServeHTTP(rec, req)
		return rec.Body.String(), rec.Result().Cookies()
	}
	if _, cs := cookieGet("/?days=7", ""); len(cs) != 1 || cs[0].Name != "days" || cs[0].Value != "7" {
		t.Errorf("?days=7 sets cookies %v", cs)
	}
	if b, cs := cookieGet("/", "7"); !strings.Contains(b, "Last 7 days") || len(cs) != 0 {
		t.Error("cookie days=7 not the 7-day view, or set again")
	}
	if b, cs := cookieGet("/?days=30", "7"); !strings.Contains(b, "Last 30 days") || len(cs) != 1 || cs[0].Value != "30" {
		t.Error("?days=30 does not win over the cookie")
	}
	if b, _ := cookieGet("/", "x"); !strings.Contains(b, "Last 30 days") {
		t.Error("a bad cookie is not the 30-day view")
	}
	if !strings.Contains(body, `class="nofc"`) {
		t.Error("no pattern, yet the future is not hatched")
	}

	// A known pattern: the line under the name, the next window in the bar and the tile.
	next := predict.Window{From: now.Add(20 * time.Hour), To: now.Add(21 * time.Hour)}
	preds := func(next predict.Window) *predict.Service {
		t.Helper()
		svc, err := predict.New(t.Context(), predict.Options{Store: predStore{ks2.FQN: {Pattern: true, Kind: predict.Weekly,
			Summary: "back Tue and Thu around 06:00", Next: next, Hits: 2, Tries: 3, Asked: now, Model: "m"}}})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	w.predict = preds(next)
	body = get("/")
	for _, want := range []string{"back Tue and Thu around 06:00", "next likely", "2 of 3 windows hit",
		`class="h-next"`, "Next likely"} {
		if !strings.Contains(body, want) {
			t.Errorf("prediction: / lacks %q", want)
		}
	}
	// The pattern's history block: from its pattern line to the next block. No hatch, also in the 7-day view
	// where the next window lies past the bar's end.
	ks2Block := func(body string) string {
		_, b, _ := strings.Cut(body, `<span class="lbl">Pattern</span><span>back Tue and Thu around 06:00`)
		b, _, _ = strings.Cut(b, `<div class="hv">`)
		return b
	}
	if b := ks2Block(body); !strings.Contains(b, `class="hrow"`) || strings.Contains(b, `class="nofc"`) {
		t.Error("a variant with a predicted window is hatched")
	}
	w.predict = preds(predict.Window{From: now.Add(72 * time.Hour), To: now.Add(73 * time.Hour)})
	if b := ks2Block(get("/?days=7")); !strings.Contains(b, `class="hrow"`) || strings.Contains(b, `class="nofc"`) {
		t.Error("7 days: a pattern whose next window is past the bar is hatched")
	}
	w.predict = nil

	all := get("/?all=1")
	if !strings.Contains(all, "KS-GONE") || !strings.Contains(all, `class="out"`) {
		t.Error("?all=1 lacks the sold-out variant")
	}
	if !strings.Contains(all, `href="?days=7&all=1"`) {
		t.Error("?all=1: the range link drops all=1")
	}
	if !strings.Contains(all, `title="unavailable">✕</span>`) {
		t.Error("?all=1 lacks the unavailable mark")
	}
	// Not orderable: no cart.
	if _, gone, ok := strings.Cut(all, `title="gone">KS-GONE</span>`); !ok || strings.HasPrefix(gone, `<a class="ord"`) {
		t.Error("?all=1: a sold-out server has a cart")
	}

	if !strings.Contains(body, `<link rel="icon" type="image/svg+xml" href="icon.svg?v=`+iconVersion+`">`) {
		t.Error("/ lacks the app icon")
	}
	rec := httptest.NewRecorder()
	w.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/icon.svg", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("icon: %d %q", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	w.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/fonts/SansWebVariable-TextRegular.woff2", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("font: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// predStore is a predict.Store that only loads.
type predStore map[string]predict.Prediction

func (p predStore) Predictions(context.Context) (map[string]predict.Prediction, error) {
	return maps.Clone(p), nil
}

func (predStore) SavePredictions(context.Context, map[string]predict.Prediction, []string) error {
	return nil
}

// Just above a wish and in stock: under the wanted ones, muted, with what it misses; not counted. Sold out: absent.
func TestPageNear(t *testing.T) {
	base := watch.Variant{PlanCode: "24sk202", CPU: "Intel Xeon-D 1540", RAM: 64, Priced: true,
		Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Stock: map[string]string{"gra": "1H-low"},
		OrderURL: "https://eco.ovhcloud.com/fr/kimsufi/ks-2/"}
	wanted, near, gone := base, base, base
	wanted.FQN, wanted.Name, wanted.Rent = "w", "KS-WANT", 26.99
	near.FQN, near.Name, near.Rent = "n", "KS-NEAR", 31.99
	gone.FQN, gone.Name, gone.Rent, gone.Stock = "g", "KS-GONE", 31.99, map[string]string{"gra": "unavailable"}
	cfg := watch.Config{Datacenters: []string{"gra"}, Interval: watch.Duration(2 * time.Minute),
		Want: []watch.Want{{Name: "cheap", PriceMax: 30}}}
	now := time.Now()
	get := func(vs []watch.Variant) string {
		s := watch.State{}.Observe(vs, now, 2*time.Minute)
		w := &watcher{cfg: cfg, variants: vs, polledAt: now, catalogAt: now, state: s}
		rec := httptest.NewRecorder()
		w.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "<title>OVH restock") {
			t.Fatalf("no page: %d", rec.Code)
		}
		return body
	}
	section := func(body string) string {
		_, s, _ := strings.Cut(body, "<h2>Wanted and in stock</h2>")
		s, _, _ = strings.Cut(s, "<h2>Wishes</h2>")
		return s
	}
	body := get([]watch.Variant{near, gone, wanted})
	s := section(body)
	if !strings.Contains(body, "<title>OVH restock"+sep+"1 in stock</title>") {
		t.Error("the just-above one is counted in the title")
	}
	w, n := strings.Index(s, "KS-WANT"), strings.Index(s, "KS-NEAR")
	if w < 0 || n < w {
		t.Errorf("KS-NEAR not under KS-WANT (%d, %d)", w, n)
	}
	for _, want := range []string{`<tr class="above">`, "just above: &#43;1.99 € over 30"} {
		if !strings.Contains(s, want) {
			t.Errorf("wanted section lacks %q", want)
		}
	}
	if strings.Count(s, `<tr class="above">`) != 1 || strings.Contains(s, "KS-GONE") || strings.Contains(s, "Nothing wanted") {
		t.Error("wrong rows in the wanted section")
	}
	// Nothing wanted in stock: the line says so, the just-above one still shows below it.
	s = section(get([]watch.Variant{near}))
	if !strings.Contains(s, "Nothing wanted is in stock right now.") || !strings.Contains(s, `<tr class="above">`) {
		t.Error("without wanted stock: no empty line or no just-above row")
	}
}

func TestCompact(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "<1m", 42 * time.Minute: "42m", 100 * time.Minute: "1h 40m", 5 * time.Hour: "5h",
		30 * time.Hour: "30h", 72 * time.Hour: "3d",
	} {
		if got := compact(d); got != want {
			t.Errorf("compact(%s) = %q, want %q", d, got, want)
		}
	}
	for in, want := range map[string]string{"Intel Xeon-D 1540": "Xeon-D 1540", "2x AMD Epyc 9354": "2× Epyc 9354",
		"Dual Intel Xeon Gold 6132": "2× Xeon Gold 6132"} {
		if got := shortCPU(in); got != want {
			t.Errorf("shortCPU(%s) = %q", in, got)
		}
	}
	for dc, want := range map[string]string{"eu-west-par-a": "par-a", "gra": "gra", "us-east-vin": "us-east-vin"} {
		if got := shortDC(dc); got != want {
			t.Errorf("shortDC(%s) = %q", dc, got)
		}
	}
	if got := thousands(15234); got != "15'234" {
		t.Errorf("thousands: %q", got)
	}
	for _, tc := range []struct {
		v, ref int
		want   string
	}{{120, 100, "↑ 20 %"}, {60, 100, "↓ 40 %"}, {103, 100, "≈"}} {
		if got := pct(tc.v, tc.ref, "x").Text; got != tc.want {
			t.Errorf("pct(%d, %d) = %q", tc.v, tc.ref, got)
		}
	}
}
