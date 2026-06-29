---
title: Planning & scheduling
layout: default
parent: Features
nav_order: 1
description: "Multilevel scheduling driven by just two inputs — the cycle and capacity. Levels, the bump rule, automatic promotion, the two capacity limits, and forecasting."
---

# Planning & scheduling
{: .no_toc }

A multilevel schedule driven by only two inputs — the cycle and the medium's capacity — with no per-DLE balancing knobs to tune.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Two inputs, no knobs

NBackup plans backups on a **multilevel** scheme (levels 0–9): level 0 is a full,
levels 1–9 are progressively smaller incrementals. The whole schedule is driven by
just two numbers you already set:

- **`cycle`** — the target and hard-maximum time between fulls of each DLE.
- the landing medium's **`capacity`** — the space NBackup may use there.

There are no balancing knobs, no per-DLE schedules, no "full on Sunday" calendar.
The planner derives everything from the cycle and capacity, re-deciding on every
run from fresh size estimates.

## What each run decides

Every run makes three decisions, in order.

### 1. Estimate

The run sizes every DLE first — its full size and its incremental at the current
level and the next — by running the dump method against `/dev/null`. GNU tar walks
the file metadata without reading file bodies, so the estimate is fast yet honors
your excludes, `one-file-system`, and the incremental snapshot exactly. Sizes are
**uncompressed** — an upper bound on the bytes that will land.

`nb status` shows this as an `estimating` phase while it runs, so a long initial
sizing pass on a large source reads as "underway", not "nothing happening".

### 2. Pick a level

Per DLE, in priority order:

- **Never fulled** → a mandatory level 0.
- **At or past the cycle deadline** → a forced level 0. The cycle is a *hard*
  ceiling: a full never ages past it, so a full is either due or it isn't.
- **Otherwise** → an incremental, governed by the bump rule below.

#### The bump rule

A DLE does **not** climb a level every run. After a full it sits at **level 1**,
re-dumping everything changed since the full. It climbs to a deeper level only when
*both* hold:

- it has held the current level for a couple of runs, **and**
- climbing would save at least **`bump_percent`** of the full size (default 5%).

The savings from climbing shrink as levels deepen, so in practice **level 1 is the
common case** and deep levels are earned by real savings. The payoff: restore
chains stay short, and consecutive same-level incrementals **overlap** — losing one
doesn't break the chain.

```yaml
cycle: 7d
bump_percent: 5      # climb a level only when it saves >= 5% of the full size
```

### 3. Promote to balance

Promotion is the **only** balancing lever, and it is automatic — there is no knob.
The planner builds a **deadline calendar** of every DLE's upcoming full, finds the
heaviest future day, and pulls one of that day's fulls forward onto a lighter run.
It promotes a DLE onto today only while all three guards hold:

1. today is **lighter** than that future peak;
2. the move **strictly lowers** the peak — so a *lone* big DLE is never re-fulled
   early, since moving it would just relocate the same peak; and
3. it **fits** the per-run room left (see [Two capacity limits](#two-capacity-limits)).

With no free capacity, promotion does nothing; with room to spare, it keeps backups
fresh by spreading fulls out.

This is what **de-clumps the cold start**: day one fulls everything (recoverability
first), and promotion staggers that lock-step apart over the next cycle or two, so
later runs don't all come due together.

## Two capacity limits

Capacity is the one number you give a medium, and it binds at two scopes:

| Scope | What must fit | How it's bounded |
|---|---|---|
| **Per run** | A single run's peak size. | **Promotion** is capped at the room left before pruning would evict a *protected* slot (`capacity − protected set`), tightened further by the landing volume's free space. No room → no promotion. A run can still be lumpy when a big DLE hits its *own* deadline — that full is mandatory, not promoted. |
| **Per cycle** | A **complete recovery set**: one full of every DLE. They coexist while `minimum_age ≥ cycle`, so `Σ full_est` must fit capacity. | Structural — no scheduling can change the cycle's fixed full demand. |

When `Σ full_est` exceeds capacity, the plan carries a **warning**: recoverability
is at risk, but **backups still run**. It is the signal to grow capacity or
lengthen the cycle, rather than silently pruning the oldest recovery points away.
The priority order is immovable — recoverability and cycle safety come first;
capacity only bounds *balancing*.

## Forecasting

Two commands let you see the schedule before it happens.

**`nb plan --days N`** projects the planner forward over N daily runs, advancing a
*copy* of the history after each simulated run — so you see when each DLE's next
full lands and how its incrementals bump in between, not just today's decision
repeated. Estimates and the capacity ceiling are sampled once and held constant
(a *level-schedule* forecast, not a capacity timeline); nothing is written.

**`nb dump --dry-run [--date <day>]`** is the single-run dry run: it plans the run
for `--date` against the current catalog — the exact decision a real `nb dump
--date <day>` would make — and prints it without writing anything.

```bash
nb plan                              # today's plan: levels, capacity usage, cost
nb plan --days 30                    # forecast the next 30 daily runs
nb dump --dry-run                    # plan today's run; write nothing
nb dump --dry-run --date 2026-07-15  # plan a specific future day
```

The planner consumes only **bytes** — it never knows whether the landing medium is
a disk, a tape library, or an object store.

---

Next: [Cost forecasting](cost) turns these byte forecasts into `$/month` for cloud
media; [Pruning & retention](pruning) covers how slots are reclaimed to stay within
capacity; [Storage media](media) covers where slots land.
