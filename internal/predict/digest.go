package predict

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ovh-availability-check/internal/watch"
)

// The weekly digest: Sunday 18:00 local, over the 7 days before.
const (
	digestDay  = time.Sunday
	digestHour = 18
	// maxDigest keeps it under Pushover's 1024 characters with the title's room.
	maxDigest = 900
)

// DigestDue tells whether the digest for the week ending at now is due, given when the last one went out. The
// caller passes the start of tracking when none went out yet: a fresh deploy must not send last week's at once.
func DigestDue(now, last time.Time, loc *time.Location) bool {
	slot := lastSlot(now, loc)
	return !now.Before(slot) && last.Before(slot)
}

// lastSlot is the newest digest time at or before now.
func lastSlot(now time.Time, loc *time.Location) time.Time {
	lt := now.In(loc)
	d := time.Date(lt.Year(), lt.Month(), lt.Day(), digestHour, 0, 0, 0, loc)
	for d.Weekday() != digestDay || d.After(lt) {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// DigestServer is one in-scope server's week.
type DigestServer struct {
	Name    string   `json:"name"`
	Config  string   `json:"config"`
	Price   string   `json:"price_eur_month"`
	Near    string   `json:"just_above,omitempty"` // which rule it misses, for one just above a wish
	Windows int      `json:"stock_windows"`
	Stays   string   `json:"stays,omitempty"` // "20m–40m"
	DCs     []string `json:"dcs,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Hits    string   `json:"predicted_windows_hit,omitempty"` // "2 of 3"
}

// Digest is the week's data; the model only words it.
type Digest struct {
	Week    string         `json:"week"`
	Servers []DigestServer `json:"servers"`
	Prices  []string       `json:"price_changes,omitempty"`
	New     []string       `json:"new_just_above,omitempty"`
}

// Empty tells whether nothing happened: then nothing is sent.
func (d Digest) Empty() bool { return len(d.Servers) == 0 && len(d.Prices) == 0 && len(d.New) == 0 }

// BuildDigest collects the week before now: stock windows of in-scope servers, price changes, predictions, servers
// newly just above a wish.
func BuildDigest(st watch.State, vs []watch.Variant, c watch.Config, preds map[string]Prediction, now time.Time,
	loc *time.Location) Digest {
	from := now.Add(-7 * 24 * time.Hour)
	_, wk := now.In(loc).ISOWeek()
	d := Digest{Week: fmt.Sprint(wk)}
	for _, v := range vs {
		if !c.InScope(v) {
			continue
		}
		// Arrivals as the prediction counts them: one restock in several DCs at once is one window.
		ins, dcs, stays := 0, []string{}, []time.Duration{}
		for _, e := range Of(st, v.FQN, now).Events {
			if !e.In || e.At.Before(from) {
				continue
			}
			ins++
			for _, dc := range e.DCs {
				if !slices.Contains(dcs, dc) {
					dcs = append(dcs, dc)
				}
			}
			if e.For > 0 {
				stays = append(stays, e.For)
			}
		}
		p, hasP := preds[v.FQN]
		if ins == 0 && (!hasP || !p.Pattern) {
			continue
		}
		ds := DigestServer{Name: cmp.Or(v.Name, v.PlanCode), Config: config(v), Price: money(v.Rent),
			Windows: ins, DCs: dcs}
		if !v.Priced {
			ds.Price = "unknown"
		}
		ds.Near, _ = c.NearMiss(v)
		slices.Sort(ds.DCs)
		if len(stays) > 0 {
			ds.Stays = dur(slices.Min(stays)) + "–" + dur(slices.Max(stays))
		}
		if hasP && p.Pattern {
			ds.Pattern = p.Summary
			if p.Tries > 0 {
				ds.Hits = fmt.Sprintf("%d of %d", p.Hits, p.Tries)
			}
		}
		d.Servers = append(d.Servers, ds)
	}
	for _, v := range vs {
		if !c.InScope(v) {
			continue
		}
		ps := st.Prices[v.FQN]
		for i := 1; i < len(ps); i++ {
			if !ps[i].At.Before(from) && ps[i].Rent != ps[i-1].Rent {
				d.Prices = append(d.Prices, fmt.Sprintf("%s %s: %s → %s", cmp.Or(v.Name, v.PlanCode), config(v),
					money(ps[i-1].Rent), money(ps[i].Rent)))
			}
		}
		// New: first priced this week, after the first day of tracking (the first catalog prices everything).
		if why, ok := c.NearMiss(v); ok && len(ps) > 0 && !ps[0].At.Before(from) && ps[0].At.Sub(st.Since) > 24*time.Hour {
			d.New = append(d.New, fmt.Sprintf("%s %s %s (%s)", cmp.Or(v.Name, v.PlanCode), config(v), money(v.Rent), why))
		}
	}
	return d
}

var digestSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"text"},
	"properties": map[string]any{"text": map[string]any{"type": "string"}},
}

const digestPrompt = `You write a short weekly notification about dedicated servers a user watches at a hosting ` +
	`provider. The data has, per server, its stock windows this week, whether a restock rhythm is known and how its ` +
	`predicted windows went, price changes and servers newly just above the user's wishes.

Write plain English, at most 800 characters, no markdown, no greeting. Lead with what matters: servers that were ` +
	`in stock, and when; then price changes; then new servers. Use only figures from the data, written exactly as ` +
	`there; invent nothing, compute nothing new. Skip what is empty.`

// Words is the digest's push: the model's words when every figure in them is in the data, the plain list otherwise.
func (s *Service) Words(ctx context.Context, d Digest) (title, body string) {
	title = "OVH week " + d.Week
	plain := Plain(d)
	obj, err := s.call(ctx, "digest", digestSchema, digestPrompt, d, 600)
	if err != nil {
		s.o.Log.Warn("digest", "err", err)
		return title, plain
	}
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(obj), &a); err != nil || !Grounded(a.Text, d) {
		s.o.Log.Warn("digest: model text dropped", "err", err)
		return title, plain
	}
	return title, strings.TrimSpace(a.Text)
}

var numRE = regexp.MustCompile(`\d+(?:[.:]\d+)?`)

// Grounded tells whether text fits the push and every figure in it appears in d.
func Grounded(text string, d Digest) bool {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > maxDigest {
		return false
	}
	b, _ := json.Marshal(d)
	known := map[string]bool{}
	for _, n := range numRE.FindAllString(string(b)+" "+Plain(d), -1) {
		known[n] = true
	}
	for _, n := range numRE.FindAllString(text, -1) {
		if !known[n] {
			return false
		}
	}
	return true
}

// Plain is the digest without a model.
func Plain(d Digest) string {
	var lines []string
	for _, s := range d.Servers {
		price := s.Price + " €"
		if s.Price == "unknown" {
			price = "price unknown"
		}
		l := fmt.Sprintf("%s %s %s: %d windows", s.Name, s.Config, price, s.Windows)
		if s.Stays != "" {
			l += ", " + s.Stays
		}
		if len(s.DCs) > 0 {
			l += ", " + strings.Join(s.DCs, ", ")
		}
		if s.Pattern != "" {
			l += " · " + s.Pattern
			if s.Hits != "" {
				l += " (" + s.Hits + " hit)"
			}
		}
		if s.Near != "" {
			l += " · just above: " + s.Near
		}
		lines = append(lines, l)
	}
	lines = append(lines, d.Prices...)
	for _, n := range d.New {
		lines = append(lines, "New: "+n)
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > maxDigest {
		out = string(r[:maxDigest-1]) + "…"
	}
	return out
}

func config(v watch.Variant) string {
	parts := []string{fmt.Sprintf("%d GB", v.RAM)}
	for _, d := range v.Disks {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, " ")
}

func money(f float64) string { return fmt.Sprintf("%.2f", f) }

func dur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
