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

	"github.com/trick77/ovh-availability-check/internal/watch"
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

func dirs(t *testing.T, config string) (data, conf string) {
	t.Helper()
	data, conf = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BACKEND_DATA_DIR", data)
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
	go func() { done <- serve(ctx, "127.0.0.1:0", false) }()
	select {
	case <-h.got:
	case <-time.After(10 * time.Second):
		t.Fatal("no push")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(h.bodies[0], "KS-2 · Intel Xeon-D 1540 · 32 GB · 2×450GB NVME · 18.99 €/mo") ||
		!strings.HasSuffix(h.bodies[0], "· gra\nhttps://eco.ovhcloud.com/fr/kimsufi/ks-2/") {
		t.Errorf("body %q", h.bodies[0])
	}
	s, err := watch.LoadState(filepath.Join(data, "state.json"))
	if err != nil || len(s.Notified) != 1 || len(s.Pushes) != 1 || len(s.Spans) != 1 {
		t.Fatalf("state %+v %v", s, err)
	}

	// A restart with the same stock pushes nothing.
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- serve(ctx, "127.0.0.1:0", false) }()
	select {
	case <-h.got:
		t.Error("pushed again after a restart")
	case <-time.After(time.Second):
	}
	cancel()
	<-done
}

func TestServeNotifyTest(t *testing.T) {
	h, url := newHook(t)
	t.Setenv("BACKEND_NOTIFY_URLS", url)
	if err := serve(context.Background(), "127.0.0.1:0", true); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 || !strings.Contains(h.bodies[0], "Test push") {
		t.Errorf("bodies %q", h.bodies)
	}
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	if err := serve(context.Background(), "127.0.0.1:0", true); err == nil {
		t.Error("no channel accepted")
	}
	t.Setenv("BACKEND_NOTIFY_URLS", "nope://x")
	if err := serve(context.Background(), "127.0.0.1:0", true); err == nil {
		t.Error("bad URL accepted")
	}
}

func TestServeRefusesBadConfig(t *testing.T) {
	t.Setenv("BACKEND_NOTIFY_URLS", "")
	dirs(t, `{"datacenters":["gra"],"want":[{"ram_max":64}]}`)
	if err := serve(context.Background(), "127.0.0.1:0", false); err == nil {
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
	if err := serve(ctx, "127.0.0.1:0", false); err != nil {
		t.Fatal(err)
	}
	s, _ := watch.LoadState(filepath.Join(data, "state.json"))
	if len(s.Notified) != 1 {
		t.Errorf("failed poll changed the state: %v", s)
	}
}
