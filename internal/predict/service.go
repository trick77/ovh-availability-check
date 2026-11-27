package predict

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/trick77/toolkit/llmwire"

	"github.com/trick77/ovh-restock/internal/watch"
)

// askEvery is the least time between two calls about one variant: a flapping stock batches into the next call.
const askEvery = 6 * time.Hour

// Options configure a Service.
type Options struct {
	Client     *llmwire.Client // nil: no model, nothing is asked
	Model      string
	DailyCalls int    // per local day, all kinds of call together
	Path       string // predictions.json
	Loc        *time.Location
	Log        *slog.Logger
}

// Service keeps the predictions and asks the model in the background.
type Service struct {
	o    Options
	jobs chan History

	mu     sync.Mutex
	preds  map[string]Prediction
	queued map[string]bool
	tried  map[string]try // last call per variant, failed ones too: askEvery and the change rule hold for both
	day    string
	calls  int
	broke  bool  // the day's budget ran out (logged once)
	spent  int64 // nano-USD at list price, since start

	saveMu sync.Mutex // marshal and rename as one: two savers must not land an older file last
}

// Spent is what the calls cost so far, in USD at the vendor's list price.
func (s *Service) Spent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return float64(s.spent) / 1e9
}

// FromEnv builds the client for model with llmwire's own configuration (LLMWIRE_<PROVIDER>_API_KEY). No model or a
// missing key is "no LLM", not an error: everything else works without one.
func FromEnv(model string, cfg llmwire.Config) (*llmwire.Client, error) {
	if model == "" {
		return nil, nil
	}
	c, err := llmwire.FromEnv(model, cfg)
	var missing *llmwire.MissingEnvError
	if errors.As(err, &missing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Measured capabilities, checked at boot: a model that can't hold a JSON schema would fail every call.
	if _, err := c.Registry().Require(model, llmwire.Needs{JSONSchema: true}); err != nil {
		return nil, err
	}
	return c, nil
}

// New loads o.Path and returns the service; Run asks the model.
func New(o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Loc == nil {
		o.Loc = time.Local
	}
	s := &Service{o: o, jobs: make(chan History, 64), preds: map[string]Prediction{}, queued: map[string]bool{},
		tried: map[string]try{}}
	b, err := os.ReadFile(o.Path)
	switch {
	case o.Path == "": // nothing kept (-wish)
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.preds); err != nil {
			return nil, fmt.Errorf("%s: %w", o.Path, err)
		}
	}
	if o.Client != nil {
		if _, err := o.Client.Validate(s.request("x", "x", schema, 100)); err != nil {
			return nil, fmt.Errorf("model %s: %w", o.Model, err)
		}
	}
	return s, nil
}

// Configured tells whether a model is set.
func (s *Service) Configured() bool { return s != nil && s.o.Client != nil }

// Get is fqn's prediction.
func (s *Service) Get(fqn string) (Prediction, bool) {
	if s == nil {
		return Prediction{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.preds[fqn]
	return p, ok
}

// All are every prediction with a pattern, by fqn.
func (s *Service) All() map[string]Prediction {
	out := map[string]Prediction{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.preds {
		out[k] = p
	}
	return out
}

// Update runs after each good poll on the in-scope variants: it rolls the windows forward, drops what fell out of
// scope or under the gate, and queues a call for a variant whose stock changed since it was last asked about.
func (s *Service) Update(st watch.State, scope []string, now time.Time) {
	if s == nil {
		return
	}
	in := map[string]bool{}
	var ask []History
	s.mu.Lock()
	changed := false
	for _, fqn := range scope {
		in[fqn] = true
		h := Of(st, fqn, now)
		if ok, _ := h.Gate(); !ok {
			if _, had := s.preds[fqn]; had {
				delete(s.preds, fqn)
				changed = true
			}
			continue
		}
		p, have := s.preds[fqn]
		if have {
			before := p
			p.roll(h, now, s.o.Loc)
			if p.Next != before.Next {
				s.preds[fqn] = p
				changed = true
			}
		}
		// Only on a change (user): a departure newer than the one the last call saw, failed calls included (else a
		// model that keeps failing is asked every 6 h without any change), and at most every askEvery.
		tr, triedOK := s.tried[fqn]
		basis, seen := p.Basis, have
		if !have && triedOK {
			basis, seen = tr.basis, true
		}
		if s.o.Client != nil && !s.queued[fqn] && (!seen || h.LastGone.After(basis)) &&
			now.Sub(p.Asked) >= askEvery && now.Sub(tr.at) >= askEvery {
			ask = append(ask, h)
		}
	}
	for fqn := range s.preds {
		if !in[fqn] {
			delete(s.preds, fqn)
			changed = true
		}
	}
	for fqn := range s.tried {
		if !in[fqn] {
			delete(s.tried, fqn)
		}
	}
	for _, h := range ask {
		select {
		case s.jobs <- h:
			s.queued[h.FQN] = true
		default: // full: asked on the next change
		}
	}
	s.mu.Unlock()
	if changed {
		s.save()
	}
}

// Run asks the model, one variant at a time, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case h := <-s.jobs:
			now := time.Now()
			p, err := s.Ask(ctx, h, now)
			s.mu.Lock()
			delete(s.queued, h.FQN)
			s.tried[h.FQN] = try{at: now, basis: h.LastGone}
			if err == nil {
				s.preds[h.FQN] = carry(s.preds[h.FQN], p, h, now, s.o.Loc)
			}
			s.mu.Unlock()
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, errBudget) {
					s.o.Log.Warn("prediction", "fqn", h.FQN, "err", err)
				}
				continue
			}
			s.o.Log.Info("prediction", "fqn", h.FQN, "pattern", p.Pattern, "kind", p.Kind, "dropped", p.Dropped)
			s.save()
		}
	}
}

