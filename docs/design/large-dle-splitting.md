# Design note: automatic splitting of large DLEs

Status: design exploration, not implemented. This maps the design space for handling
DLEs too large to schedule, dump, or full as one unit, and records the load-bearing
constraints + decision tree so an implementation starts from the right place. The
**config mechanism** for the branchy case is specified separately in
[split-sources-spec.md](split-sources-spec.md) (config-derived partition, no hidden
state); this note holds the *why* — snapshot-as-atom, catch-all-by-exclusion, and the
shape-aware "when *not* to split" reasoning. See ARCHITECTURE.md for the vocabulary
(DLE / Run / Archive / snapshot / Estimate / planner).

## The problem

A DLE is monolithic: one `host`+`path` → one tar stream → one Archive per level,
scheduled by the planner as one lump. This scales badly:

1. **The planner can't smooth one big DLE.** `promote()` balances load by moving whole
   fulls *across* DLEs; a single DLE whose full exceeds a day's `RoomBytes` can't split
   across days, or trips the structural-capacity warning.
2. **Fulls serialize on spanning media.** On any `part_size` medium dumpers clamp to 1,
   so several large fulls back-to-back → a very long window.
3. **Long incremental chains** off one aged base for a DLE you can rarely afford to full
   (level bumping already caps blind climbing, but not this).

Spanning (parts) splits the *compressed byte stream* across volumes for media-fitting;
it does not help here — the archive is still one snapshot, one tar process, one
scheduling unit. Splitting for *window and scheduling* must happen above the snapshot, in
the namespace.

## The load-bearing constraint: the snapshot is the atom

NBackup's incrementals are GNU tar `--listed-incremental` against a per-DLE `.snar`. That
snapshot is the **indivisible unit of incremental coherence** — tar can't split one
snapshot into parts, nor compose several back into one.

> Every splitting technique reduces to the same thing: a policy for choosing and
> *stabilizing* a set of sub-boundaries, each of which owns exactly one snapshot.

Consequences that kill tempting ideas up front:

- **No transparent intra-DLE parallelism.** Fanning N tar processes over subtrees means N
  snapshots — i.e. N sub-DLEs. There is no single-logical-chain parallel dump with stock
  tar.
- **Stability beats balance.** Optimal bin-packing is unstable: a file that changes
  buckets is re-dumped (new in its new shard, deleted in its old). Prefer
  stable-but-imbalanced; pay for rebalancing in *localized* re-fulls, not global ones.

