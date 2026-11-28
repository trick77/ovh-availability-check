package watch

import (
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

func gra(s State) []Span { return s.Spans[ks2.FQN]["gra"] }

func TestObserveStreaks(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), at(0), every) // first run: seeded, no change
	if got := s.Changes(nil); len(got) != 0 {
		t.Errorf("first run logged %v", got)
	}
	s = s.Observe(stocked("1H-low"), at(1), every)
	s = s.Observe(stocked("unavailable"), at(2), every) // out
	s = s.Observe(stocked("72H"), at(3), every)         // in, slow
	s = s.Observe(stocked("1H-high"), at(4), every)     // 72H → 1H: same streak

	if e := s.Seen[ks2.FQN]; !e.In || !e.Since.Equal(at(3)) || !e.Last.Equal(at(4)) {
		t.Errorf("seen %+v", e)
	}
	want := []Span{{From: at(0), To: at(1), Gone: at(2), Quiet: true}, {From: at(3), To: at(3), Slow: true},
		{From: at(3), To: at(4)}}
	if !reflect.DeepEqual(gra(s), want) {
		t.Errorf("spans %+v", gra(s))
	}
	// The out is timed at the poll that found it gone.
	got := s.Changes(nil)
	if len(got) != 2 || !got[0].In || !got[0].Slow || !got[0].At.Equal(at(3)) ||
		got[1].In || !got[1].At.Equal(at(2)) || got[1].For != 2*every {
		t.Errorf("changes %+v", got)
	}
	if c := s.ChangesOf(ks2.FQN); !reflect.DeepEqual(c, got) {
		t.Errorf("ChangesOf %+v", c)
	}
	if c := s.Changes(func(string) bool { return false }); len(c) != 0 {
		t.Errorf("filtered %+v", c)
	}
	if !reflect.DeepEqual(s.Polled, []Span{{From: at(0), To: at(4)}}) {
		t.Errorf("polled %+v", s.Polled)
	}
	if !s.AllSince.Equal(at(0)) || !s.TrackedFrom(ks2.FQN).Equal(at(0)) {
		t.Errorf("all since %v tracked %v", s.AllSince, s.TrackedFrom(ks2.FQN))
	}
}

func TestObserveGap(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), at(0), every)
	s = s.Observe(stocked("1H-low"), at(1), every)
	// Down for an hour: the streak and the span end, and the return logs no "in" nobody saw.
	s = s.Observe(stocked("1H-low"), at(31), every)
	if e := s.Seen[ks2.FQN]; !e.Since.Equal(at(31)) {
		t.Errorf("streak survived the gap: %+v", e)
	}
	if len(gra(s)) != 2 || len(s.Polled) != 2 {
		t.Errorf("spans %+v polled %+v", gra(s), s.Polled)
	}
	if got := s.Changes(nil); len(got) != 0 {
		t.Errorf("gap logged %v", got)
	}
	// A slow poll is no gap.
	s = s.Observe(stocked("1H-low"), at(31).Add(gapAfter*every+4*time.Minute), every)
	if len(gra(s)) != 2 {
		t.Errorf("slow poll broke the span: %+v", gra(s))
	}
}

func TestObserveEveryPair(t *testing.T) {
	// Wanted or not, every pair gets its history: a wish added later finds it.
	s := State{}.Observe(stocked("1H-low"), at(0), every)
	s = s.Observe(stocked("unavailable"), at(1), every)
	s = s.Observe(stocked("1H-low"), at(2), every)
	if len(gra(s)) != 2 || len(s.Changes(nil)) != 2 {
		t.Errorf("spans %+v changes %+v", gra(s), s.Changes(nil))
	}
	// A DC dropped from the config: the pair is no longer listed, its span stops without an "out".
	d := State{}.Observe(stocked("1H-low"), at(0), every)
	d = d.Observe(nil, at(1), every)
	if sp := gra(d); len(sp) != 1 || !sp[0].Gone.IsZero() {
		t.Errorf("unlisted logged gone: %+v", sp)
	}
}

func TestObserveRestartIsQuiet(t *testing.T) {
	// The first poll after a start (the state loaded from the store, warm unset) saw nothing come in: a span it
	// opens has no "in". The import that starts tracking every pair hits ~5700 such pairs at once.
	s := State{}.Observe(nil, at(0), every)
	loaded := s
	loaded.warm = false
	loaded = loaded.Observe(stocked("1H-low"), at(1), every)
	if c := loaded.Changes(nil); len(c) != 0 || !gra(loaded)[0].Quiet {
		t.Errorf("first poll after a start logged %+v", c)
	}
	// Warm: an arrival is an arrival.
	s = s.Observe(stocked("1H-low"), at(1), every)
	if c := s.Changes(nil); len(c) != 1 || !c[0].In {
		t.Errorf("arrival %+v", c)
	}
}

