package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/store"
	"github.com/trick77/ovh-restock/internal/watch"
)

// fakeOVH serves the fixtures, KS-2 32 GB NVMe put in stock in gra.
func fakeOVH(t *testing.T) *httptest.Server {
	t.Helper()
	return fakeOVHWith(t, func(dc string) string {
		if dc == "gra" {
			return "1H-low"
		}
		return ""
	})
}

// fakeOVHWith serves the fixtures, KS-2 32 GB NVMe's stock per DC as avail says at each request ("": the fixture's).
func fakeOVHWith(t *testing.T, avail func(dc string) string) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile("internal/ovh/testdata/availabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	var av []map[string]any
	if err := json.Unmarshal(b, &av); err != nil {
		t.Fatal(err)
	}
	dcs := map[string]map[string]any{}
	fixed := map[string]any{}
	for _, a := range av {
		if a["fqn"] == "24sk202.ram-32g-ecc-2133.softraid-2x450nvme" {
			for _, d := range a["datacenters"].([]any) {
				dc := d.(map[string]any)
				name := dc["datacenter"].(string)
				dcs[name], fixed[name] = dc, dc["availability"]
			}
		}
	}
	catalog, err := os.ReadFile("internal/ovh/testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dedicated/server/datacenter/availabilities", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		for name, dc := range dcs {
			dc["availability"] = fixed[name]
			if a := avail(name); a != "" {
				dc["availability"] = a
			}
		}
		stock, _ := json.Marshal(av)
		mu.Unlock()
		_, _ = w.Write(stock)
	})
	mux.HandleFunc("GET /order/catalog/public/eco", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(catalog) })
	mux.HandleFunc("GET /order/catalog/public/baremetalServers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plans":[],"addons":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// hook is a webhook channel that keeps every body it gets.
type hook struct {
	mu     sync.Mutex
	bodies []string
	got    chan struct{}
}

func newHook(t *testing.T) (*hook, string) {
	t.Helper()
	h := &hook{got: make(chan struct{}, 10)}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(b))
		h.mu.Unlock()
		h.got <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return h, "generic+" + srv.URL + "/hook"
}

// waitFor waits for a body holding s and returns it; the start's test push and the restock push come in either order.
func (h *hook) waitFor(t *testing.T, s string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		h.mu.Lock()
		for _, b := range h.bodies {
			if strings.Contains(b, s) {
				h.mu.Unlock()
				return b
			}
		}
		h.mu.Unlock()
		select {
		case <-h.got:
		case <-deadline:
			t.Fatalf("no push holding %q", s)
		}
	}
}

func dirs(t *testing.T, config string) (data, conf string) {
	t.Helper()
	data, conf = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BACKEND_DATA_DIR", data)
	t.Setenv("BACKEND_LLM_MODEL", "") // no model in these tests, whatever the shell has
	t.Setenv("BACKEND_CONF_DIR", conf)
	return data, conf
}

