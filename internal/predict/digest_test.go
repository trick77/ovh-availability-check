package predict

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ovh-restock/internal/ovh"
	"github.com/trick77/ovh-restock/internal/watch"
)

func TestDigestDue(t *testing.T) {
	sun := time.Date(2026, 11, 29, 18, 0, 0, 0, zurich)
	for _, tc := range []struct {
		now, last time.Time
		due       bool
	}{
		{sun.Add(-time.Minute), sun.AddDate(0, 0, -1), false}, // Sunday 17:59: not yet
		{sun, sun.AddDate(0, 0, -1), true},                    // 18:00 sharp
		{sun.Add(3 * time.Hour), sun, false},                  // already sent
		{sun.AddDate(0, 0, 3), sun.Add(time.Hour), false},     // Wednesday after
		{sun.AddDate(0, 0, 7), sun, true},                     // next Sunday
		{sun.AddDate(0, 0, 2), sun.AddDate(0, 0, -7), true},   // missed while down: sent late
	} {
		if got := DigestDue(tc.now, tc.last, zurich); got != tc.due {
			t.Errorf("now %s last %s: %v", tc.now.Format(time.DateTime), tc.last.Format(time.DateTime), got)
		}
	}
}

func digestFixture() (watch.State, []watch.Variant, watch.Config) {
	r := rand.New(rand.NewPCG(1, 1))
	st := synth(weekly(r, []time.Weekday{time.Tuesday, time.Thursday}, 6, 0, 20*time.Minute, 0), 30*time.Minute)
	v := watch.Variant{FQN: fqn, Name: "KS-2", RAM: 32, Disks: []ovh.Disk{{Count: 2, SizeGB: 450, Kind: "nvme"}},
		Rent: 18.99, Priced: true, Stock: map[string]string{"gra": "unavailable"}}
	st.Prices = map[string][]watch.Price{fqn: {{At: nowT.AddDate(0, 0, -20), Rent: 20.99}, {At: nowT.AddDate(0, 0, -2), Rent: 18.99}}}
	return st, []watch.Variant{v}, watch.Config{Want: []watch.Want{{PriceMax: 30}}}
}

func TestBuildDigest(t *testing.T) {
	st, vs, c := digestFixture()
	d := BuildDigest(st, vs, c, map[string]Prediction{fqn: {Pattern: true, Summary: "Tue/Thu 06:00", Hits: 2, Tries: 2}},
		nowT, zurich)
	if d.Empty() || len(d.Servers) != 1 || d.Servers[0].Windows != 2 || d.Servers[0].Hits != "2 of 2" ||
		len(d.Prices) != 1 {
		t.Fatalf("digest %+v", d)
	}
	if !strings.Contains(Plain(d), "20.99 → 18.99") {
		t.Errorf("plain %q", Plain(d))
	}
	if !BuildDigest(watch.State{}, nil, c, nil, nowT, zurich).Empty() {
		t.Error("empty week not empty")
	}
}

func TestGrounded(t *testing.T) {
	st, vs, c := digestFixture()
	d := BuildDigest(st, vs, c, nil, nowT, zurich)
	if !Grounded("KS-2 was in stock 2 times; price 20.99 → 18.99.", d) {
		t.Error("grounded text refused")
	}
	if Grounded("KS-2 was in stock 3 times.", d) {
		t.Error("invented figure passed")
	}
	if Grounded(strings.Repeat("x", maxDigest+1), d) {
		t.Error("too long passed")
	}
}

func TestWords(t *testing.T) {
	st, vs, c := digestFixture()
	d := BuildDigest(st, vs, c, nil, nowT, zurich)
	s, _ := service(t, `{"text":"KS-2 was in stock 2 times this week."}`)
	if _, body := s.Words(context.Background(), d); body != "KS-2 was in stock 2 times this week." {
		t.Errorf("grounded words replaced: %q", body)
	}
	s, _ = service(t, `{"text":"KS-2 was in stock 7 times."}`)
	if _, body := s.Words(context.Background(), d); body != Plain(d) {
		t.Errorf("invented figure not replaced by the plain list: %q", body)
	}
}

func TestWish(t *testing.T) {
	s, _ := service(t, `{"name":"nvme box","ram_min_gb":64,"ram_max_gb":0,"disk":"NVMe","disk_min_tb":0,"cpu":"",`+
		`"price_max_eur":30,"cpu_min_pct":-10,"unclear":["quiet"]}`)
	w, unclear, err := s.Wish(context.Background(), "nvme, 64 GB, unter 30 €, höchstens 10 % langsamer, leise", "Intel Xeon E3-1230 v6")
	if err != nil || w.RAMMin != 64 || w.Disk != "nvme" || w.PriceMax != 30 || w.CPUMinPct == nil || *w.CPUMinPct != -10 ||
		len(unclear) != 1 {
		t.Fatalf("wish %+v %v %v", w, unclear, err)
	}
	// cpu_min_pct without a reference CPU doesn't check.
	if _, _, err := s.Wish(context.Background(), "x", ""); err == nil {
		t.Error("cpu_min_pct without reference passed")
	}
	s, _ = service(t, `{"name":"","ram_min_gb":0,"ram_max_gb":0,"disk":"","disk_min_tb":0,"cpu":"","price_max_eur":0,`+
		`"cpu_min_pct":null,"unclear":["fast"]}`)
	if _, _, err := s.Wish(context.Background(), "something fast", ""); err == nil {
		t.Error("empty wish passed")
	}
}