func TestTrackedFrom(t *testing.T) {
	// Imported from 0.3: in scope then, its first kept span recent. Tracked from Since, or its prediction would
	// fail the 14-day gate at the upgrade.
	s := State{Since: at(0), AllSince: at(1000), Spans: map[string]map[string][]Span{
		"old": {"gra": {{From: at(900), To: at(901)}}}}}
	if got := s.TrackedFrom("old"); !got.Equal(at(0)) {
		t.Errorf("in scope under 0.3: %v", got)
	}
	if got := s.TrackedFrom("new"); !got.Equal(at(1000)) {
		t.Errorf("tracked since the upgrade: %v", got)
	}
}

func TestObservePrune(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), at(0), every)
	s = s.Observe(stocked("unavailable"), at(1), every)
	s = s.AddEvent(Event{At: at(1), Kind: KindRestock, Title: "x"})
	later := t0.Add(Keep + time.Hour)
	s = s.Observe(stocked("unavailable"), later, every)
	if len(s.Spans) != 0 || len(s.Polled) != 1 || len(s.Events) != 0 {
		t.Errorf("history not cut: %v %v %v", s.Spans, s.Polled, s.Events)
	}
	if _, ok := s.Seen[ks2.FQN]; !ok {
		t.Error("seen dropped before 180 days")
	}
	s = s.Observe(stocked("unavailable"), t0.Add(seenKeep+time.Hour), every)
	if _, ok := s.Seen[ks2.FQN]; ok {
		t.Error("seen kept after 180 days")
	}
}

func TestObserveLeavesOldState(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), at(0), every)
	before := gra(s)[0]
	_ = s.Observe(stocked("1H-low"), at(1), every)
	_ = s.Observe(stocked("unavailable"), at(1), every)
	if gra(s)[0] != before {
		t.Error("Observe changed the old state's span")
	}
}

func TestObservePrices(t *testing.T) {
	v := ks2
	s := State{}.ObservePrices([]Variant{v}, at(0))
	if len(s.Prices[v.FQN]) != 1 {
		t.Fatal("first price not kept")
	}
	before := s.Prices
	if s = s.ObservePrices([]Variant{v}, at(1)); !reflect.DeepEqual(s.Prices, before) {
		t.Error("same price: changed")
	}
	v.Rent = 24.99
	s = s.ObservePrices([]Variant{v}, at(2))
	ps := s.Prices[v.FQN]
	if len(ps) != 2 || ps[1].Rent != 24.99 || !ps[1].At.Equal(at(2)) || len(before[v.FQN]) != 1 {
		t.Fatalf("prices %+v", ps)
	}
	// A year on, the old points go but the last before the cut stays as the baseline.
	v.Rent = 22.99
	s = s.ObservePrices([]Variant{v}, t0.Add(priceKeep+time.Hour))
	if ps := s.Prices[v.FQN]; len(ps) != 2 || ps[0].Rent != 24.99 {
		t.Errorf("cut %+v", ps)
	}
	// No longer listed: gone once its last price is a year old.
	if s = s.ObservePrices(nil, t0.Add(2*priceKeep+2*time.Hour)); len(s.Prices) != 0 {
		t.Errorf("unlisted kept: %v", s.Prices)
	}
	unpriced := ks2
	unpriced.FQN, unpriced.Priced = "x", false
	if s := (State{}).ObservePrices([]Variant{unpriced}, at(0)); len(s.Prices) != 0 {
		t.Errorf("unpriced recorded: %v", s.Prices)
	}
}

func TestEvents(t *testing.T) {
	s := State{}.Observe(stocked("1H-low"), at(0), every)
	e := Event{At: at(0).Add(300 * time.Millisecond), Kind: KindRestock, Title: "a", Delivery: "failed", Retry: true,
		Items: Items(stocked("72H"), []Pair{{ks2.FQN, "gra"}})}
	s = s.AddEvent(e).AddEvent(Event{At: at(0), Kind: KindDigest, Title: "d"})
	n := s.Next(nil, nil)
	if len(gra(n)) != 1 || len(n.Events) != 2 || n.Since.IsZero() {
		t.Errorf("Next dropped history: %+v", n)
	}
	// One per At and Kind: a second restock in the same second replaces the first.
	s = s.AddEvent(Event{At: at(0), Kind: KindRestock, Title: "b", Items: e.Items})
	if len(s.Events) != 2 || s.Events[0].Title != "b" && s.Events[1].Title != "b" {
		t.Errorf("events %+v", s.Events)
	}
	s = s.SetDelivery(s.Events[0], Sent, false)
	if s.Events[0].Delivery != Sent || s.Events[0].Retry {
		t.Errorf("delivery %+v", s.Events[0])
	}
	if p := e.Pairs(); len(p) != 1 || p[0].DC != "gra" || !e.Items[0].DCs[0].Slow {
		t.Errorf("pairs %+v items %+v", p, e.Items)
	}
}
