# AGENTS.md

OVH dedicated server stock watcher. Go, stdlib + `trick77/toolkit/notify`. No JS, page is `html/template`.

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
- KS-LE plans have no CPU in `invoiceName`.
- Only Kimsufi has a per-server order page: `eco.ovhcloud.com/fr/kimsufi/<name lowercased>/`, all 22 KS names 200,
  unknown names 404. So you Start's `/fr/so-you-start/` 404s.

## Push logic (why)

- State holds pushed (fqn, dc) pairs, not the last snapshot: a wish added later must fire for stock already there.
- No decision before the first catalog: price/CPU wishes would fail, drop their pairs, and re-push once it loads.
- Sold out sends nothing (user choice).

## Repo

- Commit subjects + PR titles: Go style `area: what` (e.g. `watch: drop sold-out pairs`), gated by ship; release
  notes group by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- No README by user choice; deployment is `compose.yaml` + `.env.example` + `config.example.json`.

## User choices

- Watched DCs: France (rbx, sbg, gra, Paris 3-AZ) + Poland (waw); `fra` explicitly excluded.
- Page read-only, no login, no test-push button (would let anyone with the URL push); test via `-notify-test`.
