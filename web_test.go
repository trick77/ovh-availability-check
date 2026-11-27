package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trick77/toolkit/notify"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/watch"
)

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Minute: "2m", 10 * time.Minute: "10m", time.Hour: "1h", 90 * time.Second: "1m30s",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestPage(t *testing.T) {
	ch, err := notify.NewChannels([]string{"ntfy://user:s3cret@ntfy.example.com/topic"})
	if err != nil {
		t.Fatal(err)
	}
	ks2 := watch.Variant{FQN: "24sk202.x", PlanCode: "24sk202", Name: "KS-2", CPU: "Intel Xeon-D 1540", RAM: 64,
		Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Rent: 26.99, Priced: true,
		Stock: map[string]string{"gra": "72H"}}
	gone := ks2
	gone.FQN, gone.Name, gone.Stock = "gone", "KS-GONE", map[string]string{"gra": "unavailable"}
	w := &watcher{
		cfg: watch.Config{Datacenters: []string{"gra"}, Interval: watch.Duration(2 * time.Minute),
			Want: []watch.Want{{Name: "cheap", PriceMax: 30}, {Name: "typo", CPU: "pentium"}}},
		channels: ch, variants: []watch.Variant{ks2, gone}, polledAt: time.Now(),
		pushes: []push{{At: time.Now(), Title: "OVH: 1 server in stock", Body: "KS-2", Err: "failed on channel 1 (ntfy)"}},
	}
	get := func(path string) string {
		rec := httptest.NewRecorder()
		w.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	body := get("/")
	for _, want := range []string{"KS-2", "26.99", "every 2m", `class="zero"`, "Show all 2 variants",
		"failed on channel 1 (ntfy)"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	if strings.Contains(body, "KS-GONE") {
		t.Error("/ lists a sold-out variant under wanted")
	}
	if strings.Contains(body, "s3cret") || strings.Contains(body, "ntfy.example.com") {
		t.Error("page leaks the notify URL")
	}
	if all := get("/?all=1"); !strings.Contains(all, "KS-GONE") || !strings.Contains(all, `class="out"`) {
		t.Error("?all=1 lacks the sold-out variant")
	}
}