// Ask asks the model about h and judges the answer.
func (s *Service) Ask(ctx context.Context, h History, now time.Time) (Prediction, error) {
	obj, err := s.call(ctx, "pattern", schema, patternPrompt, input(h, now, s.o.Loc), 1500)
	if err != nil {
		return Prediction{}, err
	}
	var a answer
	if err := json.Unmarshal([]byte(obj), &a); err != nil {
		return Prediction{}, fmt.Errorf("answer: %w", err)
	}
	p := judge(a, h, s.o.Loc)
	p.Asked, p.Basis, p.Model = now, h.LastGone, s.o.Model
	p.roll(h, now, s.o.Loc)
	return p, nil
}

// try is one call about a variant: when, and the newest departure it saw.
type try struct {
	at, basis time.Time
}

// carry keeps the hit record when a new answer states the same rule: every restock asks again, and the rate must
// build up over many of them. The old prediction is settled first: windows that ended since the last poll are
// counted, and an open window an arrival already fell in counts as a hit (the new answer moves Next past it).
// A different rule starts afresh.
func carry(old, p Prediction, h History, now time.Time, loc *time.Location) Prediction {
	if !old.Pattern || !p.Pattern || old.Kind != p.Kind || !slices.Equal(old.Slots, p.Slots) ||
		(p.Kind == Interval && math.Abs(float64(old.Every-p.Every)) > 0.1*float64(old.Every)) {
		return p
	}
	old.roll(h, now, loc)
	if w := old.Next; !w.From.IsZero() && !w.From.After(now) && slices.ContainsFunc(h.Ins, w.caught) {
		old.Hits++
		old.Tries++
	}
	p.Hits, p.Tries = old.Hits, old.Tries
	return p
}

var errBudget = errors.New("daily model budget spent")

// call sends one request after the budget check and returns the reply's JSON object.
func (s *Service) call(ctx context.Context, name string, sch map[string]any, system string, data any,
	answerTokens int) (string, error) {
	if s.o.Client == nil {
		return "", errors.New("no model configured")
	}
	if !s.spend() {
		return "", errBudget
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	resp, _, err := s.o.Client.Chat(cctx, s.request(name, system+"\n\n"+guard, sch, answerTokens, string(payload)))
	if resp != nil {
		s.mu.Lock()
		s.spent += resp.Usage.Cost.NanoUSD
		s.mu.Unlock()
	}
	if err != nil {
		return "", err
	}
	obj, ok := llmwire.JSONObject(resp.Content)
	if !ok {
		return "", fmt.Errorf("reply is no JSON object (%d chars)", len(resp.Content))
	}
	return obj, nil
}

// request is one call: system prompt, the data as JSON in the user message, the strict schema. Balanced, not
// minimal: on the flash models thinking off answers wrong (llmwire's profile notes); GLM can't switch it off.
func (s *Service) request(name, system string, sch map[string]any, answerTokens int, data ...string) llmwire.ChatRequest {
	user := "DATA (JSON):\n"
	if len(data) > 0 {
		user += data[0]
	}
	return llmwire.ChatRequest{
		Model:           s.o.Model,
		Messages:        []llmwire.Message{llmwire.System(system), llmwire.User(user)},
		Reasoning:       llmwire.ReasoningBalanced(),
		MaxAnswerTokens: &answerTokens,
		BestEffort:      true,
		ResponseFormat:  llmwire.ResponseFormat{Kind: llmwire.FormatJSONSchema, Name: name, Schema: sch, Strict: true},
	}
}

// spend books one call against the day's budget; false when it is spent.
func (s *Service) spend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().In(s.o.Loc).Format(time.DateOnly)
	if day != s.day {
		s.day, s.calls, s.broke = day, 0, false
	}
	// 0 is no calls at all: a cap, never "unlimited".
	if s.calls >= s.o.DailyCalls {
		if !s.broke {
			s.broke = true
			s.o.Log.Warn("model: daily call budget spent", "calls", s.o.DailyCalls)
		}
		return false
	}
	s.calls++
	return true
}

func (s *Service) save() {
	if s.o.Path == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	b, err := json.Marshal(s.preds)
	s.mu.Unlock()
	if err == nil {
		err = writeFile(s.o.Path, b)
	}
	if err != nil {
		s.o.Log.Error("predictions", "err", err)
	}
}

// writeFile writes atomically, as state.json is.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".predictions-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
