# AGENTS.md

OVH dedicated server stock watcher. Go, stdlib + `trick77/toolkit` (notify, llmwire). No JS, page is
`html/template`.

## OVH API facts (measured 2026-11)

- All public, no auth. `eu.api.ovh.com/v1/dedicated/server/datacenter/availabilities` lists every DC worldwide
  (~4 MB, 256 planCodes). Answers `[]` to a planCode it doesn't know: an empty list is treated as a failed poll.
- Availability values: `1H-low`, `1H-high`, `72H` orderable; `unavailable`, `unknown`, `comingSoon` not.
- Prices: `/order/catalog/public/{eco,baremetalServers}?ovhSubsidiary=FR`, 13 + 22 MB, refetched every 6h.
  Unit 1e-8 EUR, net of VAT. No Swiss subsidiary. Mode `default`, interval 1 `renew` = monthly, interval 0
  `installation` = setup.
- Variant price = plan + addon per spec family (memory, storage, system-storage) + default of every other mandatory
  family (bandwidth, vrack). Addon code = spec + `-` + plan suffix, suffix is NOT always the planCode
  (`ram-64g-ecc-2133-24sk20` for plan `24sk202`).
- Availability RAM speed may disagree with the catalog (KS-1-B: 2133 vs 2400): fallback to the single addon of the same
  size. ~2% of variants stay unpriced: configs the catalog doesn't sell (24sk50-v1 2x4000sa, 27skle03 64g, noraid-0
  Scale/HCI). Unpriced never matches `price_max_eur`.
- KS-LE, Scale, HGR plans have no CPU in `invoiceName`; `products[]` (by the plan's `product`) has it in
  `description` ("2x AMD Epyc 9354", "1x …") and `blobs.technical.server.cpu` (cores, threads, number).
- Only Kimsufi has a per-server order page: `eco.ovhcloud.com/fr/kimsufi/<name lowercased>/`, all 22 KS names 200,
  unknown names 404. So you Start's `/fr/so-you-start/` 404s.

- Speed + cores/threads: `internal/ovh/cpus.json`, PassMark CPU Mark + single thread, looked up by hand. The
  catalog's own `score` is missing on some products and not PassMark's current figure. A CPU OVH adds later shows `?`
  until added there. Invoices spell one chip several ways (`Xeon-E3 1230 v6`, `i7-6700k`, `EPYC TURIN 9455`):
  `cpuKey` normalizes.

## History (why)

- Starts with the deploy. Nothing before the first catalog: `Wanted` needs CPU/price, spans would start late.
- A pause over 3 intervals + 5 min ends every streak and span: the page must not claim stock nobody saw. Slack:
  polls are an interval apart after each ends, a hanging catalog takes 2×90 s. Only a poll that finds a pair gone
  logs "out"; a span that starts unseen (first poll, gap, restart, turned wanted while in stock) logs no "in".
- A pair that stops being wanted while in stock (price rise, config edit) keeps its span until it sells out, or
  the log would show a fake "out".
- Spans only for in-scope pairs (user): wanted or *just above* a wish (one rule missed narrowly: price ≤ +10 %, RAM
  one step, disk ≤ 10 %; CPU/disk kind never). Kept 90 days (a weekly rhythm needs > 4 repeats). Prices stored only
  on change; a line chart was rejected (user: they change too rarely), the page shows the delta instead.
- Page style = ../trakk's Warm Editorial dark tokens + fonts, dark only (user choice).

## LLM (internal/predict, why)

- User's hard rule: no pattern → say so, never invent one. The model only proposes; code gates and judges:
  - no call below ≥ 14 d observed, ≥ 4 arrivals, < 80 % of the time in stock (rare and always-there aren't sent);
  - a slot must catch ≥ half the observed weeks/days, an interval sit within 20 % of the measured median gap with
    regular gaps; evidence must cite real arrivals. Else "none".
- Asked only on a change (user): a departure newer than the last call saw (failed calls too), ≤ 1 call/variant/6 h.
  Next window and hit rate are rolled by code; a window polls didn't watch counts neither way.
- GLM 5.3 flash in prod (z.ai). Evals run on MiMo flash only (user); never another model, never a paid run without
  the user's explicit yes per run.
- "Almost" (new, just above a wish) is arithmetic, no LLM: the model would only restate the numbers.
- Digest figures are checked against the data; one unknown number → the plain list is sent instead.

## Push logic (why)

- State holds pushed (fqn, dc) pairs, not the last snapshot: a wish added later must fire for stock already there.
- No decision before the first catalog: price/CPU wishes would fail, drop their pairs, and re-push once it loads.
- Sold out sends nothing (user choice).
- Start test push like ../trakk (user), channels without `!`. Per channel quiet 10 min (restart loop); failed never.

## Repo

- Commit subjects + PR titles: Go style `area: what` (e.g. `watch: drop sold-out pairs`), gated by ship; release
  notes group by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- Name: OVH restock, `ovh-restock` (ovh-availability-check until 0.2.1). README screenshot: live stock + seeded
  sample history, said so in the README.

## User choices

- Watched DCs: France (rbx, sbg, gra, Paris 3-AZ) + Poland (waw); `fra` explicitly excluded.
- Page read-only, no login, no test-push button (would let anyone with the URL push); test via `-notify-test`.
