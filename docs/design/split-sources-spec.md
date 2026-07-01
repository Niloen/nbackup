# Spec: split sources (DRY, config-derived, no workdir state)

Status: **proposal / not implemented.** This specifies how a single large source is
partitioned into several DLEs *in the config itself*, such that 100% coverage is
guaranteed by construction. It supersedes the "virtual DLE / plan-time expansion /
workdir cut-file" sketch in [large-dle-splitting.md](large-dle-splitting.md): there is
**no hidden state** — the split is declared in `sources:` and flattened at config load,
exactly like the existing `dumptype → host → [paths]` form already flattens to `[]DLE`.

## Goal and invariant

A human must not be able to express a backup that drops or double-stores data. So the
carve list is written **once** (the shards), and the "everything else" remainder is
**derived**, never hand-written. The load-time invariant:

> For a split root `P` with shard set `Σ`, the emitted DLEs **partition** `P`: their
> covered sets are pairwise disjoint and their union is exactly what the un-split DLE
> `P` would have dumped. Adding/removing a shard preserves the partition automatically.

(Caveat, stated honestly: "exactly what `P` would have dumped" is bounded by the dump
archiver's own traversal scope — `one-file-system` still stops at mount points. The
partition guarantees the shards + remainder cover the *same bytes the un-split DLE
covered*, no more. A separate filesystem under `P` was already its own-DLE
responsibility and remains so; splitting neither adds nor removes that obligation.)

## Schema

Each item in a host's path list is **either** a scalar path (a whole DLE, as today)
**or** a mapping that splits one path:

```yaml
sources:
  bigdata:
    fileserver:
      - /var/log                       # scalar: a whole DLE (unchanged)
      - path: /data                    # mapping: a split root
        split: [datasets, media, vmimages]   # RELATIVE subpaths carved out
```

Shard tokens are **relative to `path`** (`datasets`, never `/data/datasets`). Relative
tokens are load-bearing for correctness: a shard *cannot* be expressed outside its root.

> **Reconciled with main (post-merge).** The config grew a layer and renamed packages;
> this spec adapts cleanly:
> - Config is now **three layers** — `archivers:` (dump programs; `gnutar` owns
>   `one-file-system`/`tar_path`; the incremental-state root is the host's `state_dir`,
>   not an archiver option) → `dumptypes:` (an `archiver:` ref +
>   per-DLE policy, incl. `exclude: ["*.log"]` as a **`[]string`**) → `sources:`. This
>   spec touches **only `sources:` and `DLE`**, so the three-layer split is orthogonal.
> - The per-dump exclude channel **already exists**: `archiver.BackupRequest.Exclude
>   []string` ("content-dependent, per-request, not archiver config"), populated by the
>   engine from `ResolveDumpType(d).Exclude` at `engine.go:808/809, 828, 833, 1127`.
>   The partition's derived `DLE.Exclude` simply **merges into that same field** at
>   those sites (`append(dumptype.Exclude, dle.Exclude...)`) — no new plumbing.
> - Incremental-state ownership moved into the archiver (`Archiver.HasBase(dle, level)`,
>   archiver-neutral); the snapshot-continuity argument below is restated against
>   `HasBase` rather than naming a `.snar`.
> - Retention's `policy.Protected` is now `retention.Floor` (`.Keeps(id)`); the run
>   grouping type is `catalog.Run`. Neither is on this spec's path.

## Go types

```go
// SourceEntry is one item in a host's path list: a whole-path DLE, or a split root
// that partitions one path into shards plus an automatic remainder.
type SourceEntry struct {
	Path  string   // absolute path of the DLE / split root
	Split []string // relative subpaths carved into their own DLEs (nil/empty = no split)
}

// DLE gains an internal, anchored subtree-exclude list produced by a split. It is
// distinct from the dumptype's glob `exclude:` (e.g. "*.log"): these carve whole
// subtrees that partition the DLE. The engine merges them into the existing
// BackupRequest.Exclude channel: append(dumptype.Exclude, dle.Exclude...).
type DLE struct {
	Host     string
	Path     string
	DumpType string
	Exclude  []string // anchored subtree excludes from the partition (internal)
}
```

