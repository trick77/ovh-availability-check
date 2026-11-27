// Command ovh-availability-check polls OVHcloud's dedicated server stock, pushes when a wanted variant comes into
// stock and serves a read-only status page.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/version"
	"github.com/trick77/toolkit/llmwire"
	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/predict"
	"github.com/trick77/ovh-availability-check/internal/watch"

	// The page's times are in TZ; distroless has no zoneinfo.
	_ "time/tzdata"
)

// catalogAge: the catalogs are 35 MB together and prices rarely move.
const catalogAge = 6 * time.Hour

func main() {
	notifyTest := flag.Bool("notify-test", false, "send a test push to every BACKEND_NOTIFY_URLS channel and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the running server's /healthz and exit (container healthcheck)")
	wish := flag.String("wish", "", "turn a wish in plain words into a want block for config.json (needs BACKEND_LLM_MODEL) and exit")
	digestNow := flag.Bool("digest-now", false, "send the weekly digest on the first poll instead of Sunday 18:00")
	flag.Parse()
	addr := cmp.Or(os.Getenv("BACKEND_ADDR"), ":8080")
	if *healthcheck {
		os.Exit(httpx.Healthcheck(httpx.LocalURL(addr, "/healthz")))
	}
	ctx, stop := run.Context(context.Background())
	var err error
	if *wish != "" {
		err = wishCmd(ctx, *wish)
	} else {
		err = serve(ctx, addr, *notifyTest, *digestNow)
	}
	stop()
	if err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

// apiBase is the OVH API; the tests point it at a fake.
var apiBase = ovh.API

// llmFromEnv is the optional model: BACKEND_LLM_MODEL plus llmwire's LLMWIRE_<PROVIDER>_API_KEY. llmCfg is what
// the tests swap for a fake.
var llmCfg = llmwire.Config{}

func llmFromEnv(dataDir string) (*predict.Service, error) {
	model := os.Getenv("BACKEND_LLM_MODEL")
	client, err := predict.FromEnv(model, llmCfg)
	if err != nil {
		return nil, fmt.Errorf("BACKEND_LLM_MODEL %s: %w", model, err)
	}
	daily := 100
	if s := os.Getenv("BACKEND_LLM_DAILY_CALLS"); s != "" {
		if daily, err = strconv.Atoi(s); err != nil || daily < 0 {
			return nil, fmt.Errorf("BACKEND_LLM_DAILY_CALLS %q", s)
		}
	}
	if client == nil {
		slog.Info("no LLM: predictions, digest and -wish are off", "model", model)
	}
	path := ""
	if dataDir != "" {
		path = filepath.Join(dataDir, "predictions.json")
	}
	return predict.New(predict.Options{Client: client, Model: model, DailyCalls: daily, Path: path, Loc: time.Local})
}

// wishCmd prints the want block for a wish in plain words.
func wishCmd(ctx context.Context, text string) error {
	confDir := cmp.Or(os.Getenv("BACKEND_CONF_DIR"), "conf")
	ref := ""
	// The reference CPU, for cpu_min_pct. No config is fine; a broken one is named, not silently dropped.
	cfg, err := watch.LoadConfig(filepath.Join(confDir, "config.json"))
	switch {
	case err == nil:
		ref = cfg.ReferenceCPU
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	svc, err := llmFromEnv("") // no predictions file: -wish keeps nothing
	if err != nil {
		return err
	}
	if !svc.Configured() {
		return errors.New("-wish needs BACKEND_LLM_MODEL and its LLMWIRE_<PROVIDER>_API_KEY")
	}
	want, unclear, err := svc.Wish(ctx, text, ref)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(want, "", "  ")
	fmt.Println(string(b))
	for _, u := range unclear {
		fmt.Fprintln(os.Stderr, "not a criterion here:", u)
	}
	return nil
}

func serve(ctx context.Context, addr string, notifyTest, digestNow bool) error {
	dataDir := cmp.Or(os.Getenv("BACKEND_DATA_DIR"), "data")
	confDir := cmp.Or(os.Getenv("BACKEND_CONF_DIR"), "conf")

	urls, off, err := notifyURLs(os.Getenv("BACKEND_NOTIFY_URLS"))
	if err != nil {
		return fmt.Errorf("BACKEND_NOTIFY_URLS: %w", err)
	}
	var channels *notify.Channels
	if len(urls) > 0 {
		if channels, err = notify.NewChannels(urls); err != nil {
			return fmt.Errorf("BACKEND_NOTIFY_URLS: %w", err)
		}
	}
	noChannel := "BACKEND_NOTIFY_URLS is empty"
	if len(off) > 0 {
		noChannel = "every channel in BACKEND_NOTIFY_URLS is switched off"
	}
	if notifyTest {
		if channels == nil {
			return errors.New(noChannel)
		}
		if err := channels.Send("OVH availability check", "Test push, sent by -notify-test."); err != nil {
			return err
		}
		fmt.Println("sent to", strings.Join(channels.Labels(), ", "))
		return nil
	}

	cfg, err := watch.LoadConfig(filepath.Join(confDir, "config.json"))
	if err != nil {
		return err
	}
	statePath := filepath.Join(dataDir, "state.json")
	state, err := watch.LoadState(statePath)
	if err != nil {
		return err
	}
	slog.Info("config", "version", version.String(), "datacenters", cfg.Datacenters, "interval", cfg.Interval.String(), "wants", len(cfg.Want),
		"channels", channels.Labels(), "off", off)
	if channels == nil {
		slog.Warn(noChannel + ": nothing is pushed, and nothing counts as pushed")
	}

	svc, err := llmFromEnv(dataDir)
	if err != nil {
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	client := ovh.NewClient()
	client.Base = apiBase
	w := &watcher{cfg: cfg, client: client, channels: channels, off: off, state: state, statePath: statePath, predict: svc,
		digestNow: digestNow}
	var wg sync.WaitGroup
	if svc.Configured() {
		wg.Go(func() { svc.Run(ctx) })
	}
	wg.Go(func() {
		for {
			w.poll(ctx)
			if !run.Sleep(ctx, time.Duration(cfg.Interval)) {
				return
			}
		}
	})
	slog.Info("listening", "addr", addr)
	// Serve returns at once when the address can't be bound; stop ends the poll loop then too.
	err = httpx.Serve(ctx, httpx.NewServer(addr, w.routes()), 5*time.Second)
	stop()
	wg.Wait()
	return err
}

type watcher struct {
	cfg       watch.Config
	client    *ovh.Client
	channels  *notify.Channels
	off       []string // channels switched off with "!", by scheme, for the page
	statePath string

	catalog     *ovh.Catalog
	catalogAt   time.Time
	pricesDirty bool // prices.json lags the state
	predict     *predict.Service
	digestNow   bool // -digest-now: send the digest on the first poll with a catalog

	mu       sync.Mutex
	state    watch.State
	variants []watch.Variant
	polledAt time.Time
	pollErr  string
	catErr   string
}

func (w *watcher) poll(ctx context.Context) {
	if w.catalog == nil || time.Since(w.catalogAt) > catalogAge {
		cat, err := w.client.Catalog(ctx)
		w.mu.Lock()
		if err != nil {
			w.catErr = err.Error()
			slog.Warn("catalog", "err", err)
		} else {
			w.catalog, w.catalogAt, w.catErr = cat, time.Now(), ""
		}
		w.mu.Unlock()
	}

	av, err := w.client.Availabilities(ctx)
	if err != nil {
		// A failed poll changes nothing: forgetting the state would push everything again on the next good one.
		slog.Warn("availability", "err", err)
		w.mu.Lock()
		w.pollErr = err.Error()
		w.mu.Unlock()
		return
	}
	vs := watch.Build(av, w.catalog, w.cfg.Datacenters, w.cfg.ReferenceCPU)

	w.mu.Lock()
	w.variants, w.polledAt, w.pollErr = vs, time.Now(), ""
	state := w.state
	w.mu.Unlock()

	// Without a catalog every price and CPU wish fails: deciding now would drop their pairs from the state and push
	// them again once the catalog is there, and the history would log them as wanted only from then on.
	if w.catalog == nil {
		return
	}
	now := time.Now()
	// In scope: wanted or just above a wish. Their history is what the page draws and the prediction reads.
	state = state.Observe(vs, w.cfg.InScope, now, time.Duration(w.cfg.Interval))
	state, moved := state.ObservePrices(vs, now)
	// A failed write is retried every poll: the next change may be months away.
	if moved || w.pricesDirty {
		w.pricesDirty = true
		if err := state.SavePrices(w.statePath); err != nil {
			slog.Error("prices", "err", err)
		} else {
			w.pricesDirty = false
		}
	}
	cur := watch.Current(w.cfg, vs)
	fresh := state.New(cur)
	sent := false
	if len(fresh) > 0 && w.channels != nil {
		title, body := watch.Message(vs, fresh)
		state, sent = w.send(state, now, title, body, notify.High)
	}
	next := state.Next(cur, fresh, sent)
	// The weekly digest, Sunday 18:00. The state is saved first, this poll's pushed pairs and the digest marked
	// sent: a crash while the model words it loses this week's digest rather than sending a stock push or the
	// digest twice. Worded in the poll: one call a week, up to 2 minutes, within the gap slack.
	if w.predict.Configured() && w.channels != nil &&
		(w.digestNow || predict.DigestDue(now, latest(next.DigestAt, next.Since), time.Local)) {
		w.digestNow = false
		next.DigestAt = now
		if err := next.Save(w.statePath); err != nil {
			slog.Error("state", "err", err)
		}
		if d := predict.BuildDigest(next, vs, w.cfg, w.predict.All(), now, time.Local); !d.Empty() {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			title, body := w.predict.Words(dctx, d)
			cancel()
			next, _ = w.send(next, now, title, body, notify.Normal)
		}
	}
	// Kept in memory even when the save fails: the page still shows the push, and it isn't sent again before a
	// restart.
	w.mu.Lock()
	w.state = next
	w.mu.Unlock()
	if err := next.Save(w.statePath); err != nil {
		slog.Error("state", "err", err)
		return
	}
	// Predictions: windows rolled forward, and the model asked about variants whose stock changed.
	var scope []string
	for _, v := range vs {
		if w.cfg.InScope(v) {
			scope = append(scope, v.FQN)
		}
	}
	w.predict.Update(next, scope, now)
	slog.Info("polled", "variants", len(vs), "wanted_in_stock", len(cur), "new", len(fresh))
}

// notifyURLs splits BACKEND_NOTIFY_URLS into the channels to use and the ones switched off with a leading "!",
// named by their scheme only: the rest of a URL may hold a password or an ntfy topic, and notify.Scheme never
// returns more than the scheme, even for a typo without "://". URLs are whitespace separated: a shoutrrr URL may
// hold a comma. A lone "!" ("! smtp://…") is refused: it would leave the channel on that the user meant to switch off.
func notifyURLs(s string) (on, off []string, err error) {
	for _, u := range strings.Fields(s) {
		if rest, ok := strings.CutPrefix(u, "!"); ok {
			if rest == "" {
				return nil, nil, errors.New(`a lone "!": write it right before the URL, as in !smtp://…`)
			}
			off = append(off, notify.Scheme(rest))
			continue
		}
		on = append(on, u)
	}
	return on, off, nil
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// send pushes one message and keeps it in the state's list; sent is false only when no channel took it.
func (w *watcher) send(state watch.State, now time.Time, title, body string, sev notify.Severity) (watch.State, bool) {
	p := watch.Push{At: now, Title: title, Body: body}
	err := w.channels.SendMessage(notify.Message{Title: title, Body: body, Severity: sev})
	var se *notify.SendError
	// Partial: another channel took it. Sending again would repeat it there.
	sent := err == nil || (errors.As(err, &se) && se.Partial)
	if err != nil {
		// The page has no login: it names the failed channels only. The error text may quote the URL (an ntfy
		// topic is its secret); it goes to the log.
		p.Err = "failed"
		if se != nil && len(se.Failed) > 0 {
			p.Err = "failed on " + strings.Join(se.Failed, ", ")
			if se.Partial {
				p.Err = "partly " + p.Err
			}
		}
		slog.Warn("push", "err", err)
	} else {
		slog.Info("pushed", "title", title)
	}
	return state.AddPush(p), sent
}
