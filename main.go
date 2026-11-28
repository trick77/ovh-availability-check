// Command ovh-restock polls OVHcloud's dedicated server stock, pushes when a wanted variant comes into
// stock and serves a read-only status page.
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/version"
	"github.com/trick77/toolkit/llmwire"
	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/store"
	"github.com/trick77/ovh-restock/internal/watch"

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

func llmFromEnv(ctx context.Context, st predict.Store) (*predict.Service, error) {
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
	return predict.New(ctx, predict.Options{Client: client, Model: model, DailyCalls: daily, Store: st, Loc: time.Local})
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
	svc, err := llmFromEnv(ctx, nil) // no store: -wish keeps nothing
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
		if err := channels.Send("OVH restock", "Test push, sent by -notify-test."); err != nil {
			return err
		}
		fmt.Println("sent to", strings.Join(channels.Labels(), ", "))
		return nil
	}

	cfg, err := watch.LoadConfig(filepath.Join(confDir, "config.json"))
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	state, err := db.Load(ctx)
	if err != nil {
		return err
	}
	slog.Info("config", "version", version.String(), "datacenters", cfg.Datacenters, "interval", cfg.Interval.String(), "wants", len(cfg.Want),
		"channels", channels.Labels(), "off", off)
	if channels == nil {
		slog.Warn(noChannel + ": restocks are kept as events on the page, nothing is pushed")
	}

	svc, err := llmFromEnv(ctx, db)
	if err != nil {
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	client := ovh.NewClient()
	client.Base = apiBase
	w := &watcher{cfg: cfg, client: client, channels: channels, off: off, state: state, saved: state, db: db,
		predict: svc, digestNow: digestNow}
	var wg sync.WaitGroup
	if channels != nil {
		// Beside the poll: a channel that hangs holds no restock push back.
		wg.Go(func() { hello(ctx, urls, db, time.Now()) })
	}
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
	cfg      watch.Config
	client   *ovh.Client
	channels *notify.Channels
	off      []string // channels switched off with "!", by scheme, for the page
	db       *store.Store
	// saved is what the database holds: Save writes the diff to it. A failed save leaves it, so the next one carries
	// the change too.
	saved watch.State

	catalog      *ovh.Catalog
	catalogAt    time.Time
	predict      *predict.Service
	digestNow    bool // -digest-now: make the digest on the first poll with a catalog
	plansChecked bool // wishes' unknown plans logged, after the first catalog

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
	if !w.plansChecked {
		w.plansChecked = true
		for i, want := range w.cfg.Want {
			if u := want.UnknownPlans(vs); len(u) > 0 {
				slog.Warn("wish names a product no watched variant has", "wish", want.Label(i), "plans", u)
			}
		}
	}
	now := time.Now()
	// Every pair, wanted or not: a wish added later finds its history there.
	state = state.Observe(vs, now, time.Duration(w.cfg.Interval))
	state = state.ObservePrices(vs, now)
	cur := watch.Current(w.cfg, vs)
	fresh := state.New(cur)
	// Restocks no channel took are sent again while their pairs are in stock without a break: still evented
	// (Notified), so a pair that sold out and came back goes out once, as fresh. Retries and fresh pairs share one
	// push: a hanging channel costs one timeout per poll, not one per pending event.
	var retried []watch.Event
	pairs := slices.Clone(fresh)
	for _, e := range state.Events {
		if !e.Retry {
			continue
		}
		n := len(pairs)
		for _, p := range e.Pairs() {
			k := watch.Key(p.FQN, p.DC)
			if _, ok := cur[k]; ok && slices.Contains(state.Notified, k) && !slices.Contains(pairs, p) {
				pairs = append(pairs, p)
			}
		}
		if len(pairs) == n || w.channels == nil {
			state = state.SetDelivery(e, e.Delivery, false) // nothing left to send
			continue
		}
		retried = append(retried, e)
	}
	if len(pairs) > 0 {
		title, body := watch.Message(vs, pairs)
		delivery, retry := w.deliver(title, body, notify.High)
		for _, e := range retried {
			state = state.SetDelivery(e, delivery, retry)
		}
		if len(fresh) > 0 {
			title, body := watch.Message(vs, fresh)
			state = state.AddEvent(watch.Event{At: now, Kind: watch.KindRestock, Title: title, Body: body,
				Delivery: delivery, Retry: retry, Items: watch.Items(vs, fresh)})
		}
	}
	next := state.Next(cur, fresh)
	// The weekly digest, Sunday 18:00, kept as an event with or without a channel. The state is saved first, this
	// poll's restock and the digest marked made: a crash while the model words it loses this week's digest rather
	// than sending a restock or the digest twice. Worded in the poll: one call a week, up to 2 minutes, within the
	// gap slack.
	if w.predict.Configured() && (w.digestNow || predict.DigestDue(now, latest(next.DigestAt, next.Since), time.Local)) {
		w.digestNow = false
		next.DigestAt = now
		w.save(ctx, next)
		if d := predict.BuildDigest(next, vs, w.cfg, w.predict.All(), now, time.Local); !d.Empty() {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			title, body := w.predict.Words(dctx, d)
			cancel()
			delivery, _ := w.deliver(title, body, notify.Normal)
			next = next.AddEvent(watch.Event{At: now, Kind: watch.KindDigest, Title: title, Body: body, Delivery: delivery})
		}
	}
	// Kept in memory even when the save fails: the page still shows the event, and it isn't sent again before a
	// restart.
	w.mu.Lock()
	w.state = next
	w.mu.Unlock()
	if !w.save(ctx, next) {
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

// save writes next's diff against what the database holds. A shutdown mid-poll still saves: the poll's events are
// in next.
func (w *watcher) save(ctx context.Context, next watch.State) bool {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := w.db.Save(sctx, w.saved, next); err != nil {
		slog.Error("state", "err", err)
		return false
	}
	w.saved = next
	return true
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

// helloQuiet: a channel that took a start's test push this recently gets none from the next start (a restart loop,
// an update right after a deploy). A failed one never quiets the next start: that start is the one checking the fix.
const helloQuiet = 10 * time.Minute

// helloMarks keep when each channel last took a start's test push, by a hash of its URL (the URL holds secrets). A
// channel added or changed since has no entry and is tested at once.
type helloMarks interface {
	Hello(ctx context.Context) (map[string]time.Time, error)
	SaveHello(ctx context.Context, took map[string]time.Time) error
}

func urlKey(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:8])
}

// hello sends the start's test push to the channels not switched off with "!" (urls) that took none within
// helloQuiet: a dead channel shows now, not at the first restock. Not kept in the push list: the page shows
// restocks. A shutdown cuts the send; what was cut counts as not taken.
func hello(ctx context.Context, urls []string, marks helloMarks, now time.Time) {
	took, err := marks.Hello(ctx)
	if err != nil {
		slog.Warn("start's test push: marks unreadable, every channel is tested", "err", err)
		took = map[string]time.Time{}
	}
	var due, labels []string
	for i, u := range urls {
		label := fmt.Sprintf("channel %d (%s)", i+1, notify.Scheme(u))
		if since := now.Sub(took[urlKey(u)]); since >= 0 && since < helloQuiet {
			slog.Info("start's test push skipped: the channel took one recently", "channel", label, "ago", since.Round(time.Second))
			continue
		}
		due, labels = append(due, u), append(labels, label)
	}
	// Entries of channels no longer configured go.
	keep := map[string]time.Time{}
	for _, u := range urls {
		if t, ok := took[urlKey(u)]; ok {
			keep[urlKey(u)] = t
		}
	}
	if len(due) == 0 {
		return
	}
	channels, err := notify.NewChannels(due)
	if err != nil { // serve parsed every URL already
		slog.Warn("start's test push", "err", err)
		return
	}
	ds, _ := channels.DeliverMessage(ctx, notify.Message{Title: "OVH restock test push",
		Body: "OVH restock " + version.String() + " started. Test push, sent at every start: this channel works.", Severity: notify.Low})
	for i, d := range ds {
		if d.Err != nil {
			slog.Warn("start's test push not delivered", "channel", labels[i], "err", d.Err) // scrubbed by notify
			delete(keep, urlKey(due[i]))
			continue
		}
		keep[urlKey(due[i])] = now
		slog.Info("start's test push sent", "channel", labels[i])
	}
	if err := marks.SaveHello(context.WithoutCancel(ctx), keep); err != nil {
		slog.Warn("start's test push: marks not written", "err", err)
	}
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// deliver pushes one message to the channels: delivery as an event keeps it, retry when no channel took it.
func (w *watcher) deliver(title, body string, sev notify.Severity) (delivery string, retry bool) {
	if w.channels == nil {
		return watch.NoChannel, false
	}
	err := w.channels.SendMessage(notify.Message{Title: title, Body: body, Severity: sev})
	if err == nil {
		slog.Info("pushed", "title", title)
		return watch.Sent, false
	}
	slog.Warn("push", "err", err)
	// The page has no login: it names the failed channels only. The error text may quote the URL (an ntfy topic is
	// its secret); it goes to the log.
	var se *notify.SendError
	delivery = "failed"
	if errors.As(err, &se) && len(se.Failed) > 0 {
		delivery = "failed on " + strings.Join(se.Failed, ", ")
		if se.Partial {
			// Another channel took it: sending again would repeat it there.
			return "partly " + delivery, false
		}
	}
	return delivery, true
}