The recovery layer already merges member lists across archives ("one logical DLE = many
archives"), so recomposing shards into one restore tree is free.

## Two node shapes, three partition functions

"Large" hides opposite workloads, so the splitter **dispatches on the shape of the node**:

| Shape | Signal (seal `FileCount` + estimate fan-out) | Strategy |
|---|---|---|
| **Branchy** | bytes spread across many child dirs | subtree frontier-cut + catch-all |
| **Flat, byte-heavy** | few huge files, low count | per-file/group shards; delta method for hot big files |
| **Flat, count-heavy** | millions of small files, low fan-out | **do not namespace-split**; change the `archiver` |

## Branchy: the frontier cut + catch-all

**The cut.** Treat the shard set as a **frontier** through the directory tree: a cut where
every subtree below it is ≤ a target size T. Start with the whole DLE as one shard; when a
shard's estimate exceeds T, replace it with its children; recurse. The persisted artifact
is tiny (the cut-paths), and rebalancing is *local* — a directory growing past T splits
only itself.

**The catch-all: define by exclusion, never by enumeration.** The residue (loose root
files, small un-promoted dirs, and **new top-level dirs that appear later**) must land
somewhere deterministically every run, or data is silently dropped. Enumeration (explicit
includes) silently drops a new `/data/newthing`. Exclusion captures it *by construction*:
`tar /data --exclude=projects --exclude=archive --exclude=media`; the default is
"backed up," the only question is *which* shard. So the catch-all is the **ground state** —
a fresh DLE is 100% catch-all, zero shards; splitting is the catch-all progressively
shedding subtrees big enough to earn a snapshot. Each file maps to one shard by
**longest-prefix match** (each shard `--exclude --anchored`s its immediate descendant
shards); the catch-all is the zero-length default route.

**The catch-all self-heals.** It is measured every run (seal `DumpStats` + the pre-dump
`Estimate`); growth past T *is* the autotune signal, and because the estimate runs before
the dump the trigger is proactive (`nb plan --suggest-splits`). Absorbing a new whale for
one cycle then graduating it is correct, not a gap: the first full of new data is mandatory
work no matter which shard owns it; promotion only stops that subtree dragging a re-full
*every cycle after*.

**Promotion and its one cost.** When `/data/newthing` graduates, the new shard does a
level-0 full and the catch-all adds `--exclude=newthing`. GNU tar does **not** record the
now-excluded subtree as a deletion (see [the finding in
split-sources-spec.md](split-sources-spec.md#snapshot--identity-continuity), pinned by
`TestNewExcludeIsNotADeletion`) — it marks it "present, not dumped," so the catch-all's
chain **retains** the old copy. This is the **fail-safe** direction: carving can never
*drop* data; the worst case is the subtree exists in two places until the catch-all's next
natural full ages the stale copy out, and recovery resolves the overlap by
most-recent-wins. A forced catch-all re-full is therefore **optional hygiene, not a
correctness requirement**.

Other properties: the catch-all has a **permanent name** (e.g. `data//rest`) that does not
encode the shard set, so its snapshot chain survives every promote/merge; an empty
catch-all is healthy (the standing safety net); a blown per-run ceiling fires the planner's
existing loud structural warning; hardlinks across a boundary get stored twice, so prefer
cuts at top-level-dir boundaries.

## Flat, count-heavy: change the archiver, don't split

For a branchy tree, subtree splitting *divides* the metadata walk. For one flat directory
of millions of files there are no subtree boundaries — the only partition key is the
filename, and any name bucket that keeps native incremental must `--exclude` over a **full
traversal of the whole directory**. So N shards = **N× the metadata walk**, and for a flat
dir metadata *is* the cost: splitting makes the dominant cost worse. You get exactly two
ways to bucket a flat set, each with a fatal cost:

1. **Exclude-based name buckets, keep native incremental** → tar re-traverses per shard →
   N× walk.
2. **Explicit hashed file lists (`--no-recursion -T`), walk once** → `--listed-incremental`
   is traversal-shaped, so a `--no-recursion` subset bypasses its change/deletion
   detection → you lose native incremental.

That dichotomy is the wall; there is no tar trick giving one walk *and* native incremental.
What works, in order:

1. **Change the archiver, not the split (the right answer, Amanda-faithful).** A flat dir
   of millions of small files is exactly tar's worst workload. A **block-level snapshot**
   (LVM/btrfs/ZFS `send`, `xfs_dump`) captures a consistent point-in-time in ~O(changed
   blocks). This is what the `archiver` abstraction (Amanda's Application API) is *for* —
   Amanda ships `amzfs-sendrecv`/`amxfsdump` alongside `amgtar`. Routing a flat-count DLE
   to a different archiver is a registry registration, not a new concept.
2. **mtime split helps bytes, not the walk.** Cold files fall to near-empty incrementals,
   but you still `stat` every file — useful for window/egress, not the metadata storm.
3. **A single-walk, manifest-bucketed `filelist` archiver.** One traversal → a manifest
   (name, mtime, size, inode); assign each file to a bucket; per bucket diff against its
   previous manifest and feed changed+new to `tar -T --no-recursion`, recording deletions
   in the seal. Stated plainly: you have **reimplemented `--listed-incremental`** —
   deletion-faithfulness rides on your manifest, not tar's snapshot. That is why it is an
   *archiver*, not a splitter.

Hash/manifest bucketing is **total**, so there is **no catch-all inside a flat dir** — the
residue problem is a property of *tree* routing. The two compose: tree-routing carries you
down to `/data/bigdir`, the flat-dir partition takes over inside it.

## Flat, byte-heavy: split by file

A few huge files, modest count → byte-bound, walk cheap, splitting works: drive tar from an
explicit file list (`-T --no-recursion`), one big file or a balanced group per shard, so
parallel dumpers on unbounded media shrink the window. A big file that *changes* re-dumps
whole (tar is whole-file granularity) → route it to a delta/rsync-style archiver, not a
splitter.

## The `nb plan --suggest-splits` decision tree

The feedback loop earns its keep by **refusing to split when splitting would hurt** and
naming the archiver change instead. It runs on data already captured (seal `FileCount` +
`DumpStats` + the pre-dump `Estimate` walk extended with fan-out):

```
for each DLE / catch-all / shard whose estimate or last full exceeds T or RoomBytes,
or whose dump time exceeds the window:

  measure: bytes, file_count, fan_out (# child dirs), biggest_child

  if fan_out high (branchy):
      → FRONTIER CUT: promote biggest_child to its own shard (recurse until residue ≤ T).
        Catch-all absorbs the rest by exclusion.
  elif file_count high and fan_out low (flat, count-heavy):
      → DO NOT namespace-split (a cut multiplies the metadata walk N×).
      → ARCHIVER CHANGE: block snapshot (zfs/lvm/btrfs), or the manifest filelist archiver.
  elif bytes high and file_count low (flat, byte-heavy):
      → PER-FILE SHARDS (explicit file lists, parallel on unbounded media);
        flag hot big files for a delta archiver.
  else (one solid stream):
      → no split helps; recommend strategy:incronly / a longer per-DLE dumpcycle.
```

`auto_split` (opt-in) applies branchy/per-file proposals with forced fulls; archiver
changes are always operator-confirmed (they change the artifact and may need filesystem
support).

## The smaller levers, and sequencing

Splitting is heavy machinery. Level bumping already covers the incremental-chain-growth
axis. Two cheaper Amanda knobs cover the "big but unsplittable / rarely fullable" tail and
should land first — both pure planner additions, no new artifacts, making the splitter
optional rather than mandatory:

- **`strategy: incronly` / `nofull`** (per dumptype) — full a huge DLE once, then only
  incrementals.
- **Per-dumptype `dumpcycle` override** — a huge DLE fulls on a 90-day cycle while small
  DLEs stay at 7.

Suggested order: (1) `strategy` + per-dumptype `dumpcycle`; (2) shape-aware `nb plan
--suggest-splits`, advisory only (highest value — stops bad splits, teaches the operator,
no engine mutation); (3) branchy frontier-cut + catch-all via the config-derived partition
of [split-sources-spec.md](split-sources-spec.md) (the exclude-vs-deletion behavior is
already pinned, so the remaining work is the schema change and the recovery recomposition,
which must **order sibling DLEs globally by run** for most-recent-wins to resolve carve
overlap); (4) flat-dir archiver routing.

The through-line: automatic splitting's real payoff is handing the planner small, stable,
independently-schedulable units — which one giant DLE denies it — and **knowing when not to
split**, routing to a different archiver instead.
