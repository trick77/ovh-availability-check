package predict

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/trick77/toolkit/llmwire/llmwiretest"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/watch"
)

func TestDuplicateEvidence(t *testing.T) {
	h := tueThu(11)
	a := evidenceIns(weeklyAnswer(), h)
	a.Evidence = []int{a.Evidence[0], a.Evidence[0], a.Evidence[0]}
	if p := judge(a, h, zurich); p.Pattern {
		t.Error("one arrival cited three times passed")
	}
}

// Flapping three times inside one Tuesday slot in two of twelve weeks is two weeks, not six arrivals.
func TestSlotCountsWeeks(t *testing.T) {
	var arr []time.Time
	for _, w := range []int{-3, -9} {
		d := time.Date(2026, 11, 24, 6, 0, 0, 0, zurich).AddDate(0, 0, 7*w)
		arr = append(arr, d, d.Add(15*time.Minute), d.Add(30*time.Minute))
	}
	r := rand.New(rand.NewPCG(12, 12))
	h := Of(synth(append(arr, noise(r, 4)...), 5*time.Minute), fqn, nowT)
	a := evidenceIns(answer{Pattern: true, Kind: Weekly, Confidence: "high", Summary: "x",
		Slots: []aslot{{"tue", "05:30", "07:00"}}}, h)
	if p := judge(a, h, zurich); p.Pattern {
		t.Errorf("2 of 12 weeks passed: %+v", p)
	}
}

func TestCarryKeepsHits(t *testing.T) {
	h := History{Polled: []watch.Span{{From: nowT.Add(-watch.Keep), To: nowT}}}
	old := Prediction{Pattern: true, Kind: Weekly, Slots: []Slot{{2, 330, 420}}, Hits: 3, Tries: 4}
	same := Prediction{Pattern: true, Kind: Weekly, Slots: []Slot{{2, 330, 420}}}
	if p := carry(old, same, h, nowT, zurich); p.Hits != 3 || p.Tries != 4 {
		t.Errorf("same rule lost its record: %+v", p)
	}
	other := Prediction{Pattern: true, Kind: Weekly, Slots: []Slot{{4, 330, 420}}}
	if p := carry(old, other, h, nowT, zurich); p.Tries != 0 {
		t.Errorf("new rule kept the old record: %+v", p)
	}
	// Interval: stock came and went inside the open window, the re-ask moves Next on. That window is a hit.
	open := Window{From: nowT.Add(-3 * time.Hour), To: nowT.Add(3 * time.Hour)}
	iv := Prediction{Pattern: true, Kind: Interval, Every: 72 * time.Hour, Spread: 3 * time.Hour, Next: open, Hits: 1, Tries: 2}
	h.Ins = []time.Time{nowT.Add(-time.Hour)}
	if p := carry(iv, Prediction{Pattern: true, Kind: Interval, Every: 72 * time.Hour}, h, nowT, zurich); p.Hits != 2 || p.Tries != 3 {
		t.Errorf("open window hit lost: %d of %d", p.Hits, p.Tries)
	}
}

func TestRollSkipsUnwatched(t *testing.T) {
	h := tueThu(15)
	p := judge(evidenceIns(weeklyAnswer(), h), h, zurich)
	p.roll(h, nowT, zurich)
	// Down until Tuesday: Thursday's window passed unwatched and isn't counted.
	p.roll(h, time.Date(2026, 11, 30, 12, 0, 0, 0, zurich), zurich)
	if p.Tries != 0 {
		t.Errorf("unwatched window counted: %d tries", p.Tries)
	}
}

func TestFailedCallWaits(t *testing.T) {
	s, _ := service(t, "not json")
	st := synth(weekly(rand.New(rand.NewPCG(13, 1)), []time.Weekday{time.Tuesday}, 6, 0, 10*time.Minute, 0), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Update(st, []string{fqn}, nowT)
	waitIdle(t, s)
	s.Update(st, []string{fqn}, nowT.Add(2*time.Minute))
	if len(s.jobs) != 0 || s.calls != 1 {
		t.Errorf("failed call asked again at once: %d queued, %d calls", len(s.jobs), s.calls)
	}
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	for range 200 {
		s.mu.Lock()
		busy := len(s.queued) > 0
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker stuck")
}

func TestZeroBudgetIsOff(t *testing.T) {
	srv := llmwiretest.NewServer(t)
	s, err := New(Options{Client: srv.Client(), Model: llmwiretest.ChatModel, DailyCalls: 0,
		Path: filepath.Join(t.TempDir(), "p.json"), Loc: zurich})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ask(context.Background(), tueThu(14), nowT); err == nil || len(srv.Requests()) != 0 {
		t.Error("0 daily calls still called")
	}
}

func TestSlotPastMidnight(t *testing.T) {
	var arr []time.Time
	for w := range 12 {
		d := time.Date(2026, 11, 20, 23, 50, 0, 0, zurich).AddDate(0, 0, -7*w) // Fridays 23:50 …
		arr = append(arr, d.Add(time.Duration(w%3)*10*time.Minute))            // … to 00:10
	}
	h := Of(synth(arr, 20*time.Minute), fqn, nowT)
	a := evidenceIns(answer{Pattern: true, Kind: Weekly, Confidence: "high", Summary: "Fri around midnight",
		Slots: []aslot{{"fri", "23:30", "00:30"}}}, h)
	p := judge(a, h, zurich)
	if !p.Pattern {
		t.Fatalf("past-midnight slot dropped: %s", p.Dropped)
	}
	p.roll(h, nowT, zurich)
	if want := time.Date(2026, 11, 27, 23, 30, 0, 0, zurich); !p.Next.From.Equal(want) ||
		!p.Next.To.Equal(want.Add(time.Hour)) {
		t.Errorf("next %s – %s", p.Next.From, p.Next.To)
	}
}

func TestWindowOnDSTDay(t *testing.T) {
	p := Prediction{Pattern: true, Kind: Weekly, Slots: []Slot{{0, 360, 420}}} // Sunday 06:00–07:00
	// 2026-10-25 is the last Sunday of October: 25 hours long in Zurich.
	w := p.after(time.Date(2026, 10, 24, 12, 0, 0, 0, zurich), zurich)
	if lf := w.From.In(zurich); lf.Hour() != 6 || lf.Day() != 25 {
		t.Errorf("DST Sunday window from %s", lf)
	}
}

func TestDigestOneRestockManyDCs(t *testing.T) {
	a := nowT.Add(-50 * time.Hour).UTC()
	st := synth(nil, 0)
	for _, dc := range []string{"gra", "rbx", "sbg"} {
		st.Spans[watch.Key(fqn, dc)] = []watch.Span{{From: a, To: a.Add(time.Hour), Gone: a.Add(time.Hour + 2*time.Minute)}}
	}
	v := watch.Variant{FQN: fqn, Name: "KS-A", RAM: 32, Disks: []ovh.Disk{{Count: 1, SizeGB: 2000, Kind: "hdd"}},
		Rent: 10, Priced: true}
	d := BuildDigest(st, []watch.Variant{v}, watch.Config{Want: []watch.Want{{PriceMax: 30}}}, nil, nowT, zurich)
	if len(d.Servers) != 1 || d.Servers[0].Windows != 1 || len(d.Servers[0].DCs) != 3 {
		t.Errorf("digest %+v", d.Servers)
	}
}
