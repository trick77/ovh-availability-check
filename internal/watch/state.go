package watch

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/trick77/ovh-availability-check/internal/ovh"
)

// Window is how far back the history goes.
const Window = 30 * 24 * time.Hour

const (
	seenKeep  = 180 * 24 * time.Hour // "last in stock" outlives the history
	priceKeep = 365 * 24 * time.Hour
	maxSpans  = 2000 // per pair: one flapping every poll for 30 days stays below 1 MB
	maxPushes = 20
)

// State is data/state.json: what was pushed, plus the history the page shows.
type State struct {
	// Notified are the pairs already pushed. It holds what was notified, not the last snapshot, so a wish added later
	// still fires for stock that is already there.
	Notified []string `json:"notified"`
	// Since is the first observed poll: the page's "never" means never since then.
	Since time.Time `json:"since,omitzero"`
	// PolledAt is the last observed poll. A longer pause than gapAfter intervals ends every streak: nobody saw what
	// happened in between.
	PolledAt time.Time `json:"polled_at,omitzero"`
	// Seen is every variant ever in stock in a watched DC, wanted or not, by fqn. Per variant, not per pair: the
	// live API has ~5700 pairs in stock, 1.2 MB rewritten every poll (2026-11).
	Seen map[string]Seen `json:"seen,omitempty"`
	// Polled are the stretches polls covered; outside them the history has no data.
	Polled []Span `json:"polled,omitempty"`
	// Spans are the in-stock stretches of wanted pairs, by Key, oldest first.
	Spans map[string][]Span `json:"spans,omitempty"`
	// Prices are each variant's price points by fqn, one per change, oldest first. They live in prices.json, written
	// only when a price moves: every variant's, ~0.5 MB.
	Prices map[string][]Price `json:"-"`
	// Pushes are the pushes sent or tried, newest first.
	Pushes []Push `json:"pushes,omitempty"`

	// lastIn are the pairs in stock at the last poll, wanted or not: a pair that turns wanted while in stock came in
	// unseen. Memory only (~5700 pairs); nil after a restart, when every new span is quiet.
	lastIn map[string]bool
}

// Seen is one variant's stock streak, in any watched DC.
type Seen struct {
	Since time.Time `json:"since"` // the current or last streak began
	Last  time.Time `json:"last"`  // last poll that saw it in stock
	In    bool      `json:"in"`    // in stock at the last poll
}

// Span is a stretch of time. Slow: orderable with 72H delivery rather than 1H. Gone is the poll that found it sold
// out; zero while open or when polling stopped. Quiet: nobody saw it come in (first poll, after a gap or a restart,
// or already in stock when it turned wanted), so the log has no "in" for it.
type Span struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Gone  time.Time `json:"gone,omitzero"`
	Slow  bool      `json:"slow,omitempty"`
	Quiet bool      `json:"quiet,omitempty"`
}

// Price is a variant's catalog price from At on.
type Price struct {
	At    time.Time `json:"at"`
	Rent  float64   `json:"rent"`
	Setup float64   `json:"setup"`
}

// Push is one push sent, or failed.
type Push struct {
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Err   string    `json:"err,omitempty"`
}

// Key names a pair in the state: fqn@dc.
func Key(fqn, dc string) string { return fqn + "@" + dc }

// gapAfter intervals plus gapSlack without a poll break the history. The slack covers the polls' own time: polls
// are an interval apart after each one ends, and a hanging catalog takes 2×90 s.
const (
	gapAfter = 3
	gapSlack = 5 * time.Minute
)

// pricesPath is prices.json next to state.json.
func pricesPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "prices.json")
}

// LoadState reads path and the prices.json next to it; a missing file is empty.
func LoadState(path string) (State, error) {
	var s State
	if err := readJSON(path, &s); err != nil {
		return State{}, err
	}
	if err := readJSON(pricesPath(path), &s.Prices); err != nil {
		return State{}, err
	}
	return s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's BACKEND_DATA_DIR
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Save writes s to path, without the prices.
func (s State) Save(path string) error { return writeJSON(path, s) }

// SavePrices writes the prices next to path.
func (s State) SavePrices(path string) error { return writeJSON(pricesPath(path), s.Prices) }

// writeJSON writes atomically: a crash mid-write must not leave a broken file that re-pushes everything.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename: only a failed write leaves it
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// New are the pairs of cur not pushed yet, sorted.
func (s State) New(cur map[string]Pair) []Pair {
	var out []Pair
	for k, p := range cur {
		if !slices.Contains(s.Notified, k) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Pair) int { return cmp.Compare(a.key(), b.key()) })
	return out
}

