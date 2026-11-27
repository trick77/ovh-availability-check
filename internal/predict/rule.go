package predict

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ovh-availability-check/internal/watch"
)

// Kinds of pattern.
const (
	Weekly   = "weekly"
	Daily    = "daily"
	Interval = "interval"
	None     = "none"
)

// The checks an answer must pass. A weekly slot must catch at least half the weeks observed (and 3 arrivals in 2
// weeks at least), a daily one half the days; an interval must sit within 20 % of the measured median gap with the
// gaps regular. Tuned against the evals, not by feel: a looser bar let noise through as "pattern".
const (
	minSlotHits  = 3
	minCatch     = 0.5
	slotSlack    = 30 * time.Minute
	maxSlotWidth = 12 * time.Hour
	maxSlots     = 4
	intervalTol  = 0.2
	maxSpread    = 0.5 // (p75-p25)/median of the gaps
	horizon      = 14 * 24 * time.Hour
)

// Slot is a time of day, on a weekday or (Weekday -1) on any day, in minutes after midnight local time.
type Slot struct {
	Weekday int `json:"weekday"`
	From    int `json:"from"`
	To      int `json:"to"`
}

// Window is a stretch of time stock is expected in.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Prediction is what the page shows for a variant.
type Prediction struct {
	// Asked is when the LLM was asked, Basis the newest departure it saw: a newer one asks again.
	Asked time.Time `json:"asked"`
	Basis time.Time `json:"basis"`
	Model string    `json:"model"`
	// Pattern is false for "none", also when the LLM's answer failed the checks (Dropped says why). Claimed is
	// whether the LLM said there was one, before the checks.
	Pattern bool   `json:"pattern"`
	Claimed bool   `json:"claimed,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Summary string `json:"summary,omitempty"`
	Dropped string `json:"dropped,omitempty"`
	Slots   []Slot `json:"slots,omitempty"`
	// Interval: Every ± Spread after Anchor, the newest arrival.
	Every  time.Duration `json:"every,omitzero"`
	Spread time.Duration `json:"spread,omitzero"`
	Anchor time.Time     `json:"anchor,omitzero"`
	// Next is the coming window, rolled forward by code; Hits of Tries past windows saw an arrival.
	Next  Window `json:"next,omitzero"`
	Hits  int    `json:"hits"`
	Tries int    `json:"tries"`
}

// answer is the LLM's reply, as the schema asks.
type answer struct {
	Pattern     bool    `json:"pattern"`
	Kind        string  `json:"kind"`
	Summary     string  `json:"summary"`
	Evidence    []int   `json:"evidence"`
	Slots       []aslot `json:"slots"`
	EveryHours  float64 `json:"every_hours"`
	SpreadHours float64 `json:"spread_hours"`
	Confidence  string  `json:"confidence"`
}

type aslot struct {
	Day  string `json:"day"`
	From string `json:"from"`
	To   string `json:"to"`
}

// schema is the strict JSON schema of answer.
var schema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"pattern", "kind", "summary", "evidence", "slots", "every_hours", "spread_hours", "confidence"},
	"properties": map[string]any{
		"pattern":  map[string]any{"type": "boolean"},
		"kind":     map[string]any{"type": "string", "enum": []string{Weekly, Daily, Interval, None}},
		"summary":  map[string]any{"type": "string"},
		"evidence": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
		"slots": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"day", "from", "to"},
			"properties": map[string]any{
				"day":  map[string]any{"type": "string", "enum": append(slices.Clone(days), "any")},
				"from": map[string]any{"type": "string"},
				"to":   map[string]any{"type": "string"},
			},
		}},
		"every_hours":  map[string]any{"type": "number"},
		"spread_hours": map[string]any{"type": "number"},
		"confidence":   map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
	},
}

// judge turns the LLM's answer into a Prediction, or one without a pattern and the reason it was dropped. Only
// what the history bears out passes; see the constants above.
func judge(a answer, h History, loc *time.Location) Prediction {
	claimed := a.Pattern && a.Kind != None
	p := Prediction{Kind: None}
	if err := check(a, h, loc, &p); err != nil {
		return Prediction{Kind: None, Dropped: err.Error(), Claimed: claimed}
	}
	p.Pattern, p.Claimed, p.Kind, p.Summary = true, true, a.Kind, trim(a.Summary, 140)
	return p
}

var errNone = errors.New("no pattern")

func check(a answer, h History, loc *time.Location, p *Prediction) error {
	if !a.Pattern || a.Kind == None {
		return errNone
	}
	if a.Confidence != "medium" && a.Confidence != "high" {
		return fmt.Errorf("confidence %q", a.Confidence)
	}
	ins := map[int]bool{} // distinct: [5,5,5] is one arrival
	for _, n := range a.Evidence {
		if n < 1 || n > len(h.Events) || !h.Events[n-1].In {
			return fmt.Errorf("evidence #%d is no arrival", n)
		}
		ins[n] = true
	}
	if len(ins) < minSlotHits {
		return fmt.Errorf("%d distinct evidence, needs %d", len(ins), minSlotHits)
	}
	switch a.Kind {
	case Weekly, Daily:
		if len(a.Slots) == 0 || len(a.Slots) > maxSlots {
			return fmt.Errorf("%d slots", len(a.Slots))
		}
		for _, as := range a.Slots {
			s, err := parseSlot(as, a.Kind)
			if err != nil {
				return err
			}
			if err := s.borne(h, loc); err != nil {
				return err
			}
			p.Slots = append(p.Slots, s)
		}
	case Interval:
		g := h.Gaps()
		every := a.EveryHours
		if g.N < minIns-1 || g.Median <= 0 {
			return fmt.Errorf("%d gaps", g.N)
		}
		if math.Abs(every-g.Median) > intervalTol*g.Median {
			return fmt.Errorf("every %.1f h, measured median %.1f h", every, g.Median)
		}
		if (g.P75-g.P25)/g.Median > maxSpread {
			return fmt.Errorf("gaps irregular (%.1f–%.1f h)", g.P25, g.P75)
		}
		if every*float64(time.Hour) > float64(horizon) {
			return errors.New("interval beyond the horizon")
		}
		// At least an hour either side: perfectly regular gaps and a spread of 0 would make a window of no width.
		spread := math.Min(math.Max(math.Max(a.SpreadHours, (g.P75-g.P25)/2), 1), every/2)
		p.Every = time.Duration(every * float64(time.Hour))
		p.Spread = time.Duration(spread * float64(time.Hour))
		p.Anchor = h.Ins[len(h.Ins)-1]
	default:
		return fmt.Errorf("kind %q", a.Kind)
	}
	return nil
}

func parseSlot(as aslot, kind string) (Slot, error) {
	s := Slot{Weekday: -1}
	if kind == Weekly {
		s.Weekday = slices.Index(days, as.Day)
		if s.Weekday < 0 {
			return s, fmt.Errorf("weekly slot day %q", as.Day)
		}
	}
	var err1, err2 error
	s.From, err1 = clock(as.From)
	s.To, err2 = clock(as.To)
	if err := errors.Join(err1, err2); err != nil {
		return s, err
	}
	// To before From runs past midnight (23:30–00:30); the slot belongs to the day it starts on.
	if w := s.width(); w <= 0 || w > maxSlotWidth {
		return s, fmt.Errorf("slot %s–%s", as.From, as.To)
	}
	return s, nil
}

func (s Slot) width() time.Duration {
	return time.Duration((s.To-s.From+24*60)%(24*60)) * time.Minute
}

// clock reads "06:30".
func clock(s string) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if !ok || err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 || h*60+m > 24*60 {
		return 0, fmt.Errorf("time %q", s)
	}
	return h*60 + m, nil
}

// borne checks that the history bears s out: arrivals in it in at least half the weeks (or days) observed, counted
// per week, not per arrival: stock flapping inside one slot in one week is one week.
func (s Slot) borne(h History, loc *time.Location) error {
	hits := 0
	spans := map[string]bool{}
	for _, t := range h.Ins {
		day, ok := s.holds(t.In(loc))
		if !ok {
			continue
		}
		hits++
		y, w := day.ISOWeek()
		key := fmt.Sprint(y, w)
		if s.Weekday < 0 {
			key = day.Format(time.DateOnly)
		}
		spans[key] = true
	}
	periods := h.Observed.Hours() / (7 * 24)
	need := 2
	if s.Weekday < 0 {
		periods, need = h.Observed.Hours()/24, minSlotHits
	}
	if hits < minSlotHits || len(spans) < need || float64(len(spans)) < minCatch*math.Floor(periods) {
		return fmt.Errorf("slot %s %s: %d arrivals in %d periods of %.0f", dayName(s.Weekday), s.text(), hits,
			len(spans), periods)
	}
	return nil
}

// holds tells whether lt falls in s, with slotSlack either side, and on which day the slot started (the day
// before, for the part of a slot past midnight).
func (s Slot) holds(lt time.Time) (time.Time, bool) {
	m := lt.Hour()*60 + lt.Minute()
	slack := int(slotSlack / time.Minute)
	day := lt
	in := false
	if s.From <= s.To {
		in = m >= s.From-slack && m <= s.To+slack
	} else {
		switch {
		case m >= s.From-slack:
			in = true
		case m <= s.To+slack:
			in, day = true, lt.AddDate(0, 0, -1)
		}
	}
	return day, in && (s.Weekday < 0 || int(day.Weekday()) == s.Weekday)
}

func (s Slot) text() string {
	return fmt.Sprintf("%02d:%02d–%02d:%02d", s.From/60, s.From%60, s.To/60, s.To%60)
}

func dayName(wd int) string {
	if wd < 0 {
		return "daily"
	}
	return days[wd]
}

// roll sets p.Next to the first window that hasn't ended at now, and counts every window that ended since the last
// roll as a try, a hit when an arrival fell in it.
func (p *Prediction) roll(h History, now time.Time, loc *time.Location) {
	if !p.Pattern {
		return
	}
	for !p.Next.To.IsZero() && !p.Next.To.After(now) {
		// Only a window polls watched counts: one that passed while the service was down or OVH unreachable says
		// nothing either way.
		if h.covered(p.Next) {
			p.Tries++
			if slices.ContainsFunc(h.Ins, p.Next.caught) {
				p.Hits++
			}
		}
		p.Next = p.after(p.Next.To, loc)
	}
	if p.Next.To.IsZero() {
		p.Next = p.after(now, loc)
	}
}

// after is the first window ending after t.
func (p Prediction) after(t time.Time, loc *time.Location) Window {
	switch p.Kind {
	case Weekly, Daily:
		lt := t.In(loc)
		// From the day before: a slot past midnight that started yesterday may still be open.
		day0 := time.Date(lt.Year(), lt.Month(), lt.Day()-1, 0, 0, 0, 0, loc)
		for d := 0; d <= 9; d++ {
			day := day0.AddDate(0, 0, d)
			var best Window
			for _, s := range p.Slots {
				if s.Weekday >= 0 && int(day.Weekday()) != s.Weekday {
					continue
				}
				// Wall-clock times, not midnight plus minutes: a DST day is 23 or 25 hours long.
				w := Window{clockOn(day, s.From, loc), clockOn(day, s.To, loc)}
				if s.To < s.From {
					w.To = clockOn(day.AddDate(0, 0, 1), s.To, loc)
				}
				if w.To.After(t) && (best.From.IsZero() || w.From.Before(best.From)) {
					best = w
				}
			}
			if !best.From.IsZero() {
				return best
			}
		}
	case Interval:
		if p.Every <= 0 {
			return Window{}
		}
		c := p.Anchor
		for !c.Add(p.Spread).After(t) {
			c = c.Add(p.Every)
		}
		return Window{c.Add(-p.Spread), c.Add(p.Spread)}
	}
	return Window{}
}

// caught tells whether an arrival at t falls in w, with slotSlack either side.
func (w Window) caught(t time.Time) bool {
	return !t.Before(w.From.Add(-slotSlack)) && !t.After(w.To.Add(slotSlack))
}

// covered tells whether polls watched all of w.
func (h History) covered(w Window) bool {
	return slices.ContainsFunc(h.Polled, func(p watch.Span) bool { return !p.From.After(w.From) && !p.To.Before(w.To) })
}

// clockOn is minute-of-day m on day's date, local wall-clock time.
func clockOn(day time.Time, m int, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, loc)
}

func trim(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
