package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		Stock: map[string]string{"gra": "72H"}}
	gone := ks2
	gone.FQN, gone.Name, gone.Stock = "gone", "KS-GONE", map[string]string{"gra": "unavailable"}
	cfg := watch.Config{Datacenters: []string{"gra"}, Interval: watch.Duration(2 * time.Minute),
		Want:         []watch.Want{{Name: "cheap", PriceMax: 30}, {Name: "typo", CPU: "pentium"}},
		ReferenceCPU: "Intel Xeon E3-1230 v6"}
	// Three hours of history: KS-2 came into gra an hour ago, after a price cut.
	now := time.Now()
	vs := []watch.Variant{ks2, gone}
	before := []watch.Variant{gone}
	var s watch.State
	for t := now.Add(-3 * time.Hour); t.Before(now.Add(-time.Hour)); t = t.Add(2 * time.Minute) {
		s = s.Observe(before, cfg.Wanted, t, 2*time.Minute)
	}
	old := ks2
	old.Rent = 28.99
	s, _ = s.ObservePrices([]watch.Variant{old}, now.Add(-3*time.Hour))
	s, _ = s.ObservePrices(vs, now.Add(-2*time.Hour))
	for t := now.Add(-time.Hour); !t.After(now); t = t.Add(2 * time.Minute) {
		s = s.Observe(vs, cfg.Wanted, t, 2*time.Minute)
	}
	s = s.AddPush(watch.Push{At: now, Title: "OVH: 1 server in stock", Body: "KS-2", Err: "failed on channel 1 (ntfy)"})
	w := &watcher{cfg: cfg, channels: ch, off: []string{"smtp"}, variants: vs, polledAt: now, state: s}
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
		`<span class="ev in">in</span>`, `<span class="ev price">price</span>`,
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
	for _, want := range []string{"<h2>Notifications</h2>", "notify: ", ">smtp</s> off", "Last 30 days", `href="?days=7"`,
		"Just above", "multi </span><span class=\"x up\""} { // the tile's figures coloured like the table
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	if strings.Contains(body, "Recent pushes") || strings.Contains(body, "push: ") {
		t.Error("/ still says push")
	}
	if seven := get("/?days=7"); !strings.Contains(seven, "Last 7 days") || !strings.Contains(seven, "<b>7 days</b>") {
		t.Error("?days=7 not the 7-day view")
	}

	// A known pattern: the line under the name, the next window in the bar and the tile.
	dir := t.TempDir()
	next := predict.Window{From: now.Add(20 * time.Hour), To: now.Add(21 * time.Hour)}
	b, _ := json.Marshal(map[string]predict.Prediction{ks2.FQN: {Pattern: true, Kind: predict.Weekly,
		Summary: "back Tue and Thu around 06:00", Next: next, Hits: 2, Tries: 3, Asked: now, Model: "m"}})
	if err := os.WriteFile(filepath.Join(dir, "p.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if w.predict, err = predict.New(predict.Options{Path: filepath.Join(dir, "p.json")}); err != nil {
		t.Fatal(err)
	}
	body = get("/")
	for _, want := range []string{"back Tue and Thu around 06:00", "next likely", "2 of 3 windows hit",
		`class="h-next"`, "Next likely"} {
		if !strings.Contains(body, want) {
			t.Errorf("prediction: / lacks %q", want)
		}
	}
	w.predict = nil

	all := get("/?all=1")
	if !strings.Contains(all, "KS-GONE") || !strings.Contains(all, `class="out"`) {
		t.Error("?all=1 lacks the sold-out variant")
	}
	if !strings.Contains(all, `title="unavailable">✕</span>`) {
		t.Error("?all=1 lacks the unavailable mark")
	}

	rec := httptest.NewRecorder()
	w.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/fonts/SansWebVariable-TextRegular.woff2", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("font: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
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
