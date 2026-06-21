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

NBackup uses Amanda's **multilevel** scheme (levels 0–9). `nb plan` / `nb dump`
decide a level per DLE:

- No full ever for the DLE → **L0 (full)** — required before any incremental.
- Last full at/over `full_interval_days` → **due for a full**, staggered to a
  per-DLE day so fulls don't all land on the same day. A full overdue by
  ≥ 2× the interval is forced regardless.
- Otherwise → the **next incremental level** (one higher than the last run,
  capped at 9). Level N captures only what changed since level N−1, so daily
  volume stays small.

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

`nb slot prune` is a dry-run by default. A slot is eligible for deletion only
when it is older than `cycle.minimum_age` **and** every DLE it holds has a newer
full backup elsewhere — so the last valid recovery path is never removed. Add
`--apply` to actually delete.

## Configuration

See [`nbackup.example.yaml`](nbackup.example.yaml). Minimal example:

```yaml
storage:
  budget: 20TB
cycle:
  minimum_age: 30d
landing:
  media: local-disk
media:
  local-disk:
    path: /var/lib/nbackup/catalog
sources:
  - host: app01
    path: /home
  - host: db01
    path: /var/lib/postgresql
```

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
| `dle` | the DLE domain type (host, path, method) | Disklist |
| `slot` | slot format: pure data + (de)serialization | Header / amar |
| `media` | `Store` (landing) + `Vault` (copies) interfaces + registry | Device API |
| `media/localdisk`, `media/s3`, `media/tape` | implementations (s3/tape are registered stubs) | tape/s3/vfs devices |
| `method` | `Method` dump interface + registry | Application API |
| `method/gnutar` | GNU tar implementation (all tar/snapshot specifics) | amgtar |
| `xfer` | stream pipeline: zstd + checksum + counting | Xfer API |
| `catalog` | slot listing, run `History`, snapshot library, slot-id allocation | catalog / curinfo |
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

One deliberate split: slots (the source of truth) live on the `media.Store`,
while local operational state — the run history and the GNU tar snapshot library
— lives in a local **workdir** (default: the catalog path), so it stays local
even when the store is remote, exactly as Amanda keeps `gnutar-lists` and
`curinfo` on the host.

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
```
