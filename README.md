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
| —           | `nb copy`    | Copy a slot to another medium (disk → tape) |
| —           | `nb label`   | Label a volume (required for tape before its first dump) |
| —           | `nb medium`  | List media (capacity, usage, volume) or detail one |
| —           | `nb changer` | Inventory (`list`) or mount (`load`) volumes in a library |

## Quick start

```bash
cp nbackup.example.yaml nbackup.yaml   # edit sources + catalog path

nb plan                # preview today's plan and budget usage
nb dump                # run the backup, producing one sealed slot
nb slot                # list slots (with a COPIES column: where each lives)
nb slot show slot-2026-06-21   # archives + every copy's volume and file positions
nb medium              # media overview: type, slots, usage / capacity, volume
nb medium lto          # one medium's volume and the slots it holds
nb verify              # re-check all archive checksums
nb restore -dle app01-home -dest /tmp/out slot-2026-06-21
```

Every command accepts `-c <config>` and `-C <catalog>` overrides, and flags may
appear before or after positional arguments.

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

The `tape` medium is a **library of bays behind one drive**, modeled with two
internal seams: a `device` (the `mt` analogue — file I/O of one mounted tape) and
a `changer` (the robot analogue — which bay is in the drive). `dir:` selects a
directory-backed library (each bay a subdirectory, finite per-bay `volume_size`,
fully tested); `device:` selects a real single drive (`mt` + `/dev/nst0`, a
one-bay library), structurally complete but unverified without hardware, so CI
exercises the virtual library.

**Bays vs labels.** A *bay* is a physical position (`bay-01…`, the durable
cartridge identity); a *label* is logical data written at file 0 (`nbackup` magic,
name, pool, epoch). They are deliberately distinct: a blank cartridge has a bay
but no label, and relabeling rewrites the label without changing the bay. Like a
real autochanger, the `changer` is **label-agnostic** — it mounts bays; the label
is read from the drive only after mounting. `bays: N` stocks the library with N
blank bays; `nb changer list` inventories bay→label; `nb changer load` mounts one.

**Append vs one-run-per-tape.** A tape fills until end-of-tape (`ErrVolumeFull`),
then you change tapes. `appendable: true` (default, Bacula-style) packs many runs
onto a tape until full; `appendable: false` (Amanda-style) uses one run per tape.
Switching is **manual**: a full tape is refused and you `nb changer load` the next
(or `nb label --relabel` an aged-out one). Automatic advance and tape spanning
are the next step.

**Volume labels.** The label is a *capability* (`media.Labeled`), so
address-identified media (disk, S3) carry none and skip the dance. Before a write
the engine verifies the loaded tape's label and **refuses** a foreign, blank
(unless `auto_label`), wrong-pool, or relabeled-since-cached reel — Amanda's
overwrite guard. Label a tape with `nb label <medium> <name>` (it grabs a blank
bay); reuse an expired one with `nb label --relabel` (refused while it holds
protected slots; `--force` overrides). On read, the changer **auto-mounts** the
bay holding each copy's tape, and every archive header is asserted against the
catalog. The catalog caches the volume registry (`catalog.Volumes`, Amanda's
*tapelist*).

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
behind interfaces with named, registered implementations, and a single
orchestrator composes them.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | Disklist / dumptype / storage |
| `slot` | slot metadata: pure data + lifecycle (`NewSlot`/`AddArchive`/`Seal`) | Header / amar |
| `slotio` | maps a slot onto a `Volume`'s files (headers, seal record, verify) | taper / amrestore |
| `media` | `Volume` (positional, self-describing files + headers) + `Profile` + registry | Device API |
| `media/disk`, `media/tape`, `media/s3` | Volume impls: disk (sidecar headers, clean payloads), tape (sequential, file-numbered; `dir:` virtual or `device:` real via an mt seam), s3 (stub) | vfs/tape/s3 devices |
| `method` | `Method` dump interface + registry (configured via dumptype options) | Application API |
| `method/gnutar` | GNU tar implementation (all tar/snapshot specifics) | amgtar |
| `filter` | external compressor child processes (zstd/gzip/none) + registry | gzip/custom compress |
| `xfer` | in-process stream metering: checksum + byte counting | Xfer API |
| `catalog` | local cache of the slot index + snapshot library; derives run `History` | catalog / curinfo / tapelist |
| `policy` | cross-cutting retention safety floor: protected slots (pure) | Policy |
| `planner` | multilevel level scheduling (pure) | planner |
| `engine` | the driver: schedules parallel dumpers, wires planner→method→filter→media→catalog | driver / taper |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, policy, method, filter,
slotio, catalog, config}` and the leaf packages `{media, xfer, slot, sizeutil}`.
Domain packages stay pure; `method`/`media`/`filter` are pluggable adapters;
`engine` is the only component aware of all of them. A backup reads as a pipeline
of processes — **source** (`tar` via `method.Backup`) → **filter** (external
compressor child) → **dest** (`media.Volume`), metered (checksum + size) and
composed by `slotio`. Like Amanda, `engine` runs up to `parallelism.dumpers`
dumpers concurrently (each a `tar`+compressor pipeline) and can `nice` the
children. Adding a storage medium, dump method, or codec is a registry
registration, not a conditional in the core.

### The catalog is a cache

Slots on the `media.Volume` are the **source of truth**; every file is
self-describing (header block), every slot carries a seal record, and every
labeled volume carries a label record. The `catalog` is a **local cache** whose
model separates what a slot *is* from where its copies *are*: an **`Entry`** pairs
one medium-independent slot with a set of **`Placement`s**, each naming a volume
and the file position of every archive on it. So a slot copied disk→tape is one
entry with two placements; restore reads from whichever copy is available. The
cache also holds the **volume registry** (`catalog.Volumes`). Planning, listing,
restore-location, pruning, and budget reporting never touch the media — which
matters when a volume is slow or offline (S3/Glacier/tape). This mirrors Amanda,
which never scans tapes to operate: it keeps `curinfo`/`tapelist`/catalog databases
locally and rebuilds them from self-describing media when needed.

The catalog lives in its **own directory** (`workdir`, default `nbackup-catalog`),
**independent of any storage medium** — it is a cache over the whole pool, not part
of one medium. One `Files()` scan rebuilds everything: seals → the slot index,
labels → the volume registry (`nb catalog rebuild`).

Consequences:

- **Run `History` is derived** from the cached slots, not separately persisted —
  so there is no second source to drift. (Each seal record holds the date and
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
