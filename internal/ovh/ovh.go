// Package ovh reads OVHcloud's public, unauthenticated APIs: which dedicated server variant is in stock where, and
// what it costs.
package ovh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// API is the EU endpoint. It lists every datacenter (EU, CA, APAC), so no other region is needed.
const API = "https://eu.api.ovh.com/v1"

// Catalogs hold the plans: eco is Kimsufi, So you Start and Rise; baremetalServers is Advance, Scale and HGR.
// Together they price all but a handful of the planCodes the availability API lists.
var Catalogs = []string{"eco", "baremetalServers"}

// Subsidiary picks the catalog's shop and currency. There is no Swiss one; FR prices in EUR, net of VAT.
const Subsidiary = "FR"

// micro is the catalog's price unit: 1899000000 is 18.99 €.
const micro = 1e8

// Client fetches from API.
type Client struct {
	HTTP *http.Client
	Base string
}

// NewClient is a Client with a timeout: the catalogs are 13 and 22 MB.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 90 * time.Second}, Base: API}
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ovh-availability-check")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// Availability is one server variant's stock, as the API lists it.
type Availability struct {
	FQN           string `json:"fqn"`
	PlanCode      string `json:"planCode"`
	Memory        string `json:"memory"`
	Storage       string `json:"storage"`
	SystemStorage string `json:"systemStorage"`
	Datacenters   []struct {
		Datacenter   string `json:"datacenter"`
		Availability string `json:"availability"`
	} `json:"datacenters"`
}

// Availabilities fetches every variant's stock. An empty list is an error: the API only answers [] to a filter
// that matches nothing, and taking it for "all sold out" would forget what was already pushed.
func (c *Client) Availabilities(ctx context.Context) ([]Availability, error) {
	var out []Availability
	if err := c.get(ctx, "/dedicated/server/datacenter/availabilities", &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("availability API answered an empty list")
	}
	return out, nil
}

// InStock tells an availability value that can be ordered ("1H-low", "1H-high", "72H", …) from one that can't.
func InStock(v string) bool {
	switch v {
	case "", "unavailable", "unknown", "comingSoon":
		return false
	}
	return true
}

type pricing struct {
	Interval   int      `json:"interval"`
	Mode       string   `json:"mode"`
	Capacities []string `json:"capacities"`
	Price      int64    `json:"price"`
}

type product struct {
	PlanCode      string    `json:"planCode"`
	InvoiceName   string    `json:"invoiceName"`
	Product       string    `json:"product"`
	Pricings      []pricing `json:"pricings"`
	AddonFamilies []struct {
		Name      string   `json:"name"`
		Mandatory bool     `json:"mandatory"`
		Default   string   `json:"default"`
		Addons    []string `json:"addons"`
	} `json:"addonFamilies"`
}

// Catalog is one or more public order catalogs, merged.
type Catalog struct {
	plans  map[string]product
	addons map[string]product
	source map[string]string // planCode → catalog name
	cpus   map[string]string // catalog/product → its description, the CPU ("2x AMD Epyc 9354")
}

