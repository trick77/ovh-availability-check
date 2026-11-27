package predict

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/toolkit/llmwire"

	"github.com/trick77/ovh-restock/internal/watch"
)

// The eval runs the pattern prompt against real models on synthetic histories and scores it in code: no LLM
// judges. It costs money, so it only runs with BACKEND_EVAL=1:
//
//	BACKEND_EVAL=1 BACKEND_EVAL_ENV=../ai-keys/mimo.env go test ./internal/predict -run Eval -v -count=2
//
// BACKEND_EVAL_ENV lists env files (KEY=value lines) loaded before the clients are built. Evals run on MiMo flash
// only (user: evals with MiMo; the production model is never eval'd without an explicit go); BACKEND_EVAL_MODELS
// overrides it. Spend is capped at evalMaxUSD per run.
const evalMaxUSD = 0.50

type evalCase struct {
	name string
	kind string // expected: weekly, daily, interval or none
	// gen makes arrivals over [now-90d, now+14d]; the first one after now is held out and must fall in the window.
	gen  func(r *rand.Rand) []time.Time
	gaps []watch.Span
}

func evalCases() []evalCase {
	wk := func(wds []time.Weekday, hh, mm int, jitter time.Duration, miss float64) func(*rand.Rand) []time.Time {
		return func(r *rand.Rand) []time.Time {
			return future(r, func(r *rand.Rand) []time.Time {
				return weekly(r, wds, hh, mm, jitter, miss)
			})
		}
	}
	tue, thu := time.Tuesday, time.Thursday
	cases := []evalCase{
		// The user's example.
		{"tue+thu 06:00 ±30m", Weekly, wk([]time.Weekday{tue, thu}, 6, 0, 30*time.Minute, 0), nil},
		{"tue+thu 06:00 ±30m, weeks missed", Weekly, wk([]time.Weekday{tue, thu}, 6, 0, 30*time.Minute, 0.15), nil},
		{"mon 14:00 ±20m", Weekly, wk([]time.Weekday{time.Monday}, 14, 0, 20*time.Minute, 0), nil},
		{"fri 22:00 ±45m", Weekly, wk([]time.Weekday{time.Friday}, 22, 0, 45*time.Minute, 0.1), nil},
		{"wed+sat 09:00 ±15m", Weekly, wk([]time.Weekday{time.Wednesday, time.Saturday}, 9, 0, 15*time.Minute, 0.2), nil},
		{"tue 06:00, 10-day poll gap", Weekly, wk([]time.Weekday{tue}, 6, 0, 20*time.Minute, 0),
			[]watch.Span{{From: nowT.AddDate(0, 0, -40), To: nowT.AddDate(0, 0, -30)}}},
		{"daily 03:00 ±20m", Daily, func(r *rand.Rand) []time.Time {
			return future(r, func(r *rand.Rand) []time.Time { return daily(r, 40, 3, 0, 20*time.Minute, 0) })
		}, nil},
		{"daily 11:00 ±40m, days missed", Daily, func(r *rand.Rand) []time.Time {
			return future(r, func(r *rand.Rand) []time.Time { return daily(r, 40, 11, 0, 40*time.Minute, 0.2) })
		}, nil},
		{"every 9 d ±12h", Interval, intervalGen(9*24*time.Hour, 12*time.Hour), nil},
		{"every 4 d ±6h", Interval, intervalGen(4*24*time.Hour, 6*time.Hour), nil},
		{"every 6 d ±8h", Interval, intervalGen(6*24*time.Hour, 8*time.Hour), nil},
	}
	for i, n := range []int{8, 12, 16, 20, 25, 30, 40} {
		seed := uint64(100 + i)
		cases = append(cases, evalCase{fmt.Sprintf("noise %d arrivals", n), None, func(*rand.Rand) []time.Time {
			return noise(rand.New(rand.NewPCG(seed, 7)), n)
		}, nil})
	}
	// Near-noise: always on Tuesdays, but at random times; and a real rhythm too rare to count.
	cases = append(cases,
		evalCase{"tuesdays at random times", None, func(r *rand.Rand) []time.Time {
			var out []time.Time
			for _, t := range weekly(r, []time.Weekday{tue}, 0, 0, 0, 0.3) {
				out = append(out, t.Add(time.Duration(r.IntN(24*60))*time.Minute))
			}
			return out
		}, nil},
		evalCase{"3 tue 06:00 in noise", None, func(r *rand.Rand) []time.Time {
			w := weekly(r, []time.Weekday{tue}, 6, 0, 10*time.Minute, 0)
			return append(noise(r, 12), w[len(w)-3:]...)
		}, nil},
	)
	return cases
}