// Next is the state after a poll: what was pushed and is still in stock, plus fresh when its push went out. A pair
// out of stock drops out, so it fires again when it returns; sold out itself sends nothing.
func (s State) Next(cur map[string]Pair, fresh []Pair, sent bool) State {
	var out []string
	for _, k := range s.Notified {
		if _, ok := cur[k]; ok {
			out = append(out, k)
		}
	}
	if sent {
		for _, p := range fresh {
			out = append(out, p.key())
		}
	}
	slices.Sort(out)
	s.Notified = slices.Compact(out)
	return s
}

// AddPush keeps p, newest first.
func (s State) AddPush(p Push) State {
	s.Pushes = append([]Push{p}, s.Pushes...)[:min(len(s.Pushes)+1, maxPushes)]
	return s
}

// Observe records a good poll at now. It never changes s's maps or slices in place: the page reads the old state
// while the next is built.
func (s State) Observe(vs []Variant, wanted func(Variant) bool, now time.Time, every time.Duration) State {
	now = stamp(now)
	prev := s.PolledAt
	gap := prev.IsZero() || now.Sub(prev) > gapAfter*every+gapSlack
	if s.Since.IsZero() {
		s.Since = now
	}
	s.PolledAt = now

	polled := slices.Clone(s.Polled)
	if n := len(polled); n > 0 && !gap && polled[n-1].To.Equal(prev) {
		polled[n-1].To = now
	} else {
		polled = append(polled, Span{From: now, To: now})
	}
	s.Polled = cutSpans(polled, now.Add(-Window))

	seen := make(map[string]Seen, len(s.Seen))
	for k, e := range s.Seen {
		if gap {
			e.In = false
		}
		seen[k] = e
	}
	spans := make(map[string][]Span, len(s.Spans))
	for k, sp := range s.Spans {
		spans[k] = sp
	}
	in, pairs, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range vs {
		w := wanted(v)
		for dc, a := range v.Stock {
			listed[Key(v.FQN, dc)] = true
			if !ovh.InStock(a) {
				continue
			}
			if !in[v.FQN] {
				in[v.FQN] = true
				e := seen[v.FQN]
				if !e.In {
					e.Since, e.In = now, true
				}
				e.Last = now
				seen[v.FQN] = e
			}
			k := Key(v.FQN, dc)
			pairs[k] = true
			// A pair that stops being wanted while in stock (a price rise, a config edit) keeps its open span until it
			// sells out: otherwise the log would claim an "out".
			if w || isOpen(spans[k], prev, gap) {
				quiet := gap || s.lastIn == nil || s.lastIn[k]
				spans[k] = extend(spans[k], now, prev, gap, a == "72H", quiet)
			}
		}
	}
	// Gone needs the pair still listed and out of stock. A pair no longer listed (a DC dropped from the config, a
	// variant OVH stopped listing) just stops: nobody saw it sell out.
	for k, sp := range spans {
		if n := len(sp); !pairs[k] && listed[k] && isOpen(sp, prev, gap) {
			sp = slices.Clone(sp)
			sp[n-1].Gone = now
			spans[k] = sp
		}
	}
	s.lastIn = pairs
	for k, e := range seen {
		if e.In && !in[k] {
			e.In = false
			seen[k] = e
		}
		if now.Sub(e.Last) > seenKeep {
			delete(seen, k)
		}
	}
	for k, sp := range spans {
		sp = cutSpans(sp, now.Add(-Window))
		if len(sp) == 0 {
			delete(spans, k)
			continue
		}
		spans[k] = sp
	}
	s.Seen, s.Spans = seen, spans
	return s
}

// stamp is a time as the state keeps it: UTC, whole seconds, so the JSON stays short ("2026-11-27T16:28:12Z").
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// isOpen tells whether a pair's last span was in stock at the previous poll.
func isOpen(sp []Span, prev time.Time, gap bool) bool {
	n := len(sp)
	return n > 0 && !gap && sp[n-1].To.Equal(prev) && sp[n-1].Gone.IsZero()
}

