package watch

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/trick77/ovh-restock/internal/ovh"
)

// Window is how far back the page's history goes; Keep how far back the state keeps it. Patterns are looked for in
// Keep: four weeks are only four repeats of a weekly rhythm.
const (
	Window = 30 * 24 * time.Hour
	Keep   = 90 * 24 * time.Hour
)

const (
	seenKeep  = 180 * 24 * time.Hour // "last in stock" outlives the history
	priceKeep = 365 * 24 * time.Hour
	maxSpans  = 2000 // per pair: one flapping every poll for weeks stays bounded
	maxEvents = 200  // besides Keep: a wide wish firing every poll stays bounded; the page shows 20
)

// State is what was evented, plus the history the page shows. internal/store keeps it in SQLite and writes only
// what changed between two states.
type State struct {
	// Notified are the pairs that got a restock event, delivered or not. It holds what was evented, not the last
	// snapshot, so a wish added later still fires for stock that is already there.
	Notified []string
	// Since is the first observed poll: the page's "never" means never since then.
	Since time.Time
	// AllSince is the first poll that tracked every pair, not only the in-scope ones (until 0.4 only those had
	// spans). A variant's history counts from here at the latest: before, nobody watched it.
	AllSince time.Time
	// PolledAt is the last observed poll. A longer pause than gapAfter intervals ends every streak: nobody saw what
	// happened in between.
	PolledAt time.Time
	// Seen is every variant ever in stock in a watched DC by fqn: the current or last streak.
	Seen map[string]Seen
	// Polled are the stretches polls covered; outside them the history has no data.
	Polled []Span
	// Spans are the in-stock stretches of every pair in the watched DCs, by fqn, then DC, oldest first. Every pair,
	// not only the wanted ones (user): a new wish must not wait weeks for history.
	Spans map[string]map[string][]Span
	// Prices are each variant's price points by fqn, one per change, oldest first.
	Prices map[string][]Price
	// Events are the restocks and digests, newest first, delivered or not.
	Events []Event
	// DigestAt is when the last weekly digest was made: a restart must not make it twice.
	DigestAt time.Time

	// warm: a poll ran since the start. The first poll's new spans are quiet: what is in stock then came in unseen
	// (a restart, the first poll after the import that started tracking every pair).
	warm bool
}

// Event kinds.
const (
	KindRestock = "restock"
	KindDigest  = "digest"
)

// Delivery values besides "failed on …" and "partly failed on …".
const (
	Sent      = "sent"
	NoChannel = "no channel"
)

// Pairs are the event's pairs.
func (e Event) Pairs() []Pair {
	var out []Pair
	for _, it := range e.Items {
		for _, d := range it.DCs {
			out = append(out, Pair{it.FQN, d.DC})
		}
	}
	return out
}

// Key names a pair in the state: fqn@dc.
func Key(fqn, dc string) string { return fqn + "@" + dc }

// gapAfter intervals plus gapSlack without a poll break the history. The slack covers the polls' own time: polls
// are an interval apart after each one ends, and a hanging catalog takes 2×90 s.
const (
	gapAfter = 3
	gapSlack = 5 * time.Minute
)

// New are the pairs of cur not evented yet, sorted.
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

// Next is the state after a poll: what was evented and is still in stock, plus fresh. A pair out of stock drops
// out, so it fires again when it returns; sold out itself makes no event.
func (s State) Next(cur map[string]Pair, fresh []Pair) State {
	var out []string
	for _, k := range s.Notified {
		if _, ok := cur[k]; ok {
			out = append(out, k)
		}
	}
	for _, p := range fresh {
		out = append(out, p.key())
	}
	slices.Sort(out)
	s.Notified = slices.Compact(out)
	return s
}

