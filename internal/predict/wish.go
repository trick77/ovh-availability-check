package predict

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/trick77/ovh-availability-check/internal/ovh"
	"github.com/trick77/ovh-availability-check/internal/watch"
)

var wishSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"name", "ram_min_gb", "ram_max_gb", "disk", "disk_min_tb", "cpu", "price_max_eur",
		"cpu_min_pct", "unclear"},
	"properties": map[string]any{
		"name":          map[string]any{"type": "string"},
		"ram_min_gb":    map[string]any{"type": "integer"},
		"ram_max_gb":    map[string]any{"type": "integer"},
		"disk":          map[string]any{"type": "string", "enum": []string{"", "nvme", "ssd", "hdd"}},
		"disk_min_tb":   map[string]any{"type": "number"},
		"cpu":           map[string]any{"type": "string"},
		"price_max_eur": map[string]any{"type": "number"},
		"cpu_min_pct":   map[string]any{"type": []string{"integer", "null"}},
		"unclear":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

const wishPrompt = `You turn a user's wish for a dedicated server, written in any language, into filter fields. ` +
	`Fields left out of the wish are 0, "" or null; never fill one the user didn't ask for.

- ram_min_gb / ram_max_gb: memory in GB. disk: nvme, ssd or hdd when the user names a kind. disk_min_tb: raw total ` +
	`disk in TB (1 TB = 1000 GB), of that kind when one is named. price_max_eur: monthly price limit in EUR, net.
- cpu: a case-insensitive part of the CPU name the user asks for (e.g. "epyc", "ryzen", "xeon-e"); the known CPUs ` +
	`are listed in the data.
- cpu_min_pct: only when the user relates speed to the reference CPU in the data: -10 is "at most 10 % slower", ` +
	`20 "at least 20 % faster"; null otherwise.
- name: a short label for the wish.
- unclear: each part of the wish that maps to no field ("quiet", "good network"), in the user's words.`

// Wish turns free text into a wish, checked like config.json's; unclear are the parts that map to no field.
func (s *Service) Wish(ctx context.Context, text, ref string) (watch.Want, []string, error) {
	data := map[string]any{"wish": text, "known_cpus": ovh.CPUNames(), "reference_cpu": ref}
	obj, err := s.call(ctx, "wish", wishSchema, wishPrompt, data, 800)
	if err != nil {
		return watch.Want{}, nil, err
	}
	var a struct {
		watch.Want
		Unclear []string `json:"unclear"`
	}
	if err := json.Unmarshal([]byte(obj), &a); err != nil {
		return watch.Want{}, nil, fmt.Errorf("answer: %w", err)
	}
	a.Disk = strings.ToLower(a.Disk)
	if err := a.Check(ref != ""); err != nil {
		return a.Want, a.Unclear, fmt.Errorf("the wish doesn't check: %w", err)
	}
	return a.Want, a.Unclear, nil
}
