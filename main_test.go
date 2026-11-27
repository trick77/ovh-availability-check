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

	"github.com/trick77/ovh-restock/internal/watch"
)

// fakeOVH serves the fixtures, KS-2 32 GB NVMe put in stock in gra.
func fakeOVH(t *testing.T) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile("internal/ovh/testdata/availabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	var av []map[string]any
	if err := json.Unmarshal(b, &av); err != nil {
		t.Fatal(err)
	}
	for _, a := range av {
		if a["fqn"] == "24sk202.ram-32g-ecc-2133.softraid-2x450nvme" {
			for _, d := range a["datacenters"].([]any) {
				if dc := d.(map[string]any); dc["datacenter"] == "gra" {
					dc["availability"] = "1H-low"
				}
			}
		}
	}
	stock, _ := json.Marshal(av)
	catalog, err := os.ReadFile("internal/ovh/testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dedicated/server/datacenter/availabilities", func(w http.ResponseWriter, _ *http.Request) {
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
	s, err := watch.LoadState(filepath.Join(data, "state.json"))
	if err != nil || len(s.Notified) != 1 || len(s.Pushes) != 1 || len(s.Spans) != 1 {
		t.Fatalf("state %+v %v", s, err)
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
	data := t.TempDir()
	now := time.Now()
	hello(t.Context(), on, data, now)
	if len(h.bodies) != 1 || !strings.Contains(h.bodies[0], "sent at every start") || len(off.bodies) != 0 {
		t.Fatalf("bodies %q, switched off %q", h.bodies, off.bodies)
	}
	b, err := os.ReadFile(filepath.Join(data, helloMark))
	if err != nil || strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("mark %s %v", b, err)
	}
	var took map[string]time.Time
	if err := json.Unmarshal(b, &took); err != nil || len(took) != 1 || took[urlKey(url)].IsZero() {
		t.Errorf("mark %v %v: only the channel that took it", took, err)
	}

	// Within helloQuiet the working channel is quiet; a channel added since is tested.
	h2, url2 := newHook(t)
	hello(t.Context(), append(on, url2), data, now.Add(helloQuiet-time.Second))
	if len(h.bodies) != 1 || len(h2.bodies) != 1 {
		t.Errorf("within helloQuiet: %d, new channel %d", len(h.bodies), len(h2.bodies))
	}
	hello(t.Context(), on, data, now.Add(helloQuiet))
	if len(h.bodies) != 2 {
		t.Error("no test push after helloQuiet")
	}

	// A shutdown cuts the send: nothing counts as taken.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fresh := t.TempDir()
	hello(ctx, []string{url}, fresh, now)
	if b, _ := os.ReadFile(filepath.Join(fresh, helloMark)); strings.Contains(string(b), urlKey(url)) {
		t.Errorf("cut send marked as taken: %s", b)
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
	if err := (watch.State{Notified: []string{"x@gra"}}).Save(filepath.Join(data, "state.json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serve(ctx, "127.0.0.1:0", false, false); err != nil {
		t.Fatal(err)
	}
	s, _ := watch.LoadState(filepath.Join(data, "state.json"))
	if len(s.Notified) != 1 {
		t.Errorf("failed poll changed the state: %v", s)
	}
}
