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
- **Media** — a **Volume** where slots live: local disk, a virtual tape, or (stub)
  S3. Slots stream between volumes (`nb copy`), e.g. disk → tape.

A medium is a **volume**: an ordered sequence of self-describing files (Amanda's
Device API), each carrying an identity **header** (slot, DLE, level, codec, …),
addressed by position (a tape file number). A slot is a run of archive files plus
a final **seal record** (the slot's metadata) that marks it complete. The same
shape maps to disk, an object store, or tape; each medium frames the header its
own way. On **disk** the header is a separate `.hdr` sidecar so the payload stays
a clean archive, and files are human-friendly:

```text
slots/slot-2026-06-21/
  000000-app01-home-L0.tar.zst   # clean compressed tar (payload)
  000000-app01-home-L0.hdr       # JSON header sidecar
  000001-db01-pg-L1.tar.zst
  000001-db01-pg-L1.hdr
  000002-seal.json               # slot metadata (identity + sizes + checksums)
  000002-seal.hdr
```

(On **tape** the header is instead a fixed 32 KB block inline ahead of each
payload, since a tape has no sidecars.)

Archives are produced by **GNU tar** (the same engine Amanda uses) in
listed-incremental format, then piped through an external compressor (`zstd` by
default; `gzip` or `none` also built in). Recovery never requires NBackup — a disk
payload is an ordinary compressed tar, and the whole chain restores with stock
tools:

```bash
# single full archive (the disk payload is a clean tar.zst — no header to skip):
zstd -dc 000000-app01-home-L0.tar.zst | tar -xf -

# a full + incrementals, replayed in order (applies deletions):
for a in slots/slot-*/0*-app01-home-L*.tar.zst; do
  zstd -dc "$a" | tar --extract --listed-incremental=/dev/null
done
# (from tape, skip the 32 KB inline header first: dd bs=32k skip=1 < file | zstd -dc | …)
```

> Requires **GNU tar** and the configured **compressor** (`zstd` by default) at
> runtime. Like Amanda, NBackup orchestrates these as external child processes
> rather than reimplementing them — so the same `zstd`/`tar` used for the manual
> restore above are exactly what produced the archives.

## Install

```bash
make build          # builds ./bin/nb
# or
go install ./cmd/nb
```

This produces a single `nb` binary with these subcommands:

| Command              | Purpose                                                  |
|----------------------|----------------------------------------------------------|
| `nb plan`            | Show what the next run would do                          |
| `nb dump`            | Execute a run and seal a slot                            |
| `nb status`          | Show progress of the current (or most recent) run        |
| `nb slot`            | List slots (default)                                     |
| `nb slot show`       | Show a single slot's archives and copies                |
| `nb slot prune`      | Delete slots past the cycle/capacity limits             |
| `nb verify`          | Verify slot checksums                                    |
| `nb restore`         | Restore a DLE from a slot                                |
| `nb copy`            | Copy a slot to another medium (disk → tape)             |
| `nb label`           | Label a volume (required for tape before its first dump) |
| `nb medium`          | List media (capacity, usage, volume) or detail one       |
| `nb changer list`    | Inventory the bays of a library medium                   |
| `nb changer load`    | Mount a volume into a library's drive                    |
| `nb catalog rebuild` | Rebuild the local slot-index cache from media            |

Run `nb help <command>` (or `nb <command> --help`) for per-command usage and
examples, and `nb completion <shell>` to generate shell completion.

## Quick start

```bash
cp nbackup.example.yaml nbackup.yaml   # edit sources + catalog path

nb plan                # preview today's plan and budget usage
nb dump                # run the backup, producing one sealed slot
nb status              # progress of the running (or most recent) dump
nb slot                # list slots (with a COPIES column: where each lives)
nb slot show slot-2026-06-21   # archives + every copy's volume and file positions
nb medium              # media overview: type, slots, usage / capacity, volume
nb medium lto          # one medium's volume and the slots it holds
nb verify              # re-check all archive checksums
nb restore --dle app01-home --dest /tmp/out slot-2026-06-21
```

These global flags work with every command and may appear anywhere on the
command line — before or after the subcommand and its positional arguments:

| Flag              | Purpose                                  |
|-------------------|------------------------------------------|
| `-c, --config`    | path to config file (default `nbackup.yaml`) |
| `-C, --catalog`   | catalog directory (overrides config)     |
| `-q, --quiet`     | suppress progress output                 |

## How it works

### Planning (balanced, multilevel scheduling)

NBackup uses Amanda's **multilevel** scheme (levels 0–9) with a dynamic,
estimate-driven schedule. Each run:

1. **Estimates** every DLE's full and next-incremental size by running the dump
   method (Amanda's "client" estimate). For GNU tar this is a `tar` pass targeted
   at `/dev/null`: tar walks metadata without reading file bodies, so it is fast
   yet exactly honors excludes, one-file-system, and the incremental snapshot.
   Sizes are uncompressed — an upper bound on the compressed bytes finally stored.
2. Sets a base decision per DLE: never-fulled → **mandatory L0**; past the cycle
   deadline (≈ 2× interval) → **forced L0**; due (≥ interval) → **L0**;
   otherwise the **next incremental level** (capped at L9).
3. **Degrades** to balance: while the run exceeds the per-run capacity ceiling
   (hard, priority #3) or the balance target `Σ full_est / interval` (soft, #4),
   it demotes the least-urgent non-mandatory due-fulls to incrementals — pushing
   their full to a later day. Mandatory fulls are never touched (so one big DLE
   on its day may still be large — that's fine).
4. Optionally **promotes** (off by default): pulls soonest-due future fulls
   forward to fill a light run, bounded by once-per-interval **and** a capacity
   headroom so it never spends storage past the limit.

Capacity binds at **two scopes**, and they are not the same check:

- **Per run** (step 3) bounds a single run's peak to the room left before pruning
  would have to evict a *protected* slot (`capacity − protected set`). Within that
  ceiling a run may be lumpy — taking more than its even share is fine.
- **Per cycle** is structural: over a cycle every DLE is fulled once, and with
  `minimum_age ≥ cycle` those fulls coexist on the medium, so a **complete
  recovery set** (one full of every DLE) must fit capacity. Degrading cannot help
  here — a demoted due-full just climbs to its deadline and is forced full inside
  the same cycle, so the cycle's full demand is fixed. When `Σ full_est` exceeds
  capacity the plan carries a **warning** (recoverability is at risk; backups
  still run) rather than silently pruning the oldest recovery points away.

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

A run writes the archive files, verifies their checksums against what landed on
the volume, and finally appends the **seal record** — one file carrying the slot
metadata (identity, sizes, checksums, member listings) with `status: sealed`. The
seal record is written last, so its presence marks the slot complete; after
sealing, a slot is immutable — re-running a sealed date is refused.

### Monitoring a run

A long `nb dump` (run detached, e.g. from cron) reports progress to a status file
in the catalog workdir as it goes. From any other shell, `nb status` reads that
file and prints an at-a-glance report — like Amanda's `amstatus`:

```text
Run slot-2026-06-21  [running]
  started:  2026-06-21 02:00:03  (elapsed 4m12s)
  dumpers:  2 configured, 2 active
  dles:     1 done, 2 active, 1 pending

DLE          LEVEL  STATE    PROGRESS           DONE       EST        WRITTEN
app01-etc    L1     done     [##########] 100%  120.00 kB  ~118.0 kB  41.00 kB
app01-home   L0     dumping  [####......]  42%  8.40 GB    ~20.0 GB   3.10 GB
db01-pg      L0     dumping  [##........]  18%  3.60 GB    ~20.0 GB   1.40 GB
app01-var    L1     pending  -                  0 B        ~2.0 GB    0 B

Total:    12.12 GB of ~62.12 GB  (20%)
Rate:     48.10 MB/s
ETA:      17m18s
```

Each DLE's percentage is uncompressed bytes against the planner estimate; the run
streams source→compressor→volume in one pass, so there is a single `dumping`
state per DLE (no separate dumper/taper queues). `nb status --watch 2s` refreshes
until the run finishes; afterwards `nb status` shows the last run's final result.

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
  length: 7d                         # dump cycle: time between fulls per DLE
  require_verified_successor: true   # cross-cutting safety

# Named storage definitions; `landing` selects which one slots are created on.
# Budget and minimum_age are per-medium (each store has its own capacity/cycle).
media:
  disk:
    type: disk
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

# The disklist: grouped by dumptype, then host, then paths.
sources:
  default:
    app01: [/home, /etc]
  no-logs:
    db01: [/var/lib/postgresql, /var/log]
```

**Media** is a map of named definitions, each with a `type` and type-specific
parameters; `landing` names the one slots are written to. Adding a medium type
is a registry registration — no config struct changes. **Dumptypes** are named
`{method + options}` bundles (Amanda's dumptype): a dump method (the
"Application") plus its options (compression, `exclude`, `one-file-system`, …).
**Sources** (the disklist) are grouped by dumptype → host → paths, so each DLE
is just a path under the dumptype that governs it — all per-DLE tuning lives in
the dumptype, never on the entry.

### Capacity and retention are per-medium

Each medium declares its **capacity** in its own units — object stores spell it
as `budget` (`20TB`); tape spells it as `bays × volume_size` (`0` = unbounded).
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
- The configured **compressor** on `PATH`: `zstd` (default) or `gzip`; `none`
  needs nothing. NBackup checks it before running. Optional `nice` is used for
  CPU politeness when configured.

## Status & limitations (first version)

Implemented: disk and tape Volumes, **copying slots between media** (`nb copy`,
e.g. disk → tape) with the copy **recorded as a second placement** so restore and
verify use any available copy, balanced **multilevel (L0–L9)** planning with a GNU
tar snapshot library, immutable sealed slots with **sequence-suffixed** same-day
runs, **deletion-aware** incremental restore, checksum verification, point-in-time
restore, per-medium budget reporting, cycle-safe pruning.

The `tape` medium is a **library of tapes behind one drive**: `bays: N` physical
positions, each holding a finite `volume_size` tape, with `dir:` (a file-backed
virtual library, no hardware) or `device:` (a real drive via `mt`). You label a
blank tape (`nb label`), and inventory or mount tapes with `nb changer list` /
`nb changer load`. Tapes carry a self-describing label that NBackup **verifies
before every write**, so a foreign, wrong, or still-active reel is never
clobbered. A tape fills to end-of-tape and is then changed manually; `appendable:
true` (default) packs many runs per tape (Bacula-style), `appendable: false` uses
one run per tape (Amanda-style). Restore auto-mounts whichever tape holds the
copy it needs. (Internals: [ARCHITECTURE.md](ARCHITECTURE.md). Automatic tape
advance and spanning are the next step.)

Not yet implemented (declared in config for forward-compatibility):

- **S3 media** — registered as a Volume stub; no S3 client yet.
- **Budget-driven retention** — budget is reported; pruning is cycle-based, not
  yet automatically driven to fit the budget.
- **Remote sources** — `host` is metadata; `path` is read from the local
  filesystem (run the agent where the data is, or mount it).
- **Exclude/include rules** and tar tuning (one-file-system and sparse are on by
  default, as in Amanda).

## Architecture

NBackup's internals mirror Amanda's pluggable-API structure: mechanism lives
behind interfaces with named, registered implementations, and one orchestrator
(`engine`) composes them. The **media are the source of truth** (every file
self-describing, every slot sealed, every labeled volume identified); the
**catalog is a local cache** with its own directory, so planning, listing,
restore-location, and pruning never touch a slow or offline volume, and a single
scan rebuilds it (`nb catalog rebuild`).

Contributors and agents: see **[ARCHITECTURE.md](ARCHITECTURE.md)** for the
package map, the catalog `Entry`/`Placement` model, the design decisions and their
rationale, and the project conventions.

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
```
