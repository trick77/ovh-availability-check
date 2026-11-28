package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/watch"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const every = 2 * time.Minute

func at(n int) time.Time { return t0.Add(time.Duration(n) * every) }

func variant(fqn string, stock map[string]string) watch.Variant {
	return watch.Variant{FQN: fqn, PlanCode: "24sk202", Name: "KS-2", CPU: "Intel Xeon-D 1540", RAM: 64,
		Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Rent: 26.99, Priced: true, Stock: stock}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// same compares what the store keeps (everything but the memory-only fields).
func same(t *testing.T, got, want watch.State) {
	t.Helper()
	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range g.NumField() {
		f := g.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if !reflect.DeepEqual(g.Field(i).Interface(), w.Field(i).Interface()) {
			t.Errorf("%s:\n got %+v\nwant %+v", f.Name, g.Field(i).Interface(), w.Field(i).Interface())
		}
	}
}

// writes counts the rows written to the stock tables, by triggers: what a poll costs.
func writes(t *testing.T, s *Store) func() int {
	t.Helper()
	ctx := t.Context()
	stmts := []string{`CREATE TEMP TABLE w (n INTEGER)`, `INSERT INTO w VALUES (0)`}
	for _, tb := range []string{"span", "seen", "polled", "price", "event", "notified"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			stmts = append(stmts, `CREATE TEMP TRIGGER w_`+tb+`_`+op+` AFTER `+op+` ON main.`+tb+` BEGIN UPDATE w SET n = n + 1; END`)
		}
	}
	s.DB.SetMaxOpenConns(1) // TEMP objects live on one connection
	for _, q := range stmts {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	return func() int {
		var n int
		if err := s.DB.QueryRowContext(ctx, `SELECT n FROM w`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := t.Context()
	s := open(t, t.TempDir())
	a := variant("a", map[string]string{"gra": "1H-low", "rbx": "unavailable"})
	b := variant("b", map[string]string{"gra": "unavailable"})
	st := watch.State{}
	saved := watch.State{}
	step := func(n int, vs ...watch.Variant) {
		t.Helper()
		st = st.Observe(vs, at(n), every).ObservePrices(vs, at(n))
		if err := s.Save(ctx, saved, st); err != nil {
			t.Fatal(err)
		}
		saved = st
		got, err := s.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		same(t, got, st)
	}
	step(0, a, b)
	step(1, a, b)
	// b comes in at 1H, and a poll later switches to 72H: the switch span starts at the first span's to, which
	// equals its from (the key needs slow).
	b.Stock["gra"] = "1H-low"
	step(2, a, b)
	b.Stock["gra"] = "72H"
	step(3, a, b)
	a.Stock["gra"], a.Rent = "unavailable", 22.99
	step(4, a, b)
	st = st.AddEvent(watch.Event{At: at(4), Kind: watch.KindRestock, Title: "t", Body: "b\nhttps://x", Delivery: "failed on ntfy",
		Retry: true, Items: watch.Items([]watch.Variant{b}, []watch.Pair{{FQN: "b", DC: "gra"}})})
	st = st.Next(map[string]watch.Pair{"b@gra": {FQN: "b", DC: "gra"}}, []watch.Pair{{FQN: "b", DC: "gra"}})
	st.DigestAt = at(4)
	step(5, a, b)
	if sp := st.Spans["b"]["gra"]; len(sp) != 2 || !sp[0].From.Equal(sp[1].From) {
		t.Fatalf("no switch at one poll: %+v", sp)
	}
	// b unlisted: its open span stops without a gone, and the row gets its to.
	step(6, a)
	// A gap: every open stretch ends.
	step(40, a)
	// Retention: everything older than Keep goes, rows too.
	st.Notified = nil
	step(int(watch.Keep/every)+100, a)
	if len(st.Spans) != 0 {
		t.Errorf("spans kept: %+v", st.Spans)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM span`).Scan(&n); err != nil || n != 0 {
		t.Errorf("span rows %d %v", n, err)
	}
}

func TestOnlyChanges(t *testing.T) {
	ctx := t.Context()
	s := open(t, t.TempDir())
	count := writes(t, s)
	var vs []watch.Variant
	for _, f := range []string{"a", "b", "c", "d"} {
		vs = append(vs, variant(f, map[string]string{"gra": "1H-low", "rbx": "72H", "sbg": "unavailable"}))
	}
	st := watch.State{}.Observe(vs, at(0), every).ObservePrices(vs, at(0))
	if err := s.Save(ctx, watch.State{}, st); err != nil {
		t.Fatal(err)
	}
	first := count()
	for i := 1; i <= 10; i++ {
		next := st.Observe(vs, at(i), every).ObservePrices(vs, at(i))
		if err := s.Save(ctx, st, next); err != nil {
			t.Fatal(err)
		}
		st = next
	}
	// Ten polls, same stock: nothing but meta.
	if n := count() - first; n != 0 {
		t.Errorf("%d stock rows written by polls that changed nothing", n)
	}
	// One pair sells out: its span's row (to, gone) and the variant's seen row stay, only that span changes.
	vs[0].Stock["gra"] = "unavailable"
	before := count()
	next := st.Observe(vs, at(11), every)
	if err := s.Save(ctx, st, next); err != nil {
		t.Fatal(err)
	}
	if n := count() - before; n != 1 {
		t.Errorf("one sell-out wrote %d rows", n)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, next)
}

func TestPredictionsAndHello(t *testing.T) {
	ctx := t.Context()
	s := open(t, t.TempDir())
	p := predict.Prediction{Asked: at(0), Model: "m", Pattern: true, Kind: "weekly", Summary: "Tue 06:00",
		Next: predict.Window{From: at(10), To: at(20)}, Hits: 1, Tries: 2}
	if err := s.SavePredictions(ctx, map[string]predict.Prediction{"a": p, "b": {}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePredictions(ctx, nil, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Predictions(ctx)
	if err != nil || len(got) != 1 || got["a"].Summary != p.Summary || !got["a"].Next.From.Equal(p.Next.From) {
		t.Errorf("predictions %+v %v", got, err)
	}
	if err := s.SaveHello(ctx, map[string]time.Time{"k": at(1)}); err != nil {
		t.Fatal(err)
	}
	if h, err := s.Hello(ctx); err != nil || len(h) != 1 || !h["k"].Equal(at(1)) {
		t.Errorf("hello %v %v", h, err)
	}
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		legacyState: `{"notified":["a@gra"],"since":"2026-11-01T12:00:00Z","polled_at":"2026-11-01T12:04:00Z",
			"seen":{"a":{"since":"2026-11-01T12:00:00Z","last":"2026-11-01T12:04:00Z","in":true}},
			"polled":[{"from":"2026-11-01T12:00:00Z","to":"2026-11-01T12:04:00Z"}],
			"spans":{"a@gra":[{"from":"2026-11-01T12:00:00Z","to":"2026-11-01T12:04:00Z","quiet":true}],
				"b@rbx":[{"from":"2026-11-01T12:00:00Z","to":"2026-11-01T12:02:00Z","gone":"2026-11-01T12:04:00Z","slow":true}]},
			"pushes":[{"at":"2026-11-01T12:04:00.123Z","title":"OVH week 44","body":"x"},
				{"at":"2026-11-01T12:00:00Z","title":"OVH: 1 server in stock","body":"KS-2\nhttps://eco.ovhcloud.com/fr/kimsufi/ks-2/","err":"failed on channel 1 (ntfy)"}],
			"digest_at":"2026-11-01T12:04:00Z"}`,
		legacyPrices:      `{"a":[{"at":"2026-11-01T12:00:00Z","rent":18.99,"setup":0}]}`,
		legacyPredictions: `{"a":{"asked":"2026-11-01T12:00:00Z","basis":"0001-01-01T00:00:00Z","model":"m","pattern":false,"hits":0,"tries":0}}`,
		legacyHello:       `{"abc":"2026-11-01T12:00:00Z"}`,
	}
	for f, b := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := open(t, dir)
	st, err := s.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notified) != 1 || !st.Seen["a"].In || len(st.Polled) != 1 || !st.PolledAt.Equal(at(2)) ||
		!st.DigestAt.Equal(at(2)) || len(st.Prices["a"]) != 1 {
		t.Errorf("state %+v", st)
	}
	if sp := st.Spans["a"]["gra"]; len(sp) != 1 || !sp[0].To.Equal(at(2)) || !sp[0].Quiet {
		t.Errorf("open span %+v", sp)
	}
	if sp := st.Spans["b"]["rbx"]; len(sp) != 1 || !sp[0].Gone.Equal(at(2)) || !sp[0].Slow {
		t.Errorf("gone span %+v", sp)
	}
	if len(st.Events) != 2 || st.Events[0].Kind != watch.KindDigest || !st.Events[0].At.Equal(at(2)) ||
		st.Events[0].Delivery != watch.Sent || st.Events[1].Delivery != "failed on channel 1 (ntfy)" {
		t.Errorf("events %+v", st.Events)
	}
	if p, _ := s.Predictions(t.Context()); len(p) != 1 {
		t.Errorf("predictions %v", p)
	}
	if h, _ := s.Hello(t.Context()); len(h) != 1 {
		t.Errorf("hello %v", h)
	}
	for f := range files {
		if _, err := os.Stat(filepath.Join(dir, f+".imported")); err != nil {
			t.Errorf("%s not renamed: %v", f, err)
		}
	}
	// A state.json back in place (a rename that failed) is not imported twice.
	if err := os.WriteFile(filepath.Join(dir, legacyState), []byte(`{"notified":["z@gra"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = open(t, dir)
	if st, _ := s.Load(t.Context()); len(st.Notified) != 1 || st.Notified[0] != "a@gra" {
		t.Errorf("imported twice: %v", st.Notified)
	}
}

func TestOpenEmpty(t *testing.T) {
	s := open(t, t.TempDir())
	st, err := s.Load(context.Background())
	if err != nil || !st.PolledAt.IsZero() || len(st.Spans) != 0 {
		t.Errorf("empty %+v %v", st, err)
	}
}
