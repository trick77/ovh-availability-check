// Command ovh-availability-check polls OVHcloud's dedicated server stock, pushes when a wanted variant comes into
// stock and serves a read-only status page.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/version"
	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/watch"

	// The page's times are in TZ; distroless has no zoneinfo.
	_ "time/tzdata"
)

// catalogAge: the catalogs are 35 MB together and prices rarely move.
const catalogAge = 6 * time.Hour

func main() {
	notifyTest := flag.Bool("notify-test", false, "send a test push to every BACKEND_NOTIFY_URLS channel and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the running server's /healthz and exit (container healthcheck)")
	flag.Parse()
	addr := cmp.Or(os.Getenv("BACKEND_ADDR"), ":8080")
	if *healthcheck {
		os.Exit(httpx.Healthcheck(httpx.LocalURL(addr, "/healthz")))
	}
	ctx, stop := run.Context(context.Background())
	err := serve(ctx, addr, *notifyTest)
	stop()
	if err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

// apiBase is the OVH API; the tests point it at a fake.
var apiBase = ovh.API

func serve(ctx context.Context, addr string, notifyTest bool) error {
	dataDir := cmp.Or(os.Getenv("BACKEND_DATA_DIR"), "data")
	confDir := cmp.Or(os.Getenv("BACKEND_CONF_DIR"), "conf")

	// URLs are whitespace separated: a shoutrrr URL may hold a comma.
	var channels *notify.Channels
	if urls := strings.Fields(os.Getenv("BACKEND_NOTIFY_URLS")); len(urls) > 0 {
		var err error
		if channels, err = notify.NewChannels(urls); err != nil {
			return fmt.Errorf("BACKEND_NOTIFY_URLS: %w", err)
		}
	}
	if notifyTest {
		if channels == nil {
			return errors.New("BACKEND_NOTIFY_URLS is empty")
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
		"channels", channels.Labels())
	if channels == nil {
		slog.Warn("BACKEND_NOTIFY_URLS is empty: nothing is pushed, and nothing counts as pushed")
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	client := ovh.NewClient()
	client.Base = apiBase
	w := &watcher{cfg: cfg, client: client, channels: channels, state: state, statePath: statePath}
	var wg sync.WaitGroup
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
	statePath string

	catalog     *ovh.Catalog
	catalogAt   time.Time
	pricesDirty bool // prices.json lags the state

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
	vs := watch.Build(av, w.catalog, w.cfg.Datacenters)

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
	state = state.Observe(vs, w.cfg.Wanted, now, time.Duration(w.cfg.Interval))
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
		p := watch.Push{At: now, Title: title, Body: body}
		err := w.channels.SendMessage(notify.Message{Title: title, Body: body, Severity: notify.High})
		var se *notify.SendError
		// Partial: another channel took it. Sending again would repeat it there.
		sent = err == nil || (errors.As(err, &se) && se.Partial)
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
		state = state.AddPush(p)
	}
	next := state.Next(cur, fresh, sent)
	// Kept in memory even when the save fails: the page still shows the push, and it isn't sent again before a
	// restart.
	w.mu.Lock()
	w.state = next
	w.mu.Unlock()
	if err := next.Save(w.statePath); err != nil {
		slog.Error("state", "err", err)
		return
	}
	slog.Info("polled", "variants", len(vs), "wanted_in_stock", len(cur), "new", len(fresh))
}
