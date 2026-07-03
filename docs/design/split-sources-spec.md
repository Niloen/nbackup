# Spec: split sources (config-derived partition)

Status: design proposal, not implemented. This specifies how a single large source is
partitioned into several DLEs *in the config itself*, such that 100% coverage is
guaranteed by construction — **no hidden state**: the split is declared in `sources:` and
flattened at config load, exactly like the existing `dumptype → host → [paths]` form
already flattens to `[]DLE`. The shape-aware *which subtrees to carve* heuristic lives in
[large-dle-splitting.md](large-dle-splitting.md); this spec fixes only the config surface
and its coverage guarantee.

## Goal and invariant

A human must not be able to express a backup that drops or double-stores data. So the
carve list is written **once** (the shards), and the "everything else" remainder is
**derived**, never hand-written:

> For a split root `P` with shard set `Σ`, the emitted DLEs **partition** `P`: their
> covered sets are pairwise disjoint and their union is exactly what the un-split DLE `P`
> would have dumped. Adding/removing a shard preserves the partition automatically.

Caveat, stated honestly: "exactly what `P` would have dumped" is bounded by the archiver's
own traversal scope — `one-file-system` still stops at mount points. A separate filesystem
under `P` was already its own-DLE responsibility; splitting neither adds nor removes that.

## Schema

Each item in a host's path list is **either** a scalar path (a whole DLE, as today) **or**
a mapping that splits one path:

```yaml
sources:
  bigdata:
    fileserver:
      - /var/log                             # scalar: a whole DLE (unchanged)
      - path: /data                          # mapping: a split root
        split: [datasets, media, vmimages]   # RELATIVE subpaths carved out
```

Shard tokens are **relative to `path`** (`datasets`, never `/data/datasets`). Relative
tokens are load-bearing for correctness: a shard *cannot* be expressed outside its root.

The config carries a `SourceEntry` (a `Path` plus optional `Split []string`) with a custom
`UnmarshalYAML` that accepts a scalar or a `{path, split}` mapping (rejecting stray keys by
hand, since yaml.v3 does not propagate the parent decoder's `KnownFields(true)` into
`node.Decode`). Flattening iterates `dumptype → host → []SourceEntry` and runs the
partition derivation below per entry.

A split produces an internal, anchored subtree-exclude list on each derived `DLE`, distinct
from the dumptype's glob `exclude:` (e.g. `*.log`): these carve whole subtrees. The engine
merges them into the existing per-request exclude channel
(`archiver.BackupRequest.Exclude`, already populated from the dumptype) — `append(dumptype
excludes, dle excludes...)`, no new plumbing. Anchoring is encoded **in the pattern, not a
new field**: a split exclude is written root-relative with a leading `/` (`/datasets`), and
the `gnutar` archiver treats a leading-`/` pattern as anchored-at-archive-root (strip the
`/`, emit `--anchored --exclude=datasets`) while a pattern without one stays an unanchored
glob. `DLE.Name()` is unchanged (path-derived), so shard `/data/datasets` →
`fileserver-data-datasets`, remainder `/data` → `fileserver-data`; names are stable and
unique because the partition's paths are unique.

## Partition derivation (pure, no I/O)

Given a split root `P` (absolute, cleaned) and relative tokens `S`:

1. **Resolve & guard each token.** `shard_i = Clean(Join(P, s_i))`. Reject if `s_i` is
   absolute, escapes above `P` via `..`, or resolves to `P` itself.
2. **Dedup.** Reject duplicate shard paths within the entry.
3. **Nearest-enclosing assignment (longest-prefix).** For shard `x`, its exclude set =
   every shard `y` strictly under `x` with no other shard between them.
4. **Remainder.** Emit a DLE at path `P` excluding every **top-most** shard (those under no
   other shard). Always emitted; `split: []` degenerates to the plain whole-`P` DLE.
5. **Emit** one DLE per shard + the remainder, all inheriting the entry's host and dumptype.