// AddEvent keeps e, newest first. An event of the same At and Kind is replaced.
func (s State) AddEvent(e Event) State {
	e.At = stamp(e.At)
	out := make([]Event, 0, len(s.Events)+1)
	out = append(out, e)
	for _, o := range s.Events {
		if !o.At.Equal(e.At) || o.Kind != e.Kind {
			out = append(out, o)
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { return b.At.Compare(a.At) })
	s.Events = out[:min(len(out), maxEvents)]
	return s
}

// SetDelivery records a retry's outcome on the event e names.
func (s State) SetDelivery(e Event, delivery string, retry bool) State {
	out := slices.Clone(s.Events)
	for i, o := range out {
		if o.At.Equal(e.At) && o.Kind == e.Kind {
			out[i].Delivery, out[i].Retry = delivery, retry
		}
	}
	s.Events = out
	return s
}

// Observe records a good poll at now: every pair of vs, wanted or not. It never changes s's maps or slices in
// place: the page reads the old state while the next is built, and the store diffs the two.
func (s State) Observe(vs []Variant, now time.Time, every time.Duration) State {
	now = stamp(now)
	prev := s.PolledAt
	gap := prev.IsZero() || now.Sub(prev) > gapAfter*every+gapSlack
	if s.Since.IsZero() {
		s.Since = now
	}
	if s.AllSince.IsZero() {
		s.AllSince = now
	}
	s.PolledAt = now
	quiet := gap || !s.warm

	polled := slices.Clone(s.Polled)
	if n := len(polled); n > 0 && !gap && polled[n-1].To.Equal(prev) {
		polled[n-1].To = now
	} else {
		polled = append(polled, Span{From: now, To: now})
	}
	s.Polled = cutSpans(polled, now.Add(-Keep))

	seen := make(map[string]Seen, len(s.Seen))
	for k, e := range s.Seen {
		if gap {
			e.In = false
		}
		seen[k] = e
	}
	spans := maps.Clone(s.Spans)
	if spans == nil {
		spans = map[string]map[string][]Span{}
	}
	owned := map[string]bool{}
	// own is fqn's DC map, cloned on first write: the old state's stays as it was.
	own := func(fqn string) map[string][]Span {
		if !owned[fqn] {
			m := maps.Clone(spans[fqn])
			if m == nil {
				m = map[string][]Span{}
			}
			spans[fqn], owned[fqn] = m, true
		}
		return spans[fqn]
	}
	in, pairs, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range vs {
		for dc, a := range v.Stock {
			k := Key(v.FQN, dc)
			listed[k] = true
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
			pairs[k] = true
			m := own(v.FQN)
			m[dc] = extend(m[dc], now, prev, gap, a == "72H", quiet)
		}
	}
	// Gone needs the pair still listed and out of stock. A pair no longer listed (a DC dropped from the config, a
	// variant OVH stopped listing) just stops: nobody saw it sell out. Cut: spans older than Keep go.
	for fqn, m := range spans {
		for dc, sp := range m {
			k := Key(fqn, dc)
			cut := sp
			if n := len(sp); !pairs[k] && listed[k] && isOpen(sp, prev, gap) {
				cut = slices.Clone(sp)
				cut[n-1].Gone = now
			}
			cut = cutSpans(cut, now.Add(-Keep))
			switch {
			case len(cut) == 0:
				delete(own(fqn), dc)
			case len(cut) != len(sp) || &cut[0] != &sp[0]:
				own(fqn)[dc] = cut
			}
		}
		if len(spans[fqn]) == 0 {
			delete(spans, fqn)
		}
	}
	for k, e := range seen {
		if e.In && !in[k] {
			e.In = false
			seen[k] = e
		}
		if now.Sub(e.Last) > seenKeep {
			delete(seen, k)
		}
	}
	if n := len(s.Events); n > 0 && now.Sub(s.Events[n-1].At) > Keep {
		i := slices.IndexFunc(s.Events, func(e Event) bool { return now.Sub(e.At) > Keep })
		var kept []Event
		if i > 0 {
			kept = slices.Clone(s.Events[:i])
		}
		s.Events = kept
	}
	s.Seen, s.Spans, s.warm = seen, spans, true
	return s
}

// stamp is a time as the state keeps it: UTC, whole seconds (the store writes unix seconds).
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
// go, except the last of them: it is the baseline the next change compares to.
func (s State) ObservePrices(vs []Variant, now time.Time) State {
	now = stamp(now)
	var prices map[string][]Price
	own := func() {
		if prices == nil {
			prices = maps.Clone(s.Prices)
			if prices == nil {
				prices = map[string][]Price{}
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
	if prices != nil {
		s.Prices = prices
	}
	return s
}

// TrackedFrom is when fqn's history starts: AllSince (every pair is tracked from there on), or Since for a variant
// with a span before AllSince: in scope under 0.3, so watched all along (its first kept span may be recent, and
// counting from there would drop its prediction at the upgrade). Before it, no data: not sold out.
func (s State) TrackedFrom(fqn string) time.Time {
	for _, sp := range s.Spans[fqn] {
		if len(sp) > 0 && sp[0].From.Before(s.AllSince) {
			return s.Since
		}
	}
	return s.AllSince
}

// Change is one entry of the stock log: a pair came in or went out.
type Change struct {
	At      time.Time
	FQN, DC string
	In      bool
	Slow    bool
	For     time.Duration // how long it was in stock, on an out
}

// Changes are the stock log of the variants keep takes (nil: all), newest first: a span's start unless it is Quiet,
// its Gone. A switch between 1H and 72H is no change. For runs from the streak's start to the poll that found it
// gone.
func (s State) Changes(keep func(fqn string) bool) []Change {
	var out []Change
	for fqn, m := range s.Spans {
		if keep == nil || keep(fqn) {
			out = appendChanges(out, fqn, m)
		}
	}
	sortChanges(out)
	return out
}

// ChangesOf is Changes for one variant.
func (s State) ChangesOf(fqn string) []Change {
	out := appendChanges(nil, fqn, s.Spans[fqn])
	sortChanges(out)
	return out
}

func appendChanges(out []Change, fqn string, m map[string][]Span) []Change {
	for dc, sp := range m {
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
	return out
}

func sortChanges(out []Change) {
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.FQN, b.FQN), cmp.Compare(a.DC, b.DC))
	})
}

// SplitKey splits fqn@dc.
func SplitKey(k string) (fqn, dc string) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '@' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
