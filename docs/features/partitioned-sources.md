---
title: Partitioned sources
layout: default
parent: Features
nav_order: 12
description: "One source, many DLEs: partition a directory per child (with a guaranteed remainder) or select children by wildcard — resolved live at plan time, so new directories are picked up with no config edit."
---

# Partitioned sources

One configured source can become many DLEs, resolved **live at every plan**: point
NBackup at `/data` once and each child directory is scheduled, leveled, and restored as
its own DLE — including children that appear next month, with no config edit.

```yaml
sources:
  default:
    fileserver:
      - /var/log                 # a plain source: one DLE (unchanged)
      - path: /data              # PARTITION: one DLE per child dir
        partition: "*"           #   + "the rest of /data" — guaranteed full coverage
      - /srv/web-*               # SELECTION: one DLE per match — and nothing else
```

The rule to remember: **the rest exists exactly when you name a base.** The mapping form
names `path: /data`, so all of `/data` is covered — the matches plus a remainder DLE
("the rest") holding loose files and anything unmatched. A wildcard written directly in
the path is a *selection*: a dynamic list of exactly the matches, no remainder — like a
hand-written list, it does not warn about what it omits.

`nb plan` makes the split legible, with the remainder as a labeled row and an explicit
coverage line (a selection shows neither — the absence is the cue):

```
fs:/data — partitioned
  ├─ alice          L0 (full)   ~1.2 GB
  ├─ bob            L1 (incr)   ~80 MB
  └─ the rest       L1 (incr)   ~40 MB
  ✓ covers 100% of fs:/data (2 matched + the rest)
```

## Semantics worth knowing

- **Sources are directories, always.** Only child *directories* become DLEs; a matching
  file falls to the rest (partition) or is not a DLE (selection). There is no file DLE.
- **`*` matches one path segment** and — unlike a shell — **matches dot-directories**
  (for a backup tool, over-matching is the safe direction). There is no `**`; depth is
  fixed by the pattern (`*` children, `*/www` grandchildren). The base must be a
  literal, non-root path.
- **A new child is never uncovered.** The run it appears, the rest still contains it;
  the next plan graduates it to its own DLE (a mandatory first full) and re-baselines
  the rest once so the stale copy ages out. A deleted child simply stops being resolved
  and its archives age out under retention — no action, no error.
- **Resolution is live and fails loud.** `nb plan`, `nb dump`, and `nb check` expand
  patterns over the source host (a `find` per source); if the listing cannot run the
  command fails rather than guessing. Everything retrospective — status, report,
  recover, the web UI — reads the catalog and never touches the source host.
- **Children are first-class everywhere.** Each records its own catalog identity
  (`fileserver-data-alice`), restores independently, is tracked for staleness, is owed
  to its dumptype's landing route (and backfilled by `nb sync`), and `nb reset` accepts
  its name. The plan records each run's resolved set, so a child whose dumps start
  failing flags loud while a deliberately deleted one retires silently.
- **Duplicate identities are refused.** Two sources resolving to the same DLE (an
  explicit `/data/alice` beside a partition of `/data`, or nested partition bases) fail
  the plan with both origins named — one name means one incremental chain.

## Excludes and partitions

Excludes are **relative to the source** (Amanda semantics): a bare pattern (`*.log`)
matches at any depth; a `./`-prefixed pattern (`./var/cache`) anchors at the source
root; an absolute path is rejected at config load. Under a partition, anchored excludes
anchor at the **base you wrote** — NBackup maps each onto the derived DLE that owns it —
so partitioning never changes which bytes are excluded. An anchored exclude that covers
a whole child simply removes that child's DLE (the rest still excludes it).

Adding an anchored (`./`) exclude re-baselines the owning DLE with one fresh full — its
old chain still holds the now-excluded subtree, and GNU tar treats "newly excluded" as
"present, not dumped", never as deleted. Editing bare globs never forces a full.

## When not to partition

Partitioning divides a **tree**. It does not help one flat directory of millions of
files (every slice still walks the whole directory) — that workload wants a different
archiver. The `postgres` archiver is cluster-granular (`pg_basebackup` cannot dump one
database), so it takes plain sources only; `pipe` sources are opaque tokens and never
expand. Patterns are a `gnutar` (tree) feature.
