---
title: Monitoring & reporting
layout: default
parent: Features
nav_order: 12
description: "Watch a live run with nb status, review history with nb report, and push failures to a human with pluggable alerts."
---

# Monitoring & reporting
{: .no_toc }

Watch a live run with `nb status`, review history with `nb report`, and push failures to a human with pluggable alerts.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Watching a live run

A long `nb dump` (run detached, e.g. from cron) reports progress to a status file
in the catalog workdir. From any other shell, `nb status` reads it and prints an
at-a-glance report:

```text
Run run-2026-06-21.020000  [running]
  started:  2026-06-21 02:00:03  (elapsed 4m12s)
  workers:  2 configured, 2 active
  dles:     1 done, 2 active, 1 pending

DLE            LEVEL  STATE    DUMP               FLUSH   EST        DUMPED     LANDED
app01:/etc     L1     done     [##########] 100%  direct  120.00 kB  120.00 kB  41.00 kB
app01:/home    L0     dumping  [####......]  42%   -       20.00 GB   8.40 GB    2.90 GB
db01:/pg       L0     dumping  [##........]  18%   -       20.00 GB   3.60 GB    1.20 GB
app01:/var     L1     pending  -                  -       80.00 kB   0 B        0 B

Dump:     12.12 GB of ~62.12 GB  (20%)   48.10 MB/s
Volume:   4.11 GB written
ETA:      17m18s
```

The **DUMP** bar meters each DLE's source→volume progress — **uncompressed bytes
against the planner estimate** — with `EST` the estimate, `DUMPED` the uncompressed
source read so far, and `LANDED` what has landed authoritatively. The run streams
source → compressor → volume in one pass, so there is a single `dumping` state per
DLE — no separate dumper/taper queues — and **FLUSH** reads `direct` for that
one-pass path. With a [holding disk](holding-disk) FLUSH becomes a second bar
metering the drain from the holding disk to the landing, and the bottom gains a
`Flush:` line for the aggregate backlog. A run landing on a **removable medium**
(tape) adds a **VOLUME** column naming the volume(s) — the tape label(s) — each
DLE's data reached; a disk or cloud landing is its own sole volume, so the column
is dropped.

A run opens in an `estimating` phase while it sizes every DLE (a pass that can
take a while on a large source), so `nb status` shows the dump is underway rather
than nothing at all:

```text
Run estimate  [estimating]
  started:  2026-06-21 02:00:01  (elapsed 0m38s)
  sizing:   2 of 4 DLEs measured
  estimate: ~22.1 GB so far
```

It then switches to the dumping view above.

```bash
nb status              # the running (or most recent) run
nb status --watch 2s   # refresh until the run finishes
```

`nb status --watch 2s` refreshes until the run finishes; afterwards `nb status`
shows the last run's final result. Reading the status file needs no engine, so
it's cheap to poll.

## Reviewing history with `nb report`

`nb status` shows one live run; a hands-off install also wants the *history* and a
way to be told when something breaks. **Every mutating command** (`dump`, `sync`,
`prune`, `verify`, `drill`) records a machine-readable summary to the catalog
workdir — appended to `run-log.jsonl` and mirrored as `run-summary.json` (scrape
it from a monitoring system) — and **exits non-zero on failure**.

`nb report` summarizes the recent history — what ran, what failed, bytes moved —
plus a **recovery-health audit** that flags any DLE whose drills are failing,
*degrading* (passed before, failing now), stale, or never run:

```text
NBackup report — 3 run(s) from 2026-06-23 02:00 to 2026-06-23 02:25
1 run(s) FAILED, 10.74 GB moved

WHEN              COMMAND  OUTCOME  DETAIL
2026-06-23 02:25  drill    FAILED   1 failure(s), 1 overdue
2026-06-23 02:13  sync     OK       1 run(s) copied, 5.37 GB
2026-06-23 02:00  dump     OK       run-2026-06-23, 3 archive(s), 5.37 GB

FAILURES
  2026-06-23 02:25 drill [drill-failures]: 1 drill failure(s) — recovery is at risk

DRILL COVERAGE
  FAILING DLE  CLASS     LAST DRILL        REMEDY
  app01:/home  pipeline  2026-06-23 02:25  the archive would not decrypt/decompress/untar — …
  stale (overdue past 30d): app01:/etc (84d ago)
```

