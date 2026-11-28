package predict

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/trick77/toolkit/llmwire/llmwiretest"

	"github.com/trick77/ovh-restock/internal/watch"
)

func tueThu(seed uint64) History {
	r := rand.New(rand.NewPCG(seed, 1))
	return Of(synth(weekly(r, []time.Weekday{time.Tuesday, time.Thursday}, 6, 0, 30*time.Minute, 0.1), 30*time.Minute),
		fqn, nowT)
}

func TestGate(t *testing.T) {
	if ok, why := tueThu(1).Gate(); !ok {
		t.Errorf("Tue/Thu gated out: %s", why)
	}
	few := Of(synth([]time.Time{nowT.Add(-48 * time.Hour), nowT.Add(-24 * time.Hour)}, time.Hour), fqn, nowT)
	if ok, _ := few.Gate(); ok {
		t.Error("2 arrivals passed the gate")
	}
	// In stock 90 % of the time: "always there".
	var arr []time.Time
	for d := nowT.Add(-watch.Keep); d.Before(nowT); d = d.Add(24 * time.Hour) {
		arr = append(arr, d)
	}
	always := Of(synth(arr, 22*time.Hour), fqn, nowT)
	if ok, why := always.Gate(); ok {
		t.Errorf("always in stock passed: %s", why)
	}
	// Two weeks observed at most: too short.
	short := synth(weekly(rand.New(rand.NewPCG(2, 2)), []time.Weekday{time.Tuesday}, 6, 0, 0, 0), time.Hour)
	short.Polled = []watch.Span{{From: nowT.Add(-10 * 24 * time.Hour), To: nowT}}
	if ok, _ := Of(short, fqn, nowT).Gate(); ok {
		t.Error("10 days observed passed")
	}
}

func TestHistoryMergesDCs(t *testing.T) {
	s := synth([]time.Time{nowT.Add(-50 * time.Hour)}, time.Hour)
	a := nowT.Add(-50*time.Hour + 10*time.Minute).UTC()
	addSpan(&s, fqn, "rbx", watch.Span{From: a, To: a.Add(time.Hour), Gone: a.Add(time.Hour + 2*time.Minute)})
	h := Of(s, fqn, nowT)
	if len(h.Ins) != 1 || len(h.Events[0].DCs) != 2 {
		t.Errorf("one restock in two DCs: %+v", h.Events)
	}
}

func TestClusters(t *testing.T) {
	cs := tueThu(3).Clusters(zurich)
	if len(cs) < 2 || cs[0].Hour < 5 || cs[0].Hour > 7 || (cs[0].Day != "tue" && cs[0].Day != "thu") {
		t.Errorf("top clusters %+v", cs[:min(3, len(cs))])
	}
}

func weeklyAnswer() answer {
	return answer{Pattern: true, Kind: Weekly, Summary: "back Tue and Thu around 06:00", Confidence: "high",
		Evidence: []int{1, 3, 5}, Slots: []aslot{{"tue", "05:30", "07:00"}, {"thu", "05:30", "07:00"}}}
}

// evidenceIns renumbers a's evidence to the first arrivals of h.
func evidenceIns(a answer, h History) answer {
	a.Evidence = nil
	for _, e := range h.Events {
		if e.In && len(a.Evidence) < 4 {
			a.Evidence = append(a.Evidence, e.N)
		}
	}
	return a
}

