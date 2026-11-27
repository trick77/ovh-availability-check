package ovh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseRAM(t *testing.T) {
	for in, want := range map[string]int{
		"ram-64g-ecc-2133": 64, "ram-1024g-ecc-2933": 1024, "ram-32g-ddr4-ecc-2666": 32, "": 0, "noram": 0,
	} {
		if got := ParseRAM(in); got != want {
			t.Errorf("ParseRAM(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDisks(t *testing.T) {
	for in, want := range map[string][]Disk{
		"softraid-2x450nvme":                   {{2, 450, "nvme"}},
		"softraid-4x2000sa":                    {{4, 2000, "hdd"}},
		"hardraid-12x3840sas":                  {{12, 3840, "hdd"}},
		"softraid-2x960ssd":                    {{2, 960, "ssd"}},
		"softraid-2x960nvme-pcie-gen5":         {{2, 960, "nvme"}},
		"softraid-16x24000-hdd-sas":            {{16, 24000, "hdd"}},
		"hybridsoftraid-2x4000sa-2x450nvme":    {{2, 4000, "hdd"}, {2, 450, "nvme"}},
		"softraid-24x22000-hdd-sas-2x3840nvme": {{24, 22000, "hdd"}, {2, 3840, "nvme"}},
		"noraid-0":                             nil,
	} {
		if got := ParseDisks(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseDisks(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestInStock(t *testing.T) {
	for in, want := range map[string]bool{
		"1H-low": true, "1H-high": true, "72H": true, "unavailable": false, "unknown": false, "comingSoon": false, "": false,
	} {
		if InStock(in) != want {
			t.Errorf("InStock(%q) = %v", in, !want)
		}
	}
}

// fixtures: testdata holds 24sk202 (KS-2), 24rise01-v2 and 26scaleamd01-v4, cut from the live APIs (2026-11).
func fixtures(t *testing.T) ([]Availability, *Catalog) {
	t.Helper()
	var av []Availability
	read(t, "testdata/availabilities.json", &av)
	var raw rawCatalog
	read(t, "testdata/catalog.json", &raw)
	cat := &Catalog{plans: map[string]product{}, addons: map[string]product{}, source: map[string]string{}}
	cat.add("eco", raw)
	return av, cat
}

func read(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, av []Availability, fqn string) Availability {
	t.Helper()
	for _, a := range av {
		if a.FQN == fqn {
			return a
		}
	}
	t.Fatalf("no %s in fixture", fqn)
	return Availability{}
}

func TestPrice(t *testing.T) {
	av, cat := fixtures(t)
	for _, tc := range []struct {
		fqn         string
		rent, setup float64
	}{
		// KS-2 18.99 base; the 64 GB addon adds 8.
		{"24sk202.ram-32g-ecc-2133.softraid-2x450nvme", 18.99, 18.99},
		{"24sk202.ram-64g-ecc-2133.softraid-2x450nvme", 26.99, 18.99},
	} {
		rent, setup, ok := cat.Price(find(t, av, tc.fqn))
		if !ok || rent != tc.rent || setup != tc.setup {
			t.Errorf("%s: %.2f + %.2f setup (ok %v), want %.2f + %.2f", tc.fqn, rent, setup, ok, tc.rent, tc.setup)
		}
	}
	// Every fixture variant prices: spec addons, system storage and mandatory defaults all resolve.
	for _, a := range av {
		if _, _, ok := cat.Price(a); !ok {
			t.Errorf("%s: no price", a.FQN)
		}
	}
	if _, _, ok := cat.Price(Availability{PlanCode: "nope"}); ok {
		t.Error("unknown plan priced")
	}
	unsold := find(t, av, "24sk202.ram-64g-ecc-2133.softraid-2x450nvme")
	unsold.Storage = "softraid-9x9nvme"
	if _, _, ok := cat.Price(unsold); ok {
		t.Error("a storage the catalog doesn't sell priced")
	}
}

func TestSameSize(t *testing.T) {
	addons := []string{"ram-32g-ecc-2400-26sk10b-v1", "ram-64g-ecc-2400-26sk10b-v1"}
	if got := sameSize(addons, "ram-32g-ecc-2133"); got != addons[0] {
		t.Errorf("got %q", got)
	}
	if got := sameSize(append(addons, "ram-32g-ecc-3200-x"), "ram-32g-ecc-2133"); got != "" {
		t.Errorf("two candidates picked %q", got)
	}
}

func TestName(t *testing.T) {
	_, cat := fixtures(t)
	name, cpu := cat.Name("24sk202")
	if name != "KS-2" || cpu != "Intel Xeon-D 1540" {
		t.Errorf("got %q, %q", name, cpu)
	}
	if u := cat.OrderURL("24sk202", name); u != "https://eco.ovhcloud.com/fr/kimsufi/ks-2/" {
		t.Errorf("order URL %s", u)
	}

	// No CPU in the invoice name: the product's description has it.
	var raw rawCatalog
	raw.Plans = []product{{PlanCode: "27skle01-v1", InvoiceName: "KS-LE-1", Product: "27skle01"},
		{PlanCode: "x", InvoiceName: "HGR-STOR-1", Product: "y"}}
	raw.Products = append(raw.Products,
		struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}{"27skle01", "Intel Xeon E3-1245v2"},
		struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}{"y", "1x Intel Xeon Gold 6554S"})
	cat.add("eco", raw)
	raw.Plans = append(raw.Plans, product{PlanCode: "m", InvoiceName: "PROMO-1", Product: "z"})
	raw.Products = append(raw.Products, struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}{"z", "Our best server for the price"})
	cat.add("eco", raw)
	for code, want := range map[string]string{"27skle01-v1": "Intel Xeon E3-1245v2", "x": "Intel Xeon Gold 6554S",
		"m": ""} {
		if name, cpu := cat.Name(code); name == "" || cpu != want {
			t.Errorf("%s: %q, %q", code, name, cpu)
		}
	}
}

func TestAvailabilitiesEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "ovh-restock") {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Base: srv.URL}
	if _, err := c.Availabilities(context.Background()); err == nil {
		t.Error("empty list taken")
	}
}
