package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const every = 2 * time.Minute

func at(n int) time.Time { return t0.Add(time.Duration(n) * every) }

func stocked(a string) []Variant {
	v := ks2
	v.Stock = map[string]string{"gra": a}
	return []Variant{v}
}

func all(Variant) bool  { return true }
func none(Variant) bool { return false }

var gra = Key(ks2.FQN, "gra")

func TestObserveStreaks(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), all, at(0), every) // first run: seeded, no change
	if got := s.Changes(); len(got) != 0 {
		t.Errorf("first run logged %v", got)
	}
	s = s.Observe(stocked("1H-low"), all, at(1), every)
	s = s.Observe(stocked("unavailable"), all, at(2), every) // out
	s = s.Observe(stocked("72H"), all, at(3), every)         // in, slow
	s = s.Observe(stocked("1H-high"), all, at(4), every)     // 72H → 1H: same streak

	if e := s.Seen[ks2.FQN]; !e.In || !e.Since.Equal(at(3)) || !e.Last.Equal(at(4)) {
		t.Errorf("seen %+v", e)
	}
	want := []Span{{From: at(0), To: at(1), Gone: at(2), Quiet: true}, {From: at(3), To: at(3), Slow: true},
		{From: at(3), To: at(4)}}
	if !reflect.DeepEqual(s.Spans[gra], want) {
		t.Errorf("spans %+v", s.Spans[gra])
	}
	// The out is timed at the poll that found it gone.
	got := s.Changes()
	if len(got) != 2 || !got[0].In || !got[0].Slow || !got[0].At.Equal(at(3)) ||
		got[1].In || !got[1].At.Equal(at(2)) || got[1].For != 2*every {
		t.Errorf("changes %+v", got)
	}
	if !reflect.DeepEqual(s.Polled, []Span{{From: at(0), To: at(4)}}) {
		t.Errorf("polled %+v", s.Polled)
	}
}

func TestObserveGap(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), all, at(0), every)
	s = s.Observe(stocked("1H-low"), all, at(1), every)
	// Down for an hour: the streak and the span end, and the return logs no "in" nobody saw.
	s = s.Observe(stocked("1H-low"), all, at(31), every)
	if e := s.Seen[ks2.FQN]; !e.Since.Equal(at(31)) {
		t.Errorf("streak survived the gap: %+v", e)
	}
	if len(s.Spans[gra]) != 2 || len(s.Polled) != 2 {
		t.Errorf("spans %+v polled %+v", s.Spans[gra], s.Polled)
	}
	if got := s.Changes(); len(got) != 0 {
		t.Errorf("gap logged %v", got)
	}
}

func TestObserveWantedChanges(t *testing.T) {
	// Wanted, then not (a price rise) while still in stock: the span runs on, no "out".
	s := State{}.Observe(stocked("1H-low"), all, at(0), every)
	s = s.Observe(stocked("1H-low"), all, at(1), every)
	s = s.Observe(stocked("1H-low"), none, at(2), every)
	if sp := s.Spans[gra]; len(sp) != 1 || !sp[0].To.Equal(at(2)) || !sp[0].Gone.IsZero() {
		t.Errorf("no longer wanted: %+v", sp)
	}
	// Not wanted, then wanted while in stock: it came in unseen, no "in".
	s = State{}.Observe(stocked("1H-low"), none, at(0), every)
	s = s.Observe(stocked("1H-low"), none, at(1), every)
	s = s.Observe(stocked("1H-low"), all, at(2), every)
	if got := s.Changes(); len(got) != 0 {
		t.Errorf("turned wanted: %+v", got)
	}
	// A DC dropped from the config: the pair is no longer listed, its span stops without an "out".
	d := State{}.Observe(stocked("1H-low"), all, at(0), every)
	d = d.Observe(nil, all, at(1), every)
	if sp := d.Spans[gra]; len(sp) != 1 || !sp[0].Gone.IsZero() {
		t.Errorf("unlisted logged gone: %+v", sp)
	}
	// A slow poll is no gap.
	s = s.Observe(stocked("1H-low"), all, at(2).Add(gapAfter*every+4*time.Minute), every)
	if sp := s.Spans[gra]; len(sp) != 1 {
		t.Errorf("slow poll broke the span: %+v", sp)
	}
}

