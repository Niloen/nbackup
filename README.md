# NBackup

A modern, slot-based backup system inspired by Amanda. NBackup preserves
Amanda's strongest operational properties — balanced scheduling, immutable daily
artifacts, human-readable contents, tape-cycle safety — while producing portable
backup artifacts you can restore with standard tools.

> A backup administrator should be able to reason about backups by looking at a
> sequence of immutable daily backup slots rather than a database of chunks.

This is a first version. See [PRD.md](PRD.md) for the full product vision.

## Core ideas

- **Slot** — the primary artifact. One run produces exactly one slot, an
  immutable directory you can copy, inspect, and understand without NBackup.
- **DLE** — a backup source (`host` + `path`).
- **Run** — one planner execution, typically daily.
- **Cycle** — a safety boundary controlling when slots may be deleted.
- **Media** — where slot copies live (local disk today; S3/tape planned).

A slot looks exactly like the PRD describes:

```text
slot-2026-06-21/
  SLOT.json          # slot metadata; written last, which "seals" the slot
  MANIFEST.json      # per-archive file listings
  CHECKSUMS.sha256   # sha256 of every archive (sha256sum-compatible)
  archives/
    app01-home-L0.tar.zst
    db01-pg-L1.tar.zst
```

Archives are produced by **GNU tar** (the same engine Amanda uses) in
listed-incremental format, compressed with zstd. Recovery never requires
NBackup — a full (L0) is an ordinary tar, and the whole chain restores with
stock GNU tar:

```bash
# single full archive:
zstd -dc app01-home-L0.tar.zst | tar -xf -

# a full + incrementals, replayed in order (applies deletions):
for a in slot-*/archives/app01-home-L*.tar.zst; do
  zstd -dc "$a" | tar --extract --listed-incremental=/dev/null
done
```

> Requires **GNU tar** at runtime (`tar` on Linux, `gtar` on macOS/BSD). The
> `zstd` CLI is only needed for the manual restore above — NBackup itself
> compresses/decompresses in process.

## Install

```bash
make build          # builds all binaries into ./bin
# or
go install ./cmd/...
```

This produces the `nb` umbrella tool plus standalone commands:

| Tool        | `nb` alias   | Purpose                                  |
|-------------|--------------|------------------------------------------|
| `nbdump`    | `nb dump`    | Execute a run and seal a slot            |
| `nbplan`    | `nb plan`    | Show what the next run would do          |
| `nbslot`    | `nb slot`    | List / show / prune slots                |
| `nbverify`  | `nb verify`  | Verify slot checksums                    |
| `nbrestore` | `nb restore` | Restore a DLE from a slot                |
| `nbcatalog` | `nb catalog` | Maintain the local slot-index cache      |

## Quick start

```bash
cp nbackup.example.yaml nbackup.yaml   # edit sources + catalog path

nb plan                # preview today's plan and budget usage
nb dump                # run the backup, producing one sealed slot
nb slot                # list slots
nb slot show slot-2026-06-21
nb verify              # re-check all archive checksums
nb restore -dle app01-home -dest /tmp/out slot-2026-06-21
```

Every command accepts `-c <config>` and `-C <catalog>` overrides.

## How it works

### Planning (balanced, multilevel scheduling)

NBackup uses Amanda's **multilevel** scheme (levels 0–9) with a dynamic,
estimate-driven schedule. Each run:

1. **Estimates** every DLE's full and next-incremental size. By default it uses
   the last recorded sizes (cheap, accurate for stable data); a dumptype with
   `estimate: exact` runs a live `tar` estimate each run.
2. Sets a base decision per DLE: never-fulled → **mandatory L0**; past the cycle
   deadline (≈ 2× interval) → **forced L0**; due (≥ interval) → **L0**;
   otherwise the **next incremental level** (capped at L9).
