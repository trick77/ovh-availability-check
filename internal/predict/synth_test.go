package predict

import (
	"math/rand/v2"
	"slices"
	"time"

	"github.com/trick77/ovh-availability-check/internal/watch"
)

// zurich is the zone the synthetic histories and the checks run in.
var zurich = func() *time.Location {
	l, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		panic(err)
	}
	return l
}()

// nowT is a Wednesday noon.
var nowT = time.Date(2026, 11, 25, 12, 0, 0, 0, zurich)

const fqn = "x.ram-32g.softraid-2x450nvme"

// synth is a state with 90 days polled and one in-stock span per arrival, each stay long, then gone. gaps are
// stretches without polling.
func synth(arrivals []time.Time, stay time.Duration, gaps ...watch.Span) watch.State {
	start := nowT.Add(-watch.Keep)
	s := watch.State{Since: start, PolledAt: nowT, Spans: map[string][]watch.Span{}}
	from := start
	for _, g := range gaps {
		s.Polled = append(s.Polled, watch.Span{From: from, To: g.From})
		from = g.To
	}
	s.Polled = append(s.Polled, watch.Span{From: from, To: nowT})
	slices.SortFunc(arrivals, func(a, b time.Time) int { return a.Compare(b) })
	k := watch.Key(fqn, "gra")
	for _, a := range arrivals {
		if a.After(nowT) || a.Before(start) {
			continue
		}
		s.Spans[k] = append(s.Spans[k], watch.Span{From: a.UTC(), To: a.Add(stay).UTC(), Gone: a.Add(stay + 2*time.Minute).UTC()})
	}
	return s
}

// weekly is an arrival on each of wds at hh:mm, ±jitter, over the kept window; miss is the chance a week skips.
func weekly(r *rand.Rand, wds []time.Weekday, hh, mm int, jitter time.Duration, miss float64) []time.Time {
	var out []time.Time
	start := nowT.Add(-watch.Keep)
	for d := start; d.Before(nowT); d = d.AddDate(0, 0, 1) {
		if !slices.Contains(wds, d.Weekday()) || r.Float64() < miss {
			continue
		}
		t := time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, zurich)
		out = append(out, t.Add(jit(r, jitter)))
	}
	return out
}

// daily is an arrival every day at hh:mm ± jitter, from days ago.
func daily(r *rand.Rand, days, hh, mm int, jitter time.Duration, miss float64) []time.Time {
	var out []time.Time
	for d := nowT.AddDate(0, 0, -days); d.Before(nowT); d = d.AddDate(0, 0, 1) {
		if r.Float64() < miss {
			continue
		}
		out = append(out, time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, zurich).Add(jit(r, jitter)))
	}
	return out
}

// every is an arrival every gap ± jitter.
func every(r *rand.Rand, gap, jitter time.Duration) []time.Time {
	var out []time.Time
	for t := nowT.Add(-watch.Keep).Add(gap / 3); t.Before(nowT); t = t.Add(gap) {
		out = append(out, t.Add(jit(r, jitter)))
	}
	return out
}

// noise is n arrivals at random times.
func noise(r *rand.Rand, n int) []time.Time {
	var out []time.Time
	for range n {
		out = append(out, nowT.Add(-time.Duration(r.Int64N(int64(watch.Keep)))))
	}
	return out
}

func jit(r *rand.Rand, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(r.Int64N(int64(2*d))) - d
}
