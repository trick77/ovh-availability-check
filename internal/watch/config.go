// Package watch turns OVH's stock into the variants the config wants and decides what to push.
package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is conf/config.json.
type Config struct {
	// Datacenters to watch; stock elsewhere is ignored.
	Datacenters []string `json:"datacenters"`
	// Interval between availability polls, as a Go duration ("2m").
	Interval Duration `json:"interval"`
	// Want: a push fires when any one of them matches.
	Want []Want `json:"want"`
}

// Want is one wish. Every field set must hold; unset fields don't filter.
type Want struct {
	Name     string  `json:"name,omitempty"`
	RAMMin   int     `json:"ram_min_gb,omitempty"`
	RAMMax   int     `json:"ram_max_gb,omitempty"`
	Disk     string  `json:"disk,omitempty"`        // nvme, ssd or hdd: at least one disk group of that kind
	DiskMin  float64 `json:"disk_min_tb,omitempty"` // raw TB, of Disk's kind when Disk is set
	CPU      string  `json:"cpu,omitempty"`         // case-insensitive substring of the CPU name
	PriceMax float64 `json:"price_max_eur,omitempty"`
}

// Label names the wish in the UI and the log.
func (w Want) Label(i int) string {
	if w.Name != "" {
		return w.Name
	}
	return fmt.Sprintf("want %d", i+1)
}

// Duration reads "2m" from JSON.
type Duration time.Duration

// UnmarshalJSON reads a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"2m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// minInterval keeps the poll polite: the availability list is 4 MB.
const minInterval = time.Minute

// LoadConfig reads and checks path. Unknown keys are an error, so a typo ("ram_max": 64) can't silently widen a wish.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's BACKEND_CONF_DIR
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) check() error {
	if c.Interval == 0 {
		c.Interval = Duration(2 * time.Minute)
	}
	if time.Duration(c.Interval) < minInterval {
		return fmt.Errorf("interval %s is below %s", c.Interval, minInterval)
	}
	if len(c.Datacenters) == 0 {
		return errors.New("no datacenters")
	}
	if len(c.Want) == 0 {
		return errors.New("no want")
	}
	for i, w := range c.Want {
		if w == (Want{Name: w.Name}) {
			return fmt.Errorf("%s: no criterion, it would match every server", w.Label(i))
		}
		switch strings.ToLower(w.Disk) {
		case "", "nvme", "ssd", "hdd":
		default:
			return fmt.Errorf("%s: disk %q is not nvme, ssd or hdd", w.Label(i), w.Disk)
		}
		if w.RAMMin > 0 && w.RAMMax > 0 && w.RAMMin > w.RAMMax {
			return fmt.Errorf("%s: ram_min_gb above ram_max_gb", w.Label(i))
		}
	}
	return nil
}