func TestServePushesOnce(t *testing.T) {
	apiBase = fakeOVH(t).URL
	h, url := newHook(t)
	t.Setenv("BACKEND_NOTIFY_URLS", url)
	data, _ := dirs(t, `{"datacenters":["gra","rbx"],"want":[{"ram_max_gb":32,"disk":"nvme","price_max_eur":20}]}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, "127.0.0.1:0", false, false) }()
	body := h.waitFor(t, "KS-2")
	h.waitFor(t, "Test push, sent at every start")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(h.bodies) != 2 {
		t.Errorf("bodies %q", h.bodies)
	}
	if !strings.Contains(body, "KS-2 · Intel Xeon-D 1540 · 32 GB · 2×450GB NVME · 18.99 €/mo") ||
		!strings.HasSuffix(body, "· gra\nhttps://eco.ovhcloud.com/fr/kimsufi/ks-2/") {
		t.Errorf("body %q", body)
	}
	s := load(t, data)
	const ks2 = "24sk202.ram-32g-ecc-2133.softraid-2x450nvme"
	if len(s.Notified) != 1 || len(s.Events) != 1 || s.Events[0].Delivery != watch.Sent ||
		len(s.Events[0].Items) != 1 || len(s.Spans[ks2]["gra"]) != 1 {
		t.Fatalf("state %+v", s)
	}

	// A restart with the same stock pushes nothing, and minutes after the last test push it sends none either.
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- serve(ctx, "127.0.0.1:0", false, false) }()
	time.Sleep(time.Second)
	cancel()
	<-done
	if len(h.bodies) != 2 {
		t.Errorf("pushed again after a restart: %q", h.bodies[2:])
	}
}

// load reads data's database.
func load(t *testing.T, data string) watch.State {
	t.Helper()
	db, err := store.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s, err := db.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServeWithoutChannel(t *testing.T) {
	apiBase = fakeOVH(t).URL
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	data, _ := dirs(t, `{"datacenters":["gra","rbx"],"want":[{"plans":["ks-2"],"ram_max_gb":32}]}`)
	// The first poll runs at start; the database is read once serve is done with it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serve(ctx, "127.0.0.1:0", false, false); err != nil {
		t.Fatal(err)
	}
	s := load(t, data)
	// No channel: the restock is an event all the same, and its pairs count as evented (a channel added later
	// doesn't push stock already there).
	if len(s.Events) != 1 || s.Events[0].Delivery != watch.NoChannel || s.Events[0].Retry || len(s.Notified) != 1 {
		t.Errorf("state %+v", s)
	}
}

func TestRetryOnSameEvent(t *testing.T) {
	data, _ := dirs(t, `{"datacenters":["gra"],"want":[{"ram_max_gb":32}]}`)
	db, err := store.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dead, err := notify.NewChannels([]string{"generic+http://127.0.0.1:1/hook"})
	if err != nil {
		t.Fatal(err)
	}
	client := ovh.NewClient()
	client.Base = fakeOVH(t).URL
	w := &watcher{cfg: watch.Config{Datacenters: []string{"gra"}, Interval: watch.Duration(time.Minute),
		Want: []watch.Want{{RAMMax: 32}}}, client: client, channels: dead, db: db}
	w.poll(t.Context())
	if e := w.state.Events; len(e) != 1 || !e[0].Retry || !strings.HasPrefix(e[0].Delivery, "failed") {
		t.Fatalf("events %+v", e)
	}
	// The channel works again: the same event is delivered, no second one.
	h, url := newHook(t)
	if w.channels, err = notify.NewChannels([]string{url}); err != nil {
		t.Fatal(err)
	}
	w.poll(t.Context())
	h.waitFor(t, "KS-2")
	if e := w.state.Events; len(e) != 1 || e[0].Retry || e[0].Delivery != watch.Sent {
		t.Errorf("events %+v", e)
	}
	w.poll(t.Context())
	if len(h.bodies) != 1 {
		t.Errorf("sent again: %q", h.bodies)
	}
	if s, _ := db.Load(t.Context()); len(s.Events) != 1 || s.Events[0].Delivery != watch.Sent {
		t.Errorf("stored %+v", s.Events)
	}
}

func TestRetrySoldOutComesBack(t *testing.T) {
	data, _ := dirs(t, `{"datacenters":["gra","rbx"],"want":[{"ram_max_gb":32}]}`)
	db, err := store.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dead, err := notify.NewChannels([]string{"generic+http://127.0.0.1:1/hook"})
	if err != nil {
		t.Fatal(err)
	}
	// gra flips, rbx stays in stock: the failed event keeps retrying for rbx.
	avail := "1H-low"
	client := ovh.NewClient()
	client.Base = fakeOVHWith(t, func(dc string) string {
		if dc == "gra" {
			return avail
		}
		return "1H-low"
	}).URL
	w := &watcher{cfg: watch.Config{Datacenters: []string{"gra", "rbx"}, Interval: watch.Duration(time.Minute),
		Want: []watch.Want{{RAMMax: 32}}}, client: client, channels: dead, db: db}
	w.poll(t.Context()) // in at gra and rbx, the push fails: retried while in stock
	avail = "unavailable"
	w.poll(t.Context()) // gra sold out, rbx retried and failed again
	if e := w.state.Events; len(e) != 1 || !e[0].Retry {
		t.Fatalf("events %+v", e)
	}
	// gra back, the channel works: one push with both, gra as fresh, not gra a second time from the old event.
	h, url := newHook(t)
	if w.channels, err = notify.NewChannels([]string{url}); err != nil {
		t.Fatal(err)
	}
	avail = "1H-low"
	w.poll(t.Context())
	h.waitFor(t, "KS-2")
	w.poll(t.Context())
	// (The polls share a second here, so the new event takes the old one's place: one per At and Kind.)
	if len(h.bodies) != 1 || strings.Count(h.bodies[0], "· gra") != 1 || !strings.Contains(h.bodies[0], "rbx") ||
		w.state.Events[0].Delivery != watch.Sent || w.state.Events[0].Retry {
		t.Errorf("bodies %q events %+v", h.bodies, w.state.Events)
	}
}

func TestServeNotifyTest(t *testing.T) {
	h, url := newHook(t)
	t.Setenv("BACKEND_NOTIFY_URLS", url)
	if err := serve(context.Background(), "127.0.0.1:0", true, false); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 || !strings.Contains(h.bodies[0], "Test push") {
		t.Errorf("bodies %q", h.bodies)
	}
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	if err := serve(context.Background(), "127.0.0.1:0", true, false); err == nil {
		t.Error("no channel accepted")
	}
	t.Setenv("BACKEND_NOTIFY_URLS", "nope://x")
	if err := serve(context.Background(), "127.0.0.1:0", true, false); err == nil {
		t.Error("bad URL accepted")
	}
}

func TestNotifyURLsOff(t *testing.T) {
	on, off, err := notifyURLs("!SMTP://user:s3cret@mail.example.com:587/?to=a@b.ch  ntfy://ntfy.example.com/topic")
	if err != nil || len(on) != 1 || !strings.HasPrefix(on[0], "ntfy://") || len(off) != 1 || off[0] != "smtp" {
		t.Errorf("on %v off %v %v", on, off, err)
	}
	// A typo without "://" must not show the rest: it may hold the password.
	if _, off, _ := notifyURLs("!smtp:user:s3cret@mail.example.com"); len(off) != 1 || strings.Contains(off[0], "s3cret") {
		t.Errorf("typo leaks: %v", off)
	}
	// A lone "!" would leave the next URL on.
	if _, _, err := notifyURLs("! smtp://u:p@mail.example.com"); err == nil {
		t.Error("lone ! accepted")
	}
	// Only switched-off channels: -notify-test has nothing to send to.
	h, url := newHook(t)
	t.Setenv("BACKEND_NOTIFY_URLS", "!"+url)
	if err := serve(context.Background(), "127.0.0.1:0", true, false); err == nil {
		t.Error("test push with every channel off")
	}
	if len(h.bodies) != 0 {
		t.Error("switched-off channel got a push")
	}
}

func TestHello(t *testing.T) {
	h, url := newHook(t)
	off, offURL := newHook(t)
	dead := "generic+http://127.0.0.1:1/hook"
	on, _, err := notifyURLs(url + " " + dead + " !" + offURL)
	if err != nil {
		t.Fatal(err)
	}
	marks := func() *store.Store {
		db, err := store.Open(t.Context(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := marks()
	now := time.Now()
	hello(t.Context(), on, db, now)
	if len(h.bodies) != 1 || !strings.Contains(h.bodies[0], "sent at every start") || len(off.bodies) != 0 {
		t.Fatalf("bodies %q, switched off %q", h.bodies, off.bodies)
	}
	took, err := db.Hello(t.Context())
	if err != nil || len(took) != 1 || took[urlKey(url)].IsZero() {
		t.Errorf("marks %v %v: only the channel that took it", took, err)
	}
	for k := range took {
		if strings.Contains(k, "127.0.0.1") {
			t.Errorf("mark holds the URL: %s", k)
		}
	}

	// Within helloQuiet the working channel is quiet; a channel added since is tested.
	h2, url2 := newHook(t)
	hello(t.Context(), append(on, url2), db, now.Add(helloQuiet-time.Second))
	if len(h.bodies) != 1 || len(h2.bodies) != 1 {
		t.Errorf("within helloQuiet: %d, new channel %d", len(h.bodies), len(h2.bodies))
	}
	hello(t.Context(), on, db, now.Add(helloQuiet))
	if len(h.bodies) != 2 {
		t.Error("no test push after helloQuiet")
	}

	// A shutdown cuts the send: nothing counts as taken.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fresh := marks()
	hello(ctx, []string{url}, fresh, now)
	if took, _ := fresh.Hello(t.Context()); !took[urlKey(url)].IsZero() {
		t.Errorf("cut send marked as taken: %v", took)
	}
}

func TestServeRefusesBadConfig(t *testing.T) {
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	dirs(t, `{"datacenters":["gra"],"want":[{"ram_max":64}]}`)
	if err := serve(context.Background(), "127.0.0.1:0", false, false); err == nil {
		t.Error("bad config accepted")
	}
}

func TestPollFailureKeepsState(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()
	apiBase = down.URL
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	data, _ := dirs(t, `{"datacenters":["gra"],"want":[{"ram_max_gb":64}]}`)
	db, err := store.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Save(t.Context(), watch.State{}, watch.State{Notified: []string{"x@gra"}}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serve(ctx, "127.0.0.1:0", false, false); err != nil {
		t.Fatal(err)
	}
	if s := load(t, data); len(s.Notified) != 1 {
		t.Errorf("failed poll changed the state: %v", s)
	}
}