`nb report --last 30` widens the window; `nb report --json` emits the raw records.

For the classic **dump report**, `nb report --dump` prints the latest dump in
detail: a one-line headline, an overall statistics grid (Total / Full / Incr), and
the per-DLE table — each DLE's level, original/output size, compression %, files,
dump time, and rate:

```text
DUMP REPORT  run-2026-06-24.020000  (run 2026-06-24 02:00)
2 DLE(s) dumped OK · 21.47 GB -> 5.37 GB (25%) · 12m00s elapsed

STATISTICS            Total        Full         Incr
DLEs dumped               2           1            1
Original size      21.47 GB    21.47 GB    122.88 kB
Output size         5.37 GB     5.37 GB     40.96 kB
Avg compression         25%         25%          33%
Files                  1249        1240            9
Dump time (sum)      12m05s      12m04s           1s
Avg dump rate    29.62 MB/s  29.66 MB/s  122.88 kB/s
Run time (wall)      12m00s

DLE          LVL  ORIG       OUT       COMP%  FILES  TIME    RATE
app01:/home  0    21.47 GB   5.37 GB   25%    1240   12m04s  29.66 MB/s
app01:/etc   1    122.88 kB  40.96 kB  33%    9      1s      122.88 kB/s
```

Dump time is the *sum* of per-DLE dump times (it exceeds the wall-clock run time
when workers run in parallel); run time is the single wall-clock span.

`nb report --dump --run run-2026-06-21.020000` reports a specific dump.

## Alerting (notify)

To push failures to a human, add a `notify:` block. Backends are pluggable —
built-in **email (SMTP)** and a generic **webhook** (Slack/Discord/PagerDuty-compatible):

```yaml
notify:
  on_failure: [email, slack]   # omit to alert every backend
  # on_success: [email]        # see below — dump already notifies on success
  digest: [email]              # for `nb report --notify`
  backends:
    email:
      type: smtp
      host: smtp.example.com
      from: nbackup@example.com
      to: [ops@example.com]
      password_env: SMTP_PASS        # env var name — never the secret itself
    slack:
      type: webhook
      url_env: SLACK_WEBHOOK_URL
```

**What notifies, when:**

- **Any command alerts on failure** by default (every backend, unless `on_failure`
  narrows it).
- A successful **`nb dump`** also notifies by default — the nightly "backups
  happened" signal, so a silent inbox means cron didn't run, not that all is well.
  The dump notification carries the **full per-DLE dump report** (the
  `nb report --dump` table), so the nightly email *is* the report.
- Other commands' success is **opt-in**: list backends in `on_success` for
  `sync` / `verify` / `drill` / `prune` (that list then applies to dump too).

A literal `password:`/`token:` key is **rejected** (neither is a config field), so
an SMTP password is given by the **name** of an environment variable (`password_env`)
and resolved at send time — credentials never sit in the config. A webhook URL may be
a literal `url:` *or*, when the URL is itself a secret (Slack/Discord bear the
credential in the URL), the name of an environment variable (`url_env`, preferred). A
notification failure (unreachable mail server, missing secret, hung endpoint) is
**only ever a stderr warning**: it never fails or blocks the backup.

## The dead-man's switch (healthcheck)

Failure alerting has a blind spot: it only fires when `nb` actually runs. If
cron itself stops firing — a bad crontab edit, a full disk, a powered-off
machine — nothing fails, so nothing alerts. A `healthcheck` backend closes that
hole with [healthchecks.io](https://healthchecks.io)-style pings (any
compatible self-hosted service works):

