---
title: Full 3-2-1-1-0 deployment
layout: default
parent: Scenarios
nav_order: 8
description: "A complete deployment satisfying 3-2-1-1-0: three copies, two media, one offsite, one immutable, zero errors — with drills and alerting."
---

# Full 3-2-1-1-0 deployment
{: .no_toc }

The capstone deployment: three copies across two media types, one offsite, one immutable, and drilled to zero errors — all from one config and one cron line.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

This is the everything-on scenario — the one you grow into once a single offsite
copy isn't enough. It ties the whole feature set together to satisfy the
**3-2-1-1-0** rule, the modern hardening of 3-2-1:

| Digit | Rule | How this config satisfies it |
|-------|------|------------------------------|
| **3** | three copies | landing `disk` + `offsite` cloud + `deep-archive` second tier |
| **2** | two media types | local disk + object store (add `tape` for a third) |
| **1** | one offsite | the `offsite` cloud bucket |
| **1** | one immutable | S3 Object Lock on `offsite`, **detected** by the WORM probe |
| **0** | zero errors | `nb drill` actually restores, runs exit non-zero on failure, `notify` pages you |

See [Verification & drills](../features/verification) and the [Rationale](../rationale)
for why each digit earns its place.

## Config

Save this as `nbackup.yaml`. Three media, two replication rules, encryption, and
alerting.

```yaml
cycle: 7d

compress:
  scheme: zstd
  level: 3

# Config-wide encryption: every archive is piped through gpg to a public-key
# recipient after compression. The KEY is never stored — gpg finds the private key
# in the restoring host's keyring. Lose the key and the data is unrecoverable.
encrypt:
  scheme: gpg
  recipient: backups@example.com

media:
  # Copy 1 — fast local landing, kept deliberately lean so old runs leave disk on
  # disk's own budget.
  disk:
    type: disk
    path: /var/lib/nbackup/disk
    capacity: 2TB

  # Copy 2 — offsite object store. Immutability is configured operator-side via S3
  # Object Lock; NBackup only DETECTS it (see the WORM probe below).
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
    throughput: 50MB/s

  # Copy 3 — a second, cheaper offsite tier for bulk retention.
  deep-archive:
    type: cloud
    url: s3://company-backups-archive?region=eu-north-1
    capacity: 200TB
    throughput: 50MB/s

# Runs are created on disk first.
landing: disk

# Replication: mirror disk -> offsite, then chain offsite -> deep-archive. The
# second rule's source is offsite, not the landing.
sync:
  - to: offsite
  - from: offsite
    to: deep-archive

# Alerting. A literal password:/token: key is rejected (not a config field), so the
# SMTP password is given by env-var NAME (password_env), resolved at send time. The
# Slack webhook URL is secret too, so it uses url_env (a literal url: is also accepted).
notify:
  on_failure: [email, slack]
  digest: [email]
  backends:
    email:
      type: smtp
      host: smtp.example.com
      from: nbackup@example.com
      to: [ops@example.com]
      password_env: SMTP_PASS
    slack:
      type: webhook
      url_env: SLACK_WEBHOOK_URL

archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"

dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /var/log]
```

A third *media type* — robotic tape — fits straight in. Add an `lto` medium
(`type: tape`) and a `sync` rule targeting it; LTO WORM is detected by the same
probe as Object Lock. See [Media types](../features/media).

## Commands

```bash
# Hands-off cron line: dump, push both tiers, trim to budget, prove a restore, mail the digest.
# `nb prune` (no medium) trims each medium to its own budget after the copies land;
# tape recycles by relabel, so a fleet-wide prune only reclaims disk/cloud.
nb dump && nb sync && nb prune && nb drill --unattended; nb report --notify

# Routine offsite check — the no-write structural tier limits egress.
nb drill --from offsite --tier structural

# Read the history and recovery-health audit on demand.
nb report
```

`nb sync` with no `--to` runs **both** rules in order: disk → offsite, then
offsite → deep-archive.

## How each digit is satisfied

- **3 copies** — `disk` (landing), `offsite`, and `deep-archive`. The two `sync`
  rules fan a run out to all three.
- **2 media types** — local disk and an object store. Add an `lto` tape medium for
  a third, genuinely different medium.
- **1 offsite** — the `offsite` cloud bucket (and `deep-archive` beyond it).
- **1 immutable** — S3 Object Lock on `offsite`. NBackup's **WORM probe** keeps one
  fixed probe object and checks that deleting it is *refused*; you configure Object
  Lock operator-side, NBackup only detects it. The drill's 3-2-1-1-0 posture audit
  reports the result.
- **0 errors** — `nb drill` actually restores a risk-biased sample (decrypt +
  decompress + tar), classifies any failure, and **exits non-zero**, so cron and
  `notify` turn a broken backup into a page rather than silence.

## What to watch

- **Each medium prunes on its own terms.** A run leaves `disk` when *disk's*
  capacity and cycle say so — never merely because a copy reached `offsite` or
  `deep-archive`. That independence is what keeps the three copies genuinely
  separate. See [Pruning](../features/pruning) and [Replication](../features/replication).
- **Routine offsite drills use the structural tier.** An encrypted+compressed
  archive is all-or-nothing to read, so a drill costs the full bytes. Drill the
  no-write `structural` tier from `offsite` to limit egress, and watch the forecast
  egress `$` the dry-run prints. See [Cost forecasting](../features/cost).
- **Lose the encryption key and the data is unrecoverable.** NBackup holds no copy
  of the key by design. Make sure the private key is backed up *out of band*. See
  [Encryption](../features/encryption).

See [Monitoring & reporting](../features/monitoring) for the alerting and
run-history side of this deployment.