func TestObserveOnlyWantedSpans(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), none, at(0), every)
	if len(s.Spans) != 0 || !s.Seen[ks2.FQN].In {
		t.Errorf("spans %v seen %v", s.Spans, s.Seen)
	}
}

func TestObservePrune(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), all, at(0), every)
	s = s.Observe(stocked("unavailable"), all, at(1), every)
	later := t0.Add(Keep + time.Hour)
	s = s.Observe(stocked("unavailable"), all, later, every)
	if len(s.Spans) != 0 || len(s.Polled) != 1 {
		t.Errorf("history not cut: %v %v", s.Spans, s.Polled)
	}
	if _, ok := s.Seen[ks2.FQN]; !ok {
		t.Error("seen dropped before 180 days")
	}
	s = s.Observe(stocked("unavailable"), all, t0.Add(seenKeep+time.Hour), every)
	if _, ok := s.Seen[ks2.FQN]; ok {
		t.Error("seen kept after 180 days")
	}
}

func TestObserveLeavesOldState(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), all, at(0), every)
	before := s.Spans[gra][0]
	_ = s.Observe(stocked("1H-low"), all, at(1), every)
	if s.Spans[gra][0] != before {
		t.Error("Observe changed the old state's span")
	}
}

func TestObservePrices(t *testing.T) {
	v := ks2
	s, changed := State{}.ObservePrices([]Variant{v}, at(0))
	if !changed {
		t.Error("first price: not changed")
	}
	if s, changed = s.ObservePrices([]Variant{v}, at(1)); changed {
		t.Error("same price: changed")
	}
	v.Rent = 24.99
	s, _ = s.ObservePrices([]Variant{v}, at(2))
	ps := s.Prices[v.FQN]
	if len(ps) != 2 || ps[1].Rent != 24.99 || !ps[1].At.Equal(at(2)) {
		t.Fatalf("prices %+v", ps)
	}
	// A year on, the old points go but the last before the cut stays as the baseline.
	v.Rent = 22.99
	s, _ = s.ObservePrices([]Variant{v}, t0.Add(priceKeep+time.Hour))
	if ps := s.Prices[v.FQN]; len(ps) != 2 || ps[0].Rent != 24.99 {
		t.Errorf("cut %+v", ps)
	}
	// No longer listed: gone once its last price is a year old.
	if s, changed = s.ObservePrices(nil, t0.Add(2*priceKeep+2*time.Hour)); !changed || len(s.Prices) != 0 {
		t.Errorf("unlisted kept: %v", s.Prices)
	}
	unpriced := ks2
	unpriced.FQN, unpriced.Priced = "x", false
	if s, _ := (State{}).ObservePrices([]Variant{unpriced}, at(0)); len(s.Prices) != 0 {
		t.Errorf("unpriced recorded: %v", s.Prices)
	}
}

func TestNextKeepsHistory(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), all, at(0), every).AddPush(Push{Title: "x"})
	n := s.Next(nil, nil, true)
	if len(n.Spans) != 1 || len(n.Pushes) != 1 || n.Since.IsZero() {
		t.Errorf("Next dropped history: %+v", n)
	}
	for range 30 {
		s = s.AddPush(Push{Title: "y"})
	}
	if len(s.Pushes) != maxPushes || s.Pushes[0].Title != "y" {
		t.Errorf("pushes %d", len(s.Pushes))
	}
}

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, _ := State{}.Observe(stocked("72H"), all, at(0), every).ObservePrices(stocked("72H"), at(0))
	s = s.AddPush(Push{At: at(0), Title: "t", Body: "b"})
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrices(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	s.lastIn = nil // memory only
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Errorf("got %+v %v", got, err)
	}
	// A state.json from before the history loads.
	if err := os.WriteFile(p, []byte(`{"notified":["a@gra"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if old, err := LoadState(p); err != nil || len(old.Notified) != 1 {
		t.Errorf("old format: %+v %v", old, err)
	}
}