Longest-prefix means each path under `P` has exactly one owner; the remainder excludes only
the top-most shards (a nested shard's parent is already excluded).

**Worked example (nested):**
```
path: /data, split: [projects, projects/huge, media]
  /data/projects/huge   exclude: —
  /data/projects        exclude: /data/projects/huge      ← nearest enclosing
  /data/media           exclude: —
  /data                 exclude: /data/projects, /data/media   [remainder]
```

## Validation

**At config load (pure, fatal):** each token relative, resolves strictly under its root,
not equal to it; no duplicate tokens within an entry; no duplicate DLE paths across the
entire flattened disklist (a shard must not also be a standalone source; two splits must
not collide).

**At `nb plan` / `--suggest-splits` (touches disk, non-fatal):** warn if a shard token
matches nothing on disk (a typo) — **not** fatal, because the bytes simply stay in the
remainder (no data lost); print the derived partition so the operator eyeballs 100%
coverage before dumping.

## Coverage argument

Any file `f` under `P` is owned by the longest shard prefix in `Σ` containing it, or by the
remainder if none — **exactly one** owner (disjoint). The remainder is always emitted and
equals `P` minus the top-most shards, so nothing is unowned (total). The operator declares
only `Σ`; the excludes and remainder are computed, so there is **no syntactic way to write
a gap or an overlap**. The only residual error is a typo'd token, which keeps the bytes in
the remainder and is flagged by the on-disk check.

## Snapshot / identity continuity — exclusion is not deletion

Incremental state is owned by the archiver and keyed by DLE name (`Archiver.HasBase(dle,
level)`).

> **Pinned by test.** `TestNewExcludeIsNotADeletion` (in `internal/archiver/gnutar`) shows
> GNU tar 1.34 does **not** treat a newly-excluded but still-on-disk subtree as a deletion:
> it is recorded as "present, not dumped," so a chain restore **keeps** it. Only a real
> on-disk `rm` propagates as a deletion (`TestBackupRestoreWithDeletion`). So a naive "the
> remainder just records the carved subtree as deleted, no re-full" claim is **false** —
> and the surprise is that this is the **fail-safe** direction:

- **Carving never drops data.** The remainder retains the pre-carve copy of a subtree even
  after it is excluded, so a botched carve (a shard that captures nothing) loses *nothing*.
  This pushes the tar-level guarantee in the same direction as the config-level one: you
  cannot carve your way into a gap.
- **Overlap is resolved by recovery, not by a re-full.** The carved subtree lives in two
  places — the remainder's pre-carve chain (stale) and the new shard (fresh). The recovery
  recomposition orders archives **globally by run order** and takes most-recent-wins, so
  the shard's fresh copy shadows the remainder's stale one and a point-in-time restore is
  correct across the carve boundary. *(This makes "order sibling DLEs globally by run, not
  per-DLE" a hard requirement on the recomposition — the one real obligation the finding
  imposes.)*
- Each **new shard is a new DLE name** → `HasBase` is false → a level-0 full on first
  appearance, for free, via the existing "no base ⇒ full" path.
- **Cost is transient storage, not a forced full.** The stale copy ages out by the
  remainder's next natural full (≤ one cycle), bounded and self-clearing under
  `retention.Floor`.
- **An explicit remainder re-baseline is *optional* hygiene, not required for
  correctness** — worth offering to reclaim the stale duplicate sooner, or to carve
  *sensitive* data into an encrypted shard where the lingering plaintext copy is unwanted.
  Mechanism: record the remainder's effective exclude set in its incremental state and
  force a full when it changes — a knob, not a mandate.

## Interactions

- **`--suggest-splits`** proposes only additions to a `split:` list — never an exclude,
  never a remainder — as a copy-pasteable `sources:` fragment. Because the remainder is
  derived, a generated config is *structurally incapable* of expressing a gap.
- **Glob `exclude:`** (dumptype, `*.log`) still applies within each shard and the
  remainder; the split's anchored path-excludes are additional `--exclude` args. Orthogonal:
  glob excludes thin content, split excludes carve subtrees.
- **`one-file-system`** is per-DLE from each DLE's own root; it bounds what "100% of `P`"
  means (see the Goal caveat) but does not break the partition.
- **Out of scope:** the shape heuristic for *which* subtrees to carve (see
  [large-dle-splitting.md](large-dle-splitting.md)); the flat-count case routes to a
  different `archiver:` and never uses `split:`.
