package main

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/watch"
)

func TestActivity(t *testing.T) {
	v := watch.Variant{FQN: "a", PlanCode: "24sk40-v1", Name: "KS-4", CPU: "Intel Xeon-E3 1230 v6", RAM: 16,
		Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}}, Rent: 16.99, Priced: true}
	cfg := watch.Config{Want: []watch.Want{{PriceMax: 20}}}
	t0 := time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)
	in := func(st map[string]string) watch.Variant { x := v; x.Stock = st; return x }
	var s watch.State
	s = s.Observe([]watch.Variant{in(map[string]string{"rbx": "unavailable", "gra": "unavailable"})}, t0, 2*time.Minute)
	s = s.Observe([]watch.Variant{in(map[string]string{"rbx": "1H-low", "gra": "72H"})}, t0.Add(2*time.Minute), 2*time.Minute)
	s = s.AddEvent(watch.Event{At: t0.Add(2 * time.Minute), Kind: watch.KindRestock, Delivery: watch.Sent,
		Items: watch.Items([]watch.Variant{in(map[string]string{"rbx": "1H-low", "gra": "72H"})},
			[]watch.Pair{{FQN: "a", DC: "gra"}, {FQN: "a", DC: "rbx"}})})
	s = s.Observe([]watch.Variant{in(map[string]string{"rbx": "unavailable", "gra": "unavailable"})}, t0.Add(4*time.Minute), 2*time.Minute)
	s = s.AddEvent(watch.Event{At: t0.Add(5 * time.Minute), Kind: watch.KindDigest, Title: "OVH week 48", Body: "KS-4 came twice."})

	rows := activity([]watch.Variant{v}, cfg, s, t0.Add(10*time.Minute))
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	dig, out, arr := rows[0], rows[1], rows[2]
	if dig.Kind != "digest" || !strings.Contains(dig.Text, "week 48") || len(dig.Body) == 0 {
		t.Errorf("digest %+v", dig)
	}
	// Both DCs sold out at one poll: one row, two pills, the stay.
	if out.Kind != "out" || len(out.DCs) != 2 || out.After != "after 2m" {
		t.Errorf("out %+v", out)
	}
	// The arrival in two DCs is one row, carrying its push's delivery: the push has no row of its own.
	if arr.Kind != "in" || len(arr.DCs) != 2 || arr.DCs[0].Text != "gra" || arr.DCs[0].Class != "pill slow" ||
		arr.Delivery.Text != watch.Sent || arr.Code != "24sk40-v1" || arr.Spec != "Xeon-E3 1230 v6 · 16 GB · 2×450GB NVME" {
		t.Errorf("in %+v", arr)
	}
}

func TestEvents(t *testing.T) {
	w := &watcher{hub: newHub()}
	srv := httptest.NewServer(w.routes())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	for w.hub.Len() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	w.published()
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil || line != "event: poll\n" {
		t.Errorf("first line %q %v", line, err)
	}
	// Shutdown closes every stream.
	w.hub.Close()
	for {
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
	}
}
