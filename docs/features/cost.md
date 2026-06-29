---
title: Cost forecasting
layout: default
parent: Features
nav_order: 2
description: "Reason about cloud backups in dollars per month, not bytes — a fully offline storage + egress estimate. No billing API."
---

# Cost forecasting
{: .no_toc }

Reason about cloud backups in dollars per month, not bytes — a fully offline storage and egress estimate, with no billing API.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Dollars, not bytes

When backups land in the cloud, the question you actually care about isn't "how
many bytes" — it's "what will the bill be". NBackup already accounts bytes
precisely (estimates, forecasts, capacity), so it layers a thin dollar estimate on
top.

`nb plan` prints, for each priced medium:

- the current footprint's **storage `$/month`**, and
- the **marginal `$/month`** the next run adds.

`nb plan --days N` adds a **`$/MONTH` column** to the forecast, projecting the cost
curve as fulls land and pruning reclaims — so you can see the bill rise and settle
across the cycle rather than guess from a single day.

```bash
nb plan              # current storage $/month + the next run's marginal $/month
nb plan --days 30    # the $/month curve as fulls land and pruning reclaims
```

## A medium prices itself

Pricing works like sizing: a medium prices itself. With **no configuration**, a
cloud bucket infers its provider from the URL scheme:

| URL scheme | Provider |
|---|---|
| `s3://` | AWS (and a generic table for S3-compatible stores) |
| `gs://` | Google Cloud Storage |
| `azblob://` | Azure Blob |

So `nb plan` shows a monthly bill out of the box. **Local disk and tape have no
recurring bill** and show no cost line at all — they are unpriced, just as an
unbounded medium reports no capacity ceiling.

### Overriding a rate

An optional per-medium `cost:` block overrides individual rates (a region's egress,
an S3-compatible provider's pricing) or names a different provider table:

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
    cost:
      provider: aws-s3             # base rate table (default: inferred from the url)
      storage_per_gb_month: 0.021  # recurring $/GiB-month
      egress_per_gb: 0.05          # $/GiB transferred out (read off the medium)
      get_per_1000: 0.0004         # $ per 1000 read requests
```

## Egress, surfaced where it bites

The bill's real surprise is usually **egress** — the charge to read data *back out*
of a cloud store. NBackup prices it at the moment you would incur it:

- **`nb recover`** estimates the **egress `$`** before pulling a chain from a cloud
  store, and warns — interactively confirming — when the amount is material. A
  scripted or cron read prints the estimate and proceeds (it never blocks).
- an **offsite `nb drill`** spends the full bytes (an encrypted, compressed archive
  is all-or-nothing to read), so its dry-run **forecast egress carries a `$`** —
  the figure to watch when scheduling routine offsite drills.

See [Verification & drills](verification) for why an offsite drill defaults to the
no-write `structural` tier to keep that egress small.

## What pricing is — and isn't

Two deliberate boundaries keep the estimate honest:

**It is a flat estimate.** Pricing covers storage + egress + request charges. It
does **not** model storage-class lifecycle tiers (Glacier, Deep Archive). Which
tier your bytes physically sit in is something you configure **bucket-side**, on
the provider — so a forecast of it would be wrong more often than useful (it would
need per-class rates, retrieval fees and latency, minimum-retention floors, and an
age-to-class schedule). A flat rate is the honest estimate NBackup can actually
stand behind.

**It is fully offline.** The figure is a pure calculation over the catalog and a
built-in rate table — there is **no billing API call**. So it runs wherever
planning runs and never touches a slow or offline volume. The rate tables are
list-price estimates; your **provider's invoice stays authoritative**.

---

Next: [Planning & scheduling](planning) produces the byte forecast this prices;
[Storage media](media) covers configuring a cloud bucket; [Verification &
drills](verification) covers the egress an offsite drill spends.