3. **Degrades** to balance: while the run exceeds the capacity ceiling (hard,
   priority #3) or the balance target `Σ full_est / interval` (soft, #4), it
   demotes the least-urgent non-mandatory due-fulls to incrementals — pushing
   their full to a later day. Mandatory fulls are never touched (so one big DLE
   on its day may still be large — that's fine).
4. Optionally **promotes** (off by default): pulls soonest-due future fulls
   forward to fill a light run, bounded by once-per-interval **and** a capacity
   headroom so it never spends storage past the limit.

This encodes the PRD priority order directly (recoverability and cycle safety are
immovable; capacity overrides balance; balance never costs storage). It also
**de-clumps the cold start**: day one fulls everything (recoverability first),
and degrade spreads the resulting lock-step over the next cycle or two. The
planner consumes only bytes — it never knows whether the medium is tape or an
object store.

Levels are realized with GNU tar's listed-incremental **snapshot library**, kept
under `<catalog>/snapshots/<dle>/L<n>.snar` — exactly the mechanism Amanda uses
to turn tar's two-level primitive into N-level backups.

### Slot naming and multiple runs per day

The first run of a day is `slot-YYYY-MM-DD`. Run again the same day and you get
`slot-YYYY-MM-DD.2`, `.3`, … Each slot stays immutable; a sealed date is never
overwritten. Restores and pruning order slots by date **then** sequence.

### Sealing

A run writes archives, then `MANIFEST.json` and `CHECKSUMS.sha256`, verifies the
checksums against what landed on disk, and finally writes `SLOT.json` with
`status: sealed`. After sealing, a slot is treated as immutable — re-running a
sealed date is refused.

### Restore

Restoring a DLE as of a slot replays its most recent full at or before that
slot, then every later incremental up to it, in run order, with GNU tar's
incremental extraction. Because the incrementals carry directory census data,
**deletions are applied** — a file removed between the full and the chosen slot
is absent after restore. You can restore a single DLE (`-dle`) or all DLEs in
the slot.

### Pruning (cycle safety)

`nb slot prune` is a dry-run by default. Pruning has two layers:

1. **Safety floor** (shared, medium-agnostic): a slot is *protected* if it is
   younger than the medium's `minimum_age`, or if any DLE it holds has no newer
   full elsewhere (its last recovery path). Protected slots are never reclaimed.
2. **Capacity reclamation** (per-medium): among non-protected slots, the medium's
   retention strategy reclaims to fit capacity. For object stores this deletes
   the **oldest slots until total ≤ budget**; for tape it will reclaim whole
   tapes (not yet implemented).

Add `--apply` to actually delete. Capacity and `minimum_age` are per-medium, so
each store is pruned against its own limits.

## Configuration

See [`nbackup.example.yaml`](nbackup.example.yaml). Minimal example:

```yaml
cycle:
  require_verified_successor: true   # cross-cutting safety

# Named storage definitions; `landing` selects which one slots are created on.
# Budget and minimum_age are per-medium (each store has its own capacity/cycle).
media:
  disk:
    type: local-disk
    path: /var/lib/nbackup/catalog
    budget: 20TB
    minimum_age: 30d
landing: disk

# Named method+option bundles (Amanda's "dumptype"); a DLE selects one.
dumptypes:
  default:
    method: gnutar
    one-file-system: "true"
  no-logs:
    method: gnutar
    exclude: "*.log,*.tmp"

sources:
  - host: app01
    path: /home              # uses the "default" dumptype
  - host: db01
    path: /var/lib/postgresql
    dumptype: no-logs
```

**Media** is a map of named definitions, each with a `type` and type-specific
parameters; `landing` names the one slots are written to. Adding a medium type
is a registry registration — no config struct changes. **Dumptypes** are named
`{method + options}` bundles that DLEs reference, so a DLE associates with a
dump method (the "Application") plus its options (compression, `exclude`,
`one-file-system`, …) without hardcoding.

### Capacity and retention are per-medium

Each medium declares its **capacity** in its own units — object stores spell it
as `budget` (`20TB`); tape spells it as `tapes × tape_size` (`0` = unbounded).
Capacity is the only genuinely per-medium quantity. `minimum_age` (a per-medium
safety floor) and the global `cycle.require_verified_successor` round out
retention.

Balancing dumps over time is **not** a medium property — it's a global, temporal
planning concern (an S3 bucket has no meaningful per-run size). So the planner
spreads fulls across the global cycle on its own (see Planning). Pruning consumes
only capacity; the reclamation difference (delete a slot vs reclaim a whole tape)
lives entirely in the medium's retention strategy.

## Requirements

- **Go 1.25+** to build.
- **GNU tar** at runtime (`tar` on Linux, `gtar` elsewhere; set `gnutar_path` in
  config to override). NBackup checks the binary is GNU tar before running.

## Status & limitations (first version)

Implemented: local-disk landing, balanced **multilevel (L0–L9)** planning with a
GNU tar snapshot library, immutable sealed slots with **sequence-suffixed**
same-day runs, **deletion-aware** incremental restore, checksum verification,
point-in-time restore, budget reporting, cycle-safe pruning.

Not yet implemented (declared in config for forward-compatibility):

- **S3 and tape media** — only `local-disk` landing works today.
- **Budget-driven retention** — budget is reported; pruning is cycle-based, not
  yet automatically driven to fit the budget.
- **Remote sources** — `host` is metadata; `path` is read from the local
  filesystem (run the agent where the data is, or mount it).
- **Exclude/include rules** and tar tuning (one-file-system and sparse are on by
  default, as in Amanda).

## Architecture

NBackup's internals mirror Amanda's pluggable-API structure: mechanism lives
behind interfaces with named, registered implementations, and a single
orchestrator composes them.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | Disklist / dumptype / storage |
| `slot` | slot format: pure data + (de)serialization | Header / amar |
| `media` | `Store`/`Vault` I/O + `Profile` (capacity) + `Retention` (reclamation) + registry | Device API + Policy |
| `media/localdisk`, `media/s3`, `media/tape` | implementations (s3/tape are registered stubs) | tape/s3/vfs devices |
| `method` | `Method` dump interface + registry (configured via dumptype options) | Application API |
| `method/gnutar` | GNU tar implementation (all tar/snapshot specifics) | amgtar |
| `policy` | cross-cutting safety floor (protected slots) | Policy |
| `xfer` | stream pipeline: zstd + checksum + counting | Xfer API |
| `catalog` | local cache of the slot index + snapshot library; derives run `History` | catalog / curinfo / tapelist |
| `policy` | retention/cycle/budget decisions (pure) | Policy |
| `planner` | multilevel level scheduling (pure) | planner |
| `engine` | the driver: wires planner→method→xfer→media→catalog | driver / taper |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, policy, method, media,
catalog, xfer, slot, dle, config}`. Domain packages stay pure; `method`/`media`
are pluggable adapters; `engine` is the only component aware of all of them. A
backup reads as a pipeline — **source** (`method.Backup`) → **filter**
(`xfer` zstd+checksum) → **dest** (`media.Store`) — and adding a storage medium
or dump method is a registry registration, not a conditional in the core.

### The catalog is a cache

Slots on the `media.Store` are the **source of truth**; they are fully
self-describing. The `catalog` is a **local cache** of their index (kept in the
workdir as `catalog.json`), so planning, listing, restore-location, pruning, and
budget reporting never touch the media — which matters when the store is slow or
offline (S3/Glacier/tape). This mirrors Amanda, which never scans tapes to
operate: it keeps `curinfo`/`tapelist`/catalog databases locally and treats the
media as self-describing enough to rebuild them.

Consequences:

- **Run `History` is derived** from the cached slots, not separately persisted —
  so there is no second source to drift. (Each `SLOT.json` records the date and
  per-archive level, which is all the planner needs.)
- The cache is kept in sync **by construction**: `nb dump` adds the sealed slot,
  `nb slot prune` removes deleted ones.
- If the cache is **lost**, it is rebuilt automatically on the next command. For
  out-of-band changes (slots copied/removed directly on the store), run
  `nb catalog rebuild` to reconcile — the one operation that rescans the media.
- The **only** non-derivable local state is the GNU tar snapshot library
  (`snapshots/…/L<n>.snar`). It is precious — losing it forces a new full —
  exactly like Amanda's `gnutar-lists`.

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
```