type rawCatalog struct {
	Plans    []product `json:"plans"`
	Addons   []product `json:"addons"`
	Products []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"products"`
}

// Catalog fetches and merges Catalogs.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	cat := &Catalog{plans: map[string]product{}, addons: map[string]product{}, source: map[string]string{},
		cpus: map[string]string{}}
	for _, name := range Catalogs {
		var raw rawCatalog
		if err := c.get(ctx, "/order/catalog/public/"+name+"?ovhSubsidiary="+Subsidiary, &raw); err != nil {
			return nil, err
		}
		cat.add(name, raw)
	}
	return cat, nil
}

func (cat *Catalog) add(name string, raw rawCatalog) {
	for _, p := range raw.Plans {
		cat.plans[p.PlanCode] = p
		cat.source[p.PlanCode] = name
	}
	for _, a := range raw.Addons {
		cat.addons[a.PlanCode] = a
	}
	if cat.cpus == nil {
		cat.cpus = map[string]string{}
	}
	for _, p := range raw.Products {
		cat.cpus[name+"/"+p.Name] = p.Description
	}
}

// monthly is the monthly rent and the one-off setup fee when paid month by month (mode "default").
func (p product) monthly() (rent, setup int64, ok bool) {
	var haveRent bool
	for _, pr := range p.Pricings {
		if pr.Mode != "default" {
			continue
		}
		switch {
		case pr.Interval == 1 && slices.Contains(pr.Capacities, "renew"):
			rent, haveRent = pr.Price, true
		case pr.Interval == 0 && slices.Contains(pr.Capacities, "installation"):
			setup = pr.Price
		}
	}
	return rent, setup, haveRent
}

// specFamilies are the addon families an availability names the variant of. Their addons carry the spec plus a
// plan suffix that is not always the planCode ("ram-64g-ecc-2133-24sk20" for plan 24sk202).
var specFamilies = map[string]func(Availability) string{
	"memory":         func(a Availability) string { return a.Memory },
	"storage":        func(a Availability) string { return a.Storage },
	"system-storage": func(a Availability) string { return a.SystemStorage },
}

// Price is a variant's monthly rent and setup fee in EUR, net of VAT: the plan plus the addons its spec picks plus
// the defaults of every other mandatory family (bandwidth). ok is false when the plan or one of those addons is not
// in the catalog.
func (cat *Catalog) Price(a Availability) (rent, setup float64, ok bool) {
	if cat == nil {
		return 0, 0, false
	}
	plan, found := cat.plans[a.PlanCode]
	if !found {
		return 0, 0, false
	}
	r, s, ok := plan.monthly()
	if !ok {
		return 0, 0, false
	}
	for _, fam := range plan.AddonFamilies {
		var code string
		if spec, isSpec := specFamilies[fam.Name]; isSpec {
			want := spec(a)
			if want == "" {
				continue
			}
			code = pick(fam.Addons, want)
			if code == "" && fam.Name == "memory" {
				code = sameSize(fam.Addons, want)
			}
		} else if fam.Mandatory {
			code = fam.Default
		} else {
			continue
		}
		addon, found := cat.addons[code]
		if code == "" || !found {
			return 0, 0, false
		}
		ar, as, ok := addon.monthly()
		if !ok {
			return 0, 0, false
		}
		r += ar
		s += as
	}
	return float64(r) / micro, float64(s) / micro, true
}

// pick finds the addon for a spec: spec plus "-" plus a suffix. Of several (a spec that is a prefix of another),
// the shortest suffix wins.
func pick(addons []string, spec string) string {
	best := ""
	for _, a := range addons {
		if strings.HasPrefix(a, spec+"-") && (best == "" || len(a) < len(best)) {
			best = a
		}
	}
	return best
}

// sameSize finds the memory addon of the spec's size when the speeds disagree: the availability API lists KS-1-B
// as ram-32g-ecc-2133, its catalog sells ram-32g-ecc-2400 (2026-11). Only a single candidate counts.
func sameSize(addons []string, spec string) string {
	gb := ParseRAM(spec)
	if gb == 0 {
		return ""
	}
	var found []string
	for _, a := range addons {
		if ParseRAM(a) == gb {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// Name is the plan's commercial name and CPU, from its invoice name ("KS-2 | Intel Xeon-D 1540"). Some invoice
// names carry no CPU (KS-LE, Scale, HGR: "KS-LE-1"); their product's description has it ("Intel Xeon E3-1245v2",
// "2x AMD Epyc 9354", "1x Intel Xeon Gold 6554S").
func (cat *Catalog) Name(planCode string) (name, cpu string) {
	if cat == nil {
		return "", ""
	}
	p, ok := cat.plans[planCode]
	if !ok {
		return "", ""
	}
	name, cpu, _ = strings.Cut(p.InvoiceName, "|")
	cpu = strings.TrimSpace(cpu)
	if d := strings.TrimPrefix(strings.TrimSpace(cat.cpus[cat.source[planCode]+"/"+p.Product]), "1x "); cpu == "" &&
		looksLikeCPU(d) {
		cpu = d
	}
	return strings.TrimSpace(name), cpu
}

// looksLikeCPU keeps a product description that is marketing text out of the CPU column and the cpu wishes.
func looksLikeCPU(s string) bool {
	s = strings.ToLower(s)
	for _, f := range []string{"xeon", "epyc", "ryzen", "core i", "atom", "opteron"} {
		if strings.Contains(s, f) {
			return true
		}
	}
	_, ok := CPU(s)
	return ok
}

// OrderURL is where the plan is ordered. Only Kimsufi has a page per server (eco.ovhcloud.com/fr/kimsufi/ks-2/,
// checked 2026-11); everything else links to its range's overview.
func (cat *Catalog) OrderURL(planCode, name string) string {
	if cat == nil || cat.source[planCode] == "" {
		return "https://eco.ovhcloud.com/fr/"
	}
	if cat.source[planCode] == "baremetalServers" {
		return "https://www.ovhcloud.com/fr/bare-metal/"
	}
	if strings.HasPrefix(name, "KS-") {
		return "https://eco.ovhcloud.com/fr/kimsufi/" + strings.ToLower(name) + "/"
	}
	return "https://eco.ovhcloud.com/fr/"
}