// future runs gen with now moved two weeks on, so the history has arrivals after now to hold out.
func future(r *rand.Rand, gen func(*rand.Rand) []time.Time) []time.Time {
	saved := nowT
	nowT = nowT.Add(14 * 24 * time.Hour)
	defer func() { nowT = saved }()
	return gen(r)
}

func intervalGen(gap, jitter time.Duration) func(*rand.Rand) []time.Time {
	return func(r *rand.Rand) []time.Time {
		var out []time.Time
		for t := nowT.Add(-watch.Keep).Add(gap / 3); t.Before(nowT.Add(14 * 24 * time.Hour)); t = t.Add(gap) {
			out = append(out, t.Add(jit(r, jitter)))
		}
		return out
	}
}

func TestEvalPattern(t *testing.T) {
	if os.Getenv("BACKEND_EVAL") != "1" {
		t.Skip("costs money: set BACKEND_EVAL=1 (see the comment at the top)")
	}
	for _, f := range strings.Split(os.Getenv("BACKEND_EVAL_ENV"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			loadEnv(t, f)
		}
	}
	models := strings.Split(cmpOr(os.Getenv("BACKEND_EVAL_MODELS"), "mimo-v2.6-flash"), ",")
	var spent float64
	for _, model := range models {
		client, err := FromEnv(model, llmwire.Config{})
		if err != nil || client == nil {
			t.Fatalf("%s: no client (key missing?): %v", model, err)
		}
		s, err := New(Options{Client: client, Model: model, DailyCalls: 1000, Path: filepath.Join(t.TempDir(), "p.json"),
			Loc: zurich})
		if err != nil {
			t.Fatal(err)
		}
		var right, kinds, hits, patterned, noiseClaimed, noisePassed, noiseN int
		for i, c := range evalCases() {
			if spent+s.Spent() > evalMaxUSD {
				t.Fatalf("spend cap $%.2f reached", evalMaxUSD)
			}
			r := rand.New(rand.NewPCG(uint64(i+1), 42))
			all := c.gen(r)
			var past []time.Time
			var held time.Time
			slices.SortFunc(all, func(a, b time.Time) int { return a.Compare(b) })
			for _, a := range all {
				if !a.After(nowT) {
					past = append(past, a)
				} else if held.IsZero() {
					held = a
				}
			}
			h := Of(synth(past, 30*time.Minute, c.gaps...), fqn, nowT)
			if ok, why := h.Gate(); !ok {
				t.Logf("%-12s %-34s gated out (%s)", model, c.name, why)
				if c.kind == None {
					right++
					noiseN++
				}
				continue
			}
			p, err := s.Ask(context.Background(), h, nowT)
			if err != nil {
				t.Errorf("%s %s: %v", model, c.name, err)
				continue
			}
			ok := p.Pattern == (c.kind != None)
			hit := ""
			if c.kind == None {
				noiseN++
				if p.Claimed {
					noiseClaimed++
				}
				if p.Pattern {
					noisePassed++
				}
			} else {
				patterned++
				if p.Pattern && p.Kind == c.kind {
					kinds++
				}
				if p.Pattern && !held.IsZero() && !held.Before(p.Next.From.Add(-slotSlack)) && !held.After(p.Next.To.Add(slotSlack)) {
					hits++
					hit = "hit"
				} else if p.Pattern {
					hit = fmt.Sprintf("miss (held %s, window %s–%s)", held.In(zurich).Format("Mon 15:04"),
						p.Next.From.In(zurich).Format("Mon 15:04"), p.Next.To.In(zurich).Format("15:04"))
				}
			}
			if ok {
				right++
			}
			t.Logf("%-12s %-34s want %-8s got %-8s claimed %-5v %s %s %s", model, c.name, c.kind, p.Kind, p.Claimed,
				mark(ok), hit, p.Dropped)
		}
		spent += s.Spent()
		t.Logf("%s: right %d/%d · kind %d/%d · window hit %d/%d · noise: claimed %d, passed %d of %d · $%.4f",
			model, right, len(evalCases()), kinds, patterned, hits, patterned, noiseClaimed, noisePassed, noiseN, s.Spent())
		// The user's rule: no invented pattern reaches the page.
		if noisePassed > 0 {
			t.Errorf("%s: %d invented patterns passed the checks", model, noisePassed)
		}
	}
	t.Logf("total $%.4f", spent)
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// loadEnv sets KEY=value lines from path; a variable already set wins. Values are never logged.
func loadEnv(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the eval's own key file
	if err != nil {
		t.Fatalf("BACKEND_EVAL_ENV: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			t.Setenv(k, strings.Trim(v, `"'`))
		}
	}
}