```yaml
notify:
  backends:
    hc:
      type: healthcheck
      url_env: HEALTHCHECKS_URL
```

Every covered run pings `<url>/start` when it begins and `<url>` on success or
`<url>/fail` on failure. The monitoring service alarms when the pings *stop
arriving* — which is exactly the signal `on_failure` routing can never carry,
so a healthcheck backend deliberately ignores `on_failure`/`on_success` and
fires on every run. A `command` backend is the escape hatch for everything
else: it execs your script (no shell) with `NB_COMMAND`, `NB_STATUS`, and
`NB_SUBJECT` in the environment and the rendered report on stdin.

## Staleness: "is anything falling behind?"

An alert on failed runs still misses the DLE that quietly stopped being backed
up (removed from cron's reach, host renamed, forever-skipped). There is no
separate config for this: [`cycle`](../reference/configuration#cycle) is
already NBackup's freshness contract — "a full never ages past one cycle" — so
`nb check` enforces it directly, with no key to set and nothing to get wrong.

`nb check` **fails** for any configured DLE whose newest backup (any level —
an incremental counts) is older than one cycle; a DLE that has **never** been
backed up is a **warning** instead, so a fresh install isn't red before its
first dump ever runs. `nb report` and the nightly digest list the same
offenders with their last-backup age.

## Prometheus metrics (`nb web`)

If you already run Prometheus, [`nb web`](web) exposes a **`/metrics`** endpoint on
its port (default `:8080`) in the standard text exposition format — point-in-time
gauges read from the catalog on each scrape, so there is no daemon, no registry, and
no extra dependency. Scrape it like any other target:

```yaml
scrape_configs:
  - job_name: nbackup
    static_configs:
      - targets: ["backup-host:8080"]
```

Every metric is a gauge, prefixed `nbackup_`, with timestamps as unix seconds:

| Metric | Labels | Meaning |
|---|---|---|
| `nbackup_last_run_success` | `command` | 1 if the most recent run of that command succeeded, 0 if it failed |
| `nbackup_last_run_timestamp_seconds` | `command` | when that run finished |
| `nbackup_last_run_duration_seconds` | `command` | how long it took |
| `nbackup_dle_last_backup_timestamp_seconds` | `dle` | each DLE's newest archive (absent if never backed up) |
| `nbackup_dle_count` | — | configured DLEs |
| `nbackup_dle_stale_count` | — | DLEs overdue against one dump cycle (never backed up, or older than one cycle) |
| `nbackup_drill_overdue_count` | — | DLEs not covered by a passing drill within the drill window |
| `nbackup_drill_failing_count` | — | DLEs whose most recent drill failed |
| `nbackup_medium_used_bytes` | `medium` | bytes stored on a medium |
| `nbackup_medium_capacity_bytes` | `medium` | a bounded medium's capacity (absent for an unbounded medium) |

A backup that "silently stopped" shows up three ways: `nbackup_last_run_success` goes
to 0, `time() - nbackup_dle_last_backup_timestamp_seconds` climbs, and
`nbackup_dle_stale_count` rises — any of which makes a clean alerting rule. The
endpoint is read-only and never errors, even on an empty catalog.

## A hands-off cron line

```sh
nb dump && nb sync && nb prune && nb drill --unattended; nb report --notify
```

This dumps, replicates offsite, trims each medium to its cycle/capacity budget,
rehearses a recovery, and mails the nightly digest — every step recording its run
summary and alerting on failure. `nb prune` with no medium named prunes every
configured medium against its own retention, and runs after `nb sync` so nothing
is reclaimed before it is replicated. Add `nb check` and, with a `healthcheck`
backend configured, this one line also proves liveness (the pings) and
freshness (`nb check` fails while any DLE is overdue against the cycle).

---

See also [Verification & drills](verification) for what a drill proves,
[Planning](planning) for what each run decides, and the
[Full 3-2-1-1-0](../scenarios/full-321) scenario for the whole unattended
pipeline end to end.
