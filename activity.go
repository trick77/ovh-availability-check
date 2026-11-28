package main

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ovh-restock/internal/watch"
)

// maxActivity is how many entries the page's Activity log shows.
const maxActivity = 40

// activityRow is one line of the Activity log: a server coming in or going out (all its DCs of one poll in one
// row), a price change, or an event without a matching "in" (a digest, an imported push, a restock nobody saw
// arrive). A restock's push is no row of its own: its delivery sits on the "in" it was about (user: no repeats).
type activityRow struct {
	At          time.Time
	FQN         string // with At and Kind the row's key for the live update; a name is shared by a plan's variants
	Kind        string // in, out, price, digest
	Name, Code  string
	Spec, Price string
	Old         string // price: the rent before; Price then is cell-coloured by PriceClass
	PriceClass  string
	DCs         []cell
	After       string // out: "after 3h 6m"; in: "just above" for an unwanted server
	Delivery    cell   // in: the push about it
	Text        string // digest: "Weekly digest · week 47"; imported push: its server line. Never the pushed text (user)

	stayed time.Duration // out: the longest stay among its DCs
}

// activity is the log of scope's stock and prices plus the events, newest first.
func activity(scope []watch.Variant, c watch.Config, s watch.State, now time.Time) []activityRow {
	byFQN := make(map[string]watch.Variant, len(scope))
	for _, v := range scope {
		byFQN[v.FQN] = v
	}
	type key struct {
		at  int64
		fqn string
		in  bool
	}
	var out []activityRow
	at := map[key]int{} // the row of a poll's in or out of one server
	for _, ch := range s.Changes(inScope(scope)) {
		k := key{ch.At.Unix(), ch.FQN, ch.In}
		i, ok := at[k]
		if !ok {
			v := byFQN[ch.FQN]
			// Spec, price and "just above" are the server's now, not at ch.At: the log is about stock, the price
			// history has its own table.
			r := activityRow{At: ch.At, FQN: ch.FQN, Kind: "out", Name: cmp.Or(v.Name, v.PlanCode, ch.FQN),
				Code: code(v.Name, v.PlanCode), Spec: spec(v.CPU, v.RAM, diskStrings(v))}
			if v.Priced {
				r.Price = money(v.Rent) + " €"
			}
			if ch.In {
				r.Kind = "in"
				if !c.Wanted(v) {
					r.After = "just above"
				}
			}
			out = append(out, r)
			i = len(out) - 1
			at[k] = i
		}
		dc := cell{Text: shortDC(ch.DC), Class: "pill", Title: ch.DC}
		if ch.In {
			dc.Class, dc.Title = "pill on", ch.DC+": 1H"
			if ch.Slow {
				dc.Class, dc.Title = "pill slow", ch.DC+": 72H"
			}
		} else if ch.For >= out[i].stayed { // several DCs out at once: the longest stay
			out[i].stayed, out[i].After = ch.For, "after "+compact(ch.For)
		}
		out[i].DCs = append(out[i].DCs, dc)
	}
	// A push is never a line of its own (user): its delivery is a pill on the "in" it was about. That is the arrival
	// at the push's poll, else the server's latest arrival before it (it came in unseen, or turned wanted while in
	// stock). A push with no such "in" shows nowhere. Events are newest first: the newest push about an "in" wins.
	latestIn := func(at time.Time, match func(activityRow) bool) int {
		best := -1
		for i, r := range out {
			if r.Kind == "in" && match(r) && !r.At.After(at) && (best < 0 || r.At.After(out[best].At)) {
				best = i
			}
		}
		return best
	}
	pill := func(i int, e watch.Event) {
		if i >= 0 && out[i].Delivery.Text == "" {
			out[i].Delivery, out[i].After = deliveryCell(e), ""
		}
	}
	for _, e := range s.Events {
		switch {
		case e.Kind == watch.KindDigest:
			out = append(out, activityRow{At: e.At, Kind: "digest", Delivery: deliveryCell(e),
				Text: "Weekly digest · " + strings.TrimPrefix(e.Title, "OVH ")})
		case len(e.Items) == 0: // imported from 0.3: only its text names the server ("KS-2 · Intel … · gra")
			name, _, _ := strings.Cut(firstLine(e.Body), " · ")
			pill(latestIn(e.At, func(r activityRow) bool { return r.Name == name }), e)
		default:
			for _, it := range e.Items {
				i, ok := at[key{e.At.Unix(), it.FQN, true}]
				if !ok {
					pill(latestIn(e.At, func(r activityRow) bool { return r.FQN == it.FQN }), e)
					continue
				}
				pill(i, e)
				// A DC that came in unseen at the same poll (a quiet span) has no "in" of its own, but the push
				// named it: the line shows every DC the push did.
				for _, d := range it.DCs {
					if !slices.ContainsFunc(out[i].DCs, func(x cell) bool { return x.Title == d.DC+": 1H" || x.Title == d.DC+": 72H" }) {
						out[i].DCs = append(out[i].DCs, itemDC(d))
					}
				}
			}
		}
	}
	for _, v := range scope {
		ps := s.Prices[v.FQN]
		for i := 1; i < len(ps); i++ {
			// A setup-only change is in the price table; here it would read "18.99 → 18.99".
			if now.Sub(ps[i].At) > watch.Window || ps[i].Rent == ps[i-1].Rent {
				continue
			}
			r := activityRow{At: ps[i].At, FQN: v.FQN, Kind: "price", Name: cmp.Or(v.Name, v.PlanCode), Code: code(v.Name, v.PlanCode),
				Spec: spec(v.CPU, v.RAM, diskStrings(v)), Old: money(ps[i-1].Rent), Price: money(ps[i].Rent) + " €",
				PriceClass: "down"}
			if ps[i].Rent > ps[i-1].Rent {
				r.PriceClass = "up"
			}
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b activityRow) int { return b.At.Compare(a.At) })
	for i := range out {
		slices.SortFunc(out[i].DCs, func(a, b cell) int { return strings.Compare(a.Text, b.Text) })
	}
	return out[:min(len(out), maxActivity)]
}