// extend adds the poll at now to a pair's spans: the open one grows when the pair was in stock at the previous poll
// (prev) with the same delivery; a switch between 1H and 72H starts a new span where the old one ends.
func extend(sp []Span, now, prev time.Time, gap, slow, quiet bool) []Span {
	n := len(sp)
	open := isOpen(sp, prev, gap)
	switch {
	case open && sp[n-1].Slow == slow:
		sp = slices.Clone(sp)
		sp[n-1].To = now
	case open:
		sp = append(slices.Clip(sp), Span{From: sp[n-1].To, To: now, Slow: slow})
	default:
		sp = append(slices.Clip(sp), Span{From: now, To: now, Slow: slow, Quiet: quiet})
	}
	if cut := len(sp) - maxSpans; cut > 0 {
		cont := sp[cut-1].To.Equal(sp[cut].From)
		sp = slices.Clone(sp[cut:])
		// The first kept span continues one that is gone now: its start was no "in".
		sp[0].Quiet = sp[0].Quiet || cont
	}
	return sp
}

// cutSpans drops spans that ended before from.
func cutSpans(sp []Span, from time.Time) []Span {
	i := 0
	for i < len(sp) && sp[i].To.Before(from) {
		i++
	}
	if i == 0 {
		return sp
	}
	return slices.Clone(sp[i:])
}

// ObservePrices records each priced variant's price when it differs from the last one kept. Points older than a year
// go, except the last of them: it is the baseline the next change compares to. changed tells whether prices.json
// needs writing.
func (s State) ObservePrices(vs []Variant, now time.Time) (_ State, changed bool) {
	now = stamp(now)
	var prices map[string][]Price
	own := func() {
		if prices == nil {
			prices = make(map[string][]Price, len(s.Prices)+1)
			for k, p := range s.Prices {
				prices[k] = p
			}
		}
	}
	listed := map[string]bool{}
	for _, v := range vs {
		listed[v.FQN] = true
		if !v.Priced {
			continue
		}
		ps := s.Prices[v.FQN]
		if prices != nil {
			ps = prices[v.FQN]
		}
		if n := len(ps); n > 0 && ps[n-1].Rent == v.Rent && ps[n-1].Setup == v.Setup {
			continue
		}
		own()
		ps = append(slices.Clip(ps), Price{At: now, Rent: v.Rent, Setup: v.Setup})
		i := 0
		for i+1 < len(ps) && now.Sub(ps[i+1].At) > priceKeep {
			i++
		}
		prices[v.FQN] = ps[i:]
	}
	// A variant OVH no longer lists goes once its last price is a year old.
	for k, ps := range s.Prices {
		if !listed[k] && (len(ps) == 0 || now.Sub(ps[len(ps)-1].At) > priceKeep) {
			own()
			delete(prices, k)
		}
	}
	if prices == nil {
		return s, false
	}
	s.Prices = prices
	return s, true
}

// Change is one entry of the stock log: a wanted pair came in or went out.
type Change struct {
	At      time.Time
	FQN, DC string
	In      bool
	Slow    bool
	For     time.Duration // how long it was in stock, on an out
}

// Changes are the stock log, newest first: a span's start unless it is Quiet, its Gone. A switch between 1H and 72H
// is no change. For runs from the streak's start to the poll that found it gone.
func (s State) Changes() []Change {
	var out []Change
	for k, sp := range s.Spans {
		fqn, dc := splitKey(k)
		for i, x := range sp {
			cont := i > 0 && sp[i-1].To.Equal(x.From)
			if !cont && !x.Quiet {
				out = append(out, Change{At: x.From, FQN: fqn, DC: dc, In: true, Slow: x.Slow})
			}
			if !x.Gone.IsZero() {
				from := x.From
				for j := i; j > 0 && sp[j-1].To.Equal(sp[j].From); j-- {
					from = sp[j-1].From
				}
				out = append(out, Change{At: x.Gone, FQN: fqn, DC: dc, For: x.Gone.Sub(from)})
			}
		}
	}
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.FQN, b.FQN), cmp.Compare(a.DC, b.DC))
	})
	return out
}

func splitKey(k string) (fqn, dc string) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '@' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
