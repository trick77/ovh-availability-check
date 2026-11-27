// Package predict looks for a rhythm in when a wanted server comes into stock and says when it may come next. An
// LLM proposes the pattern; the code here decides whether there is enough history to ask, counts what the LLM
// must agree with, and drops any answer the history doesn't bear out. No pattern is the expected answer: the user
// asked never to see one invented.
package predict

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/trick77/ovh-restock/internal/watch"
)

// The gate: below it no call is made (user: rarely or never stocked → nothing to find).
const (
	minObserved = 14 * 24 * time.Hour // polled time in the kept history
	minIns      = 4                   // stock arrivals
	maxInShare  = 0.8                 // in stock this much of the time is "always there", not a rhythm
)

// sameArrival: an arrival in a second DC within this is the same restock.
const sameArrival = time.Hour

// Event is one stock arrival or departure of a variant, any of its DCs.
type Event struct {
	N   int       `json:"n"` // number the LLM cites as evidence
	At  time.Time `json:"-"`
	In  bool      `json:"-"`
	DCs []string  `json:"dcs"`
	// For is how long it stayed, on an arrival whose departure was seen.
	For time.Duration `json:"-"`
}

// History is what the prediction sees of one variant.
type History struct {
	FQN      string
	Events   []Event       // oldest first, numbered from 1
	Ins      []time.Time   // arrivals, oldest first
	Observed time.Duration // polled time in the kept window
	Polled   []watch.Span  // the stretches polls covered
	InShare  float64       // share of Observed it was in stock
	LastGone time.Time     // the newest departure: a new one is what triggers a call
}

// Of builds fqn's history from the state. Arrivals in several DCs within sameArrival count once.
func Of(s watch.State, fqn string, now time.Time) History {
	h := History{FQN: fqn, Polled: s.Polled}
	from := now.Add(-watch.Keep)
	for _, p := range s.Polled {
		a, b := maxT(p.From, from), p.To
		if b.After(a) {
			h.Observed += b.Sub(a)
		}
	}
	var spans []watch.Span
	for k, sp := range s.Spans {
		if f, _ := splitKey(k); f == fqn {
			spans = append(spans, sp...)
		}
	}
	h.InShare = share(spans, from, h.Observed)

	var raw []Event
	for _, c := range s.Changes() {
		if c.FQN != fqn || c.At.Before(from) {
			continue
		}
		raw = append(raw, Event{At: c.At, In: c.In, DCs: []string{c.DC}, For: c.For})
		if !c.In && c.At.After(h.LastGone) {
			h.LastGone = c.At
		}
	}
	slices.SortStableFunc(raw, func(a, b Event) int { return a.At.Compare(b.At) })
	for _, e := range raw {
		if n := len(h.Events); n > 0 {
			last := &h.Events[n-1]
			if last.In == e.In && e.At.Sub(last.At) < sameArrival {
				last.DCs = append(last.DCs, e.DCs...)
				last.For = max(last.For, e.For)
				continue
			}
		}
		h.Events = append(h.Events, e)
	}
	for i := range h.Events {
		h.Events[i].N = i + 1
		if h.Events[i].In {
			h.Ins = append(h.Ins, h.Events[i].At)
		}
	}
	// An arrival's stay: the next departure after it.
	for i, e := range h.Events {
		if !e.In {
			continue
		}
		for _, o := range h.Events[i+1:] {
			if !o.In {
				h.Events[i].For = o.At.Sub(e.At)
				break
			}
		}
	}
	return h
}

// Gate tells whether h is worth asking about, and if not why.
func (h History) Gate() (bool, string) {
	switch {
	case h.Observed < minObserved:
		return false, fmt.Sprintf("observed %s, needs %s", h.Observed.Round(time.Hour), minObserved)
	case len(h.Ins) < minIns:
		return false, fmt.Sprintf("%d arrivals, needs %d", len(h.Ins), minIns)
	case h.InShare >= maxInShare:
		return false, "in stock nearly all the time"
	}
	return true, ""
}

// share is the part of observed the spans cover, overlaps counted once.
func share(spans []watch.Span, from time.Time, observed time.Duration) float64 {
	if observed <= 0 {
		return 0
	}
	slices.SortFunc(spans, func(a, b watch.Span) int { return a.From.Compare(b.From) })
	var total time.Duration
	var end time.Time
	for _, sp := range spans {
		a := maxT(sp.From, maxT(from, end))
		if sp.To.After(a) {
			total += sp.To.Sub(a)
			end = sp.To
		}
	}
	return float64(total) / float64(observed)
}

// Cluster is a weekday and time of day arrivals bunch at, counted by the code for the LLM to agree with.
type Cluster struct {
	Day   string `json:"day"` // mon … sun, or "any" for a daily one
	Hour  int    `json:"hour"`
	Hits  int    `json:"hits"`  // arrivals within ±1 h of Hour on Day
	Spans int    `json:"spans"` // distinct weeks (weekly) or days (daily) observed
}

// Gaps are the times between arrivals.
type Gaps struct {
	N      int     `json:"n"`
	Median float64 `json:"median_hours"`
	P25    float64 `json:"p25_hours"`
	P75    float64 `json:"p75_hours"`
}

var days = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Clusters counts arrivals per weekday and hour (±1 h), the busiest first; and per hour on any day. Only counts of
// 2 or more are kept.
func (h History) Clusters(loc *time.Location) []Cluster {
	weeks := max(1, int(h.Observed/(7*24*time.Hour)))
	dayCount := max(1, int(h.Observed/(24*time.Hour)))
	var out []Cluster
	for wd := -1; wd < 7; wd++ {
		for hr := range 24 {
			hits := 0
			for _, t := range h.Ins {
				lt := t.In(loc)
				if (wd < 0 || int(lt.Weekday()) == wd) && near(lt, hr) {
					hits++
				}
			}
			if hits < 2 {
				continue
			}
			c := Cluster{Day: "any", Hour: hr, Hits: hits, Spans: dayCount}
			if wd >= 0 {
				c.Day, c.Spans = days[wd], weeks
			}
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b Cluster) int {
		return cmp.Compare(float64(b.Hits)/float64(b.Spans), float64(a.Hits)/float64(a.Spans))
	})
	return out[:min(len(out), 12)]
}

// near: lt's time of day lies within an hour of hr:00.
func near(lt time.Time, hr int) bool {
	m := lt.Hour()*60 + lt.Minute()
	d := m - hr*60
	if d < 0 {
		d = -d
	}
	return min(d, 24*60-d) <= 60
}

// Gaps measures the times between arrivals.
func (h History) Gaps() Gaps {
	var g []float64
	for i := 1; i < len(h.Ins); i++ {
		g = append(g, h.Ins[i].Sub(h.Ins[i-1]).Hours())
	}
	if len(g) == 0 {
		return Gaps{}
	}
	sort.Float64s(g)
	q := func(p float64) float64 { return round1(g[int(p*float64(len(g)-1)+0.5)]) }
	return Gaps{N: len(g), Median: q(0.5), P25: q(0.25), P75: q(0.75)}
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func splitKey(k string) (fqn, dc string) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '@' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