// itemDC is a pushed DC as a pill: 1H or 72H.
func itemDC(d watch.ItemDC) cell {
	if d.Slow {
		return cell{Text: shortDC(d.DC), Class: "pill slow", Title: d.DC + ": 72H"}
	}
	return cell{Text: shortDC(d.DC), Class: "pill on", Title: d.DC + ": 1H"}
}

// spec is a server's short line: "Xeon-E3 1230 v6 · 16 GB · 2×450GB NVME".
func spec(cpu string, ram int, disks []string) string {
	var p []string
	if cpu != "" {
		p = append(p, shortCPU(cpu))
	}
	if ram > 0 {
		p = append(p, itoa(ram)+" GB")
	}
	if len(disks) > 0 {
		p = append(p, strings.Join(disks, " + "))
	}
	return strings.Join(p, " · ")
}

func diskStrings(v watch.Variant) []string {
	out := make([]string, len(v.Disks))
	for i, d := range v.Disks {
		out[i] = d.String()
	}
	return out
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// deliveryCell is a push's delivery as a pill.
func deliveryCell(e watch.Event) cell {
	switch {
	case e.Delivery == watch.Sent:
		return cell{Text: e.Delivery, Class: "pill sent"}
	case e.Delivery == watch.NoChannel:
		return cell{Text: e.Delivery, Class: "pill faint", Title: "BACKEND_NOTIFY_URLS has no channel on"}
	case e.Retry:
		return cell{Text: e.Delivery + " · retrying", Class: "pill bad", Title: "sent again each poll while in stock"}
	}
	return cell{Text: e.Delivery, Class: "pill bad"}
}