**Anchoring convention.** `BackupRequest.Exclude` is a flat `[]string` carrying *both*
the dumptype's content globs (`*.log`, unanchored) and the split's subtree excludes
(which must be **root-anchored**, or `*.log` semantics would wrongly let a shard leak).
Encode the distinction in the pattern, not a new field: a split exclude is written
**root-relative with a leading `/`** (`/datasets`), and the `gnutar` archiver treats a
leading-`/` pattern as anchored-at-archive-root (strip the `/`, emit `--anchored
--exclude=datasets`), while a pattern without a leading `/` stays an unanchored glob.
This keeps the channel a plain `[]string` and the anchoring an archiver-local rule.

`DLE.Name()` is unchanged (path-derived), so a shard `/data/datasets` →
`fileserver-data-datasets` and the remainder `/data` → `fileserver-data`. Names are
stable and unique because the partition's paths are unique.

## Decoding

The `Sources` decoder changes from `map[string]map[string][]string` to
`map[string]map[string][]SourceEntry`, and `SourceEntry` carries a custom
`UnmarshalYAML` that accepts a scalar **or** a mapping:

```go
func (e *SourceEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {        // "- /var/log"
		e.Path = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("source entry must be a path string or {path, split} mapping")
	}
	// Strict by hand: yaml.v3 does NOT propagate the parent decoder's KnownFields(true)
	// into node.Decode, so reject stray keys explicitly (the same strictness the rest
	// of the config relies on).
	for i := 0; i < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "path", "split":
		default:
			return fmt.Errorf("unknown key %q in source entry", node.Content[i].Value)
		}
	}
	var raw struct {
		Path  string   `yaml:"path"`
		Split []string `yaml:"split"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	e.Path, e.Split = raw.Path, raw.Split
	return nil
}
```

`Sources.UnmarshalYAML` then iterates `dumptype → host → []SourceEntry` and calls the
partition derivation below per entry, replacing today's one-DLE-per-path append.

## Partition derivation (pure)

Given a split root `P` (absolute, cleaned) and relative tokens `S`:

1. **Resolve & guard each token.** `shard_i = path.Clean(path.Join(P, s_i))`. Reject if
   `s_i` is absolute, contains `..` escaping above `P`, or resolves to `P` itself.
   (Relative tokens make "under `P`" the default; the guard catches `../evil`.)
2. **Dedup.** Reject duplicate shard paths within the entry.
3. **Nearest-enclosing assignment (longest-prefix).** For shard `x`, its `Exclude` =
   every shard `y` strictly under `x` with no other shard `z` between (`x ⊏ z ⊏ y`).
4. **Remainder.** Emit a DLE at path `P` whose `Exclude` = every **top-most** shard
   (those not under any other shard). Always emitted; for `split: []` it degenerates to
   the plain whole-`P` DLE with no excludes.
5. **Emit.** One DLE per shard (`Path = shard_i`, `Exclude` from step 3) + the remainder
   (step 4). All inherit the entry's host and dumptype.

```go
// derive expands one split entry into a partition of DLEs. Pure; no I/O.
func derive(host, dumptype, root string, tokens []string) ([]DLE, error)
```

The excludes are **anchored** so a shard carves its whole subtree exactly once;
longest-prefix means each path under `P` has exactly one owner. The remainder excludes
only the top-most shards (listing a nested shard there would be redundant, since its
parent shard is already excluded).

## Worked examples

**Top-level:**
```
path: /data, split: [a, b]
  /data/a     exclude: —
  /data/b     exclude: —
  /data       exclude: /data/a, /data/b          [remainder]
```

**Nested (longest-prefix in action):**
```
path: /data, split: [projects, projects/huge, media]
  /data/projects/huge   exclude: —
  /data/projects        exclude: /data/projects/huge      ← nearest enclosing
  /data/media           exclude: —
  /data                 exclude: /data/projects, /data/media   [remainder]