func TestJudge(t *testing.T) {
	h := tueThu(4)
	if p := judge(evidenceIns(weeklyAnswer(), h), h, zurich); !p.Pattern || len(p.Slots) != 2 {
		t.Fatalf("true pattern dropped: %+v", p)
	}
	r := rand.New(rand.NewPCG(5, 5))
	noisy := Of(synth(noise(r, 20), 30*time.Minute), fqn, nowT)
	for name, tc := range map[string]struct {
		a answer
		h History
	}{
		"invented on noise":   {evidenceIns(weeklyAnswer(), noisy), noisy},
		"evidence no arrival": {func() answer { a := weeklyAnswer(); a.Evidence = []int{2, 4, 6}; return a }(), h},
		"evidence missing":    {func() answer { a := evidenceIns(weeklyAnswer(), h); a.Evidence = a.Evidence[:2]; return a }(), h},
		"low confidence":      {func() answer { a := evidenceIns(weeklyAnswer(), h); a.Confidence = "low"; return a }(), h},
		"slot too wide": {func() answer {
			a := evidenceIns(weeklyAnswer(), h)
			a.Slots = []aslot{{"tue", "00:00", "23:00"}}
			return a
		}(), h},
		"wrong weekday": {func() answer {
			a := evidenceIns(weeklyAnswer(), h)
			a.Slots = []aslot{{"mon", "05:30", "07:00"}}
			return a
		}(), h},
		"interval off": {func() answer {
			a := evidenceIns(weeklyAnswer(), h)
			a.Kind, a.Slots, a.EveryHours = Interval, nil, 24*9
			return a
		}(), h},
		"none": {answer{Kind: None}, h},
	} {
		if p := judge(tc.a, tc.h, zurich); p.Pattern {
			t.Errorf("%s: passed: %+v", name, p)
		}
	}
	// An interval with regular gaps passes, an irregular one doesn't.
	r = rand.New(rand.NewPCG(6, 6))
	reg := Of(synth(every(r, 9*24*time.Hour, 12*time.Hour), time.Hour), fqn, nowT)
	ia := evidenceIns(answer{Pattern: true, Kind: Interval, Confidence: "medium", EveryHours: reg.Gaps().Median,
		SpreadHours: 12, Summary: "about every 9 days"}, reg)
	if p := judge(ia, reg, zurich); !p.Pattern || p.Every <= 0 {
		t.Errorf("regular interval dropped: %+v (gaps %+v)", p, reg.Gaps())
	}
	ia2 := evidenceIns(answer{Pattern: true, Kind: Interval, Confidence: "medium", EveryHours: noisy.Gaps().Median,
		Summary: "x"}, noisy)
	if p := judge(ia2, noisy, zurich); p.Pattern {
		t.Errorf("irregular gaps passed: %+v", noisy.Gaps())
	}
}

func TestRoll(t *testing.T) {
	h := tueThu(7)
	p := judge(evidenceIns(weeklyAnswer(), h), h, zurich)
	p.roll(h, nowT, zurich) // Wednesday noon: next is Thursday 05:30
	if want := time.Date(2026, 11, 26, 5, 30, 0, 0, zurich); !p.Next.From.Equal(want) {
		t.Fatalf("next %s, want %s", p.Next.From, want)
	}
	// Polls keep running; Thursday's window passes with an arrival at 06:10: a hit; next is Tuesday.
	h.Polled = []watch.Span{{From: nowT.Add(-watch.Keep), To: time.Date(2026, 12, 2, 0, 0, 0, 0, zurich)}}
	h.Ins = append(h.Ins, time.Date(2026, 11, 26, 6, 10, 0, 0, zurich))
	p.roll(h, time.Date(2026, 11, 26, 9, 0, 0, 0, zurich), zurich)
	if p.Hits != 1 || p.Tries != 1 || p.Next.From.Weekday() != time.Tuesday {
		t.Errorf("after Thursday: %+v", p)
	}
	// Tuesday passes without one: a miss.
	p.roll(h, time.Date(2026, 12, 1, 9, 0, 0, 0, zurich), zurich)
	if p.Hits != 1 || p.Tries != 2 {
		t.Errorf("after Tuesday: hits %d of %d", p.Hits, p.Tries)
	}
}

func service(t *testing.T, reply string) (*Service, *llmwiretest.Server) {
	t.Helper()
	srv := llmwiretest.NewServer(t)
	srv.SetReply(reply)
	s, err := New(t.Context(), Options{Client: srv.Client(), Model: llmwiretest.ChatModel, DailyCalls: 3,
		Store: &memStore{}, Loc: zurich})
	if err != nil {
		t.Fatal(err)
	}
	return s, srv
}

