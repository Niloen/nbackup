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

Archives are ordinary tar streams compressed with zstd. Recovery never requires
NBackup:

```bash
tar --zstd -xf app01-home-L0.tar.zst      # or: zstd -dc file.tar.zst | tar -xf -
```

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

### Planning (balanced scheduling)

`nb plan` / `nb dump` decide a level per DLE:

- No full ever for the DLE → **L0 (full)** — required before any incremental.
- Last full younger than `full_interval_days` → **L1 (incremental)**.
- Last full at/over the interval → **due for a full**, staggered to a
  per-DLE day so fulls don't all land on the same day. A full overdue by
  ≥ 2× the interval is forced regardless.

Incrementals archive only files that are new or modified since the DLE's last
full, compared against a snapshot stored in the catalog's `state.json`.

### Sealing

A run writes archives, then `MANIFEST.json` and `CHECKSUMS.sha256`, verifies the
checksums against what landed on disk, and finally writes `SLOT.json` with
`status: sealed`. After sealing, a slot is treated as immutable — re-running a
sealed date is refused.

### Restore

Restoring a DLE as of a slot replays its most recent full at or before that
slot, then every later incremental up to it, in order. You can restore a single
DLE (`-dle`) or all DLEs in the slot.

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

## Status & limitations (first version)

Implemented: local-disk landing, balanced full/incremental planning, immutable
sealed slots, checksum verification, point-in-time restore, budget reporting,
cycle-safe pruning.

Not yet implemented (declared in config for forward-compatibility):

- **S3 and tape media** — only `local-disk` landing works today.
- **Budget-driven retention** — budget is reported; pruning is cycle-based, not
  yet automatically driven to fit the budget.
- **Remote sources** — `host` is metadata; `path` is read from the local
  filesystem (run the agent where the data is, or mount it).
- **Deletion tracking in incrementals** — a file deleted between a full and an
  incremental will reappear on restore. Levels are L0/L1 only (no L2+ yet).

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
```
