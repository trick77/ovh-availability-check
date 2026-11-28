package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/watch"
)

// The JSON files of releases before 0.4, imported once into an empty database and then renamed *.imported. Drop
// this file once every deployment has run it.
const (
	legacyState       = "state.json"
	legacyPrices      = "prices.json"
	legacyPredictions = "predictions.json"
	legacyHello       = "hello.json"
)

type oldSpan struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Gone  time.Time `json:"gone"`
	Slow  bool      `json:"slow"`
	Quiet bool      `json:"quiet"`
}

type oldState struct {
	Notified []string  `json:"notified"`
	Since    time.Time `json:"since"`
	PolledAt time.Time `json:"polled_at"`
	Seen     map[string]struct {
		Since time.Time `json:"since"`
		Last  time.Time `json:"last"`
		In    bool      `json:"in"`
	} `json:"seen"`
	Polled []oldSpan            `json:"polled"`
	Spans  map[string][]oldSpan `json:"spans"`
	Pushes []struct {
		At    time.Time `json:"at"`
		Title string    `json:"title"`
		Body  string    `json:"body"`
		Err   string    `json:"err"`
	} `json:"pushes"`
	DigestAt time.Time `json:"digest_at"`
}

type oldPrice struct {
	At    time.Time `json:"at"`
	Rent  float64   `json:"rent"`
	Setup float64   `json:"setup"`
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's BACKEND_DATA_DIR
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func stamp(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Second)
}

// importLegacy fills an empty database (no meta row) from dir's JSON files, one transaction, then renames them.
// A failed rename is harmless: the meta row exists, nothing is imported twice.
func (s *Store) importLegacy(ctx context.Context, dir string) error {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta`).Scan(&n); err != nil || n > 0 {
		return err
	}
	var old oldState
	ok, err := readJSON(filepath.Join(dir, legacyState), &old)
	if err != nil || !ok {
		return err
	}
	st := watch.State{Notified: old.Notified, Since: stamp(old.Since), PolledAt: stamp(old.PolledAt),
		DigestAt: stamp(old.DigestAt), Seen: map[string]watch.Seen{}, Spans: map[string]map[string][]watch.Span{},
		Prices: map[string][]watch.Price{}}
	conv := func(o oldSpan) watch.Span {
		return watch.Span{From: stamp(o.From), To: stamp(o.To), Gone: stamp(o.Gone), Slow: o.Slow, Quiet: o.Quiet}
	}
	for k, e := range old.Seen {
		st.Seen[k] = watch.Seen{Since: stamp(e.Since), Last: stamp(e.Last), In: e.In}
	}
	for _, p := range old.Polled {
		st.Polled = append(st.Polled, conv(p))
	}
	for k, sp := range old.Spans {
		fqn, dc := watch.SplitKey(k)
		if st.Spans[fqn] == nil {
			st.Spans[fqn] = map[string][]watch.Span{}
		}
		for _, x := range sp {
			st.Spans[fqn][dc] = append(st.Spans[fqn][dc], conv(x))
		}
	}
	// Oldest first, so AddEvent's newest-first order and its same-second rule hold.
	for i := len(old.Pushes) - 1; i >= 0; i-- {
		p := old.Pushes[i]
		e := watch.Event{At: p.At, Kind: watch.KindRestock, Title: p.Title, Body: p.Body, Delivery: watch.Sent}
		if strings.HasPrefix(p.Title, "OVH week") {
			e.Kind = watch.KindDigest
		}
		if p.Err != "" {
			e.Delivery = p.Err
		}
		st = st.AddEvent(e)
	}
	var prices map[string][]oldPrice
	if _, err := readJSON(filepath.Join(dir, legacyPrices), &prices); err != nil {
		return err
	}
	for k, ps := range prices {
		for _, p := range ps {
			st.Prices[k] = append(st.Prices[k], watch.Price{At: stamp(p.At), Rent: p.Rent, Setup: p.Setup})
		}
	}
	var preds map[string]predict.Prediction
	if _, err := readJSON(filepath.Join(dir, legacyPredictions), &preds); err != nil {
		return err
	}
	var took map[string]time.Time
	if _, err := readJSON(filepath.Join(dir, legacyHello), &took); err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := save(ctx, tx, watch.State{}, st); err != nil {
		return err
	}
	if err := savePredictions(ctx, tx, preds, nil); err != nil {
		return err
	}
	if err := saveHello(ctx, tx, took); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var spans int
	for _, m := range st.Spans {
		for _, sp := range m {
			spans += len(sp)
		}
	}
	slog.Info("imported the JSON state", "spans", spans, "prices", len(st.Prices), "events", len(st.Events),
		"predictions", len(preds))
	for _, f := range []string{legacyState, legacyPrices, legacyPredictions, legacyHello} {
		p := filepath.Join(dir, f)
		if err := os.Rename(p, p+".imported"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("import: not renamed", "file", f, "err", err)
		}
	}
	return nil
}