```
Every byte under `/data` resolves to exactly one DLE; `/data/projects/huge` is owned by
its own shard, the rest of `projects` by `~projects`, loose `/data` files by the
remainder.

## Validation rules

Split into pure (config-load) and on-disk (`nb plan`) so config parsing stays I/O-free:

**At config load (pure, fatal):**
- each token relative, resolves strictly under its root, not equal to the root;
- no duplicate tokens within an entry;
- no duplicate DLE paths across the **entire flattened disklist** (a shard path must
  not also be a standalone source; two splits must not collide).

**At `nb plan` / `--suggest-splits` (touches disk, non-fatal):**
- warn if a shard token matches nothing on disk (a typo) — **not** fatal, because the
  bytes simply stay in the remainder (no data lost), which is the whole point of the
  derived remainder;
- print the derived partition so the operator eyeballs 100% coverage before dumping.

## Coverage argument

Let `Σ` be the shards. Any file `f` under `P` is owned by the longest shard prefix in
`Σ` containing it, or by the remainder if none — **exactly one** owner (disjoint). The
remainder is always emitted and equals `P` minus the top-most shards, so nothing is
unowned (total). The operator declares only `Σ`; the `Exclude` lists and the remainder
are computed, so there is **no syntactic way to write a gap or an overlap**. The only
residual error is a typo'd token, which keeps the bytes in the remainder and is flagged
by the on-disk check.

## Snapshot / identity continuity

Incremental state is now owned by the archiver and keyed by DLE name, stated
archiver-neutrally via `Archiver.HasBase(dle, level)`.

> **Pinned by test — and it overturned the cheap-reshard assumption.**
> `TestNewExcludeIsNotADeletion` (in `internal/archiver/gnutar`) shows GNU tar 1.34 does
> **not** treat a newly-excluded but still-on-disk subtree as a deletion: it is recorded
> as "present, not dumped", so a chain restore **keeps** it. Only a real on-disk `rm`
> propagates as a deletion (`TestBackupRestoreWithDeletion`). So the earlier "the
> remainder just records the carved subtree as deleted, no re-full" claim is **false**.

The corrected rule — and the surprise is that exclusion-is-not-deletion is the
**fail-safe** behavior, not a problem:

- **Carving never drops data.** Because the remainder retains the pre-carve copy of a
  subtree even after it's excluded, a botched carve (a shard misconfigured so it captures
  nothing) loses *nothing* — the remainder still holds it. This pushes the tar-level
  guarantee in the same direction as the config-level one: you cannot carve your way into
  a gap. It is exactly the "100% of your data, no mistakes" property, now backed by the
  archiver, not just the schema.
- **Overlap is resolved by recovery, not by a re-full.** The carved subtree now lives in
  two places — the remainder's pre-carve chain (stale) and the new shard (fresh). The
  recovery recomposition orders archives **globally by run order** and takes
  most-recent-wins, so the shard's fresh copy shadows the remainder's stale one, and a
  point-in-time restore is correct across the carve boundary (before the carve →
  remainder's copy; after → shard's). The union member index never deletes, so nothing
  is ever lost in the merge. *(This makes "order sibling DLEs globally by run, not
  per-DLE" a hard requirement on the recomposition — the one real obligation the finding
  imposes.)*
- Each **new shard is a new DLE name** → `HasBase` is false → a level-0 full on first
  appearance, for free, via the existing "no base ⇒ full" path.
- **Cost is transient storage, not a forced full.** The remainder's pre-carve full keeps
  the stale copy until it ages out / is superseded by the remainder's next *natural*
  full (≤ one cycle) — the safe direction (an extra copy, never a missing one), bounded
  and self-clearing under `retention.Floor`.
- **An explicit remainder re-baseline is *optional* hygiene, not required for
  correctness.** It's worth offering for two cases: reclaiming the stale duplicate sooner
  than the next natural full, or carving *sensitive* data into an encrypted shard where
  the lingering plaintext copy in the remainder's chain is unwanted. Mechanism: record
  the remainder's effective exclude set in its incremental state and force a full when it
  changes — available as a knob, not a mandate.

## `--suggest-splits` interaction

`nb plan --suggest-splits` proposes **only additions to a `split:` list** — never an
exclude, never a remainder. Output is a copy-pasteable `sources:` fragment. Because the
remainder is derived, a generated (or hand-edited) config is *structurally incapable* of
expressing a gap: the worst a bad suggestion can do is propose a redundant or
nonexistent token, which the on-disk check flags and which loses no data.

## Interaction with existing options

- **Glob `exclude:`** (dumptype, e.g. `*.log`) still applies within each shard and the
  remainder; the split's anchored path-excludes are *additional* `--exclude` args.
  Orthogonal: glob excludes thin content, split excludes carve subtrees.
- **`one-file-system`** is per-DLE and applies from each DLE's own root; it bounds what
  "100% of `P`" means (see the Goal caveat) but does not break the partition.

## Out of scope (deferred)

- Auto-proposing *which* subtrees to carve by shape (the branchy/flat/archiver-routing
  decision tree) — that is the `--suggest-splits` heuristic, specified in
  [large-dle-splitting.md](large-dle-splitting.md); this spec only fixes the **config
  surface and its coverage guarantee**.
- The flat-count case routes to a different `archiver:` and never uses `split:` at all.