func TestAsk(t *testing.T) {
	h := tueThu(8)
	b, _ := json.Marshal(evidenceIns(weeklyAnswer(), h))
	s, srv := service(t, string(b))
	p, err := s.Ask(context.Background(), h, nowT)
	if err != nil || !p.Pattern || p.Next.From.IsZero() || p.Model != llmwiretest.ChatModel {
		t.Fatalf("ask: %+v %v", p, err)
	}
	req := srv.Last()
	if rf, _ := req.Body["response_format"].(map[string]any); rf["type"] != "json_schema" {
		t.Errorf("response_format %v", req.Body["response_format"])
	}
	if req.Reasoning() != llmwiretest.BalancedSent {
		t.Errorf("reasoning %q", req.Reasoning())
	}
	// The model sees the clusters and the numbered events, not the state.
	msgs, _ := json.Marshal(req.Body["messages"])
	if !strings.Contains(string(msgs), `\"clusters\"`) || !strings.Contains(string(msgs), `\"n\":1`) {
		t.Errorf("input lacks clusters or events: %.300s", msgs)
	}
	// Budget: 3 a day.
	for range 3 {
		_, err = s.Ask(context.Background(), h, nowT)
	}
	if err == nil {
		t.Error("budget not enforced")
	}
}

func TestUpdateAsksOnChangeOnly(t *testing.T) {
	h := tueThu(9)
	b, _ := json.Marshal(evidenceIns(weeklyAnswer(), h))
	s, _ := service(t, string(b))
	st := synth(weekly(rand.New(rand.NewPCG(9, 1)), []time.Weekday{time.Tuesday, time.Thursday}, 6, 0,
		30*time.Minute, 0.1), 30*time.Minute)
	s.Update(st, []string{fqn}, nowT)
	if len(s.jobs) != 1 {
		t.Fatalf("first look: %d jobs", len(s.jobs))
	}
	job := <-s.jobs
	p, err := s.Ask(context.Background(), job, nowT)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.preds[fqn], s.queued = p, map[string]bool{}
	s.mu.Unlock()
	// Nothing new: no call, even a day later.
	s.Update(st, []string{fqn}, nowT.Add(24*time.Hour))
	if len(s.jobs) != 0 {
		t.Error("asked without a change")
	}
	// A new departure: asked again, once 6 h have passed.
	a := nowT.Add(time.Hour).UTC()
	addSpan(&st, fqn, "gra", watch.Span{From: a, To: a.Add(time.Hour), Gone: a.Add(time.Hour + 2*time.Minute)})
	st.Polled[len(st.Polled)-1].To = nowT.Add(3 * time.Hour)
	s.Update(st, []string{fqn}, nowT.Add(3*time.Hour))
	if len(s.jobs) != 0 {
		t.Error("asked again within 6 h")
	}
	s.Update(st, []string{fqn}, nowT.Add(7*time.Hour))
	if len(s.jobs) != 1 {
		t.Error("not asked after a change")
	}
	// Out of scope: dropped.
	s.Update(st, nil, nowT.Add(8*time.Hour))
	if _, ok := s.Get(fqn); ok {
		t.Error("kept out of scope")
	}
}

func TestNoModelAsksNothing(t *testing.T) {
	s, err := New(t.Context(), Options{Store: &memStore{}})
	if err != nil || s.Configured() {
		t.Fatal(err)
	}
	s.Update(synth(noise(rand.New(rand.NewPCG(1, 1)), 10), time.Hour), []string{fqn}, nowT)
	if len(s.jobs) != 0 {
		t.Error("queued without a model")
	}
}

func TestPersist(t *testing.T) {
	h := tueThu(10)
	b, _ := json.Marshal(evidenceIns(weeklyAnswer(), h))
	s, _ := service(t, string(b))
	p, _ := s.Ask(context.Background(), h, nowT)
	s.preds[fqn] = p
	s.save()
	s2, err := New(t.Context(), Options{Store: s.o.Store, Loc: zurich})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get(fqn); !ok || !got.Pattern || len(got.Slots) != 2 || !got.Next.From.Equal(p.Next.From) {
		t.Errorf("reloaded %+v", got)
	}
	// Gone from the service: gone from the store.
	s.mu.Lock()
	delete(s.preds, fqn)
	s.mu.Unlock()
	s.save()
	if got, _ := s.o.Store.Predictions(t.Context()); len(got) != 0 {
		t.Errorf("deleted prediction kept: %v", got)
	}
}
