# Design note: automatic splitting of large DLEs

Status: **exploration / not implemented.** This is a design-space map for handling
DLEs too large to schedule, dump, or full as one unit. It proposes a *shape-aware*
splitter driven by the existing catalog + estimate machinery. Nothing here is built;
the point is to record the load-bearing constraints and the decision tree so an
eventual implementation starts from the right place. See
[ARCHITECTURE.md](../../ARCHITECTURE.md) for the vocabulary (DLE / Run / Archive /
snapshot / Estimate / planner).

> **Superseded where they disagree:** the **config mechanism** is now specified in
> [split-sources-spec.md](split-sources-spec.md), which uses a **config-derived
> partition with no hidden state** — the operator declares the carve points once in
> `sources:`, the "everything else" remainder is derived at config load, and
> `--suggest-splits` only ever *proposes a config edit* (no magic, no plan-time virtual
> DLE, no workdir cut-file). Where sections below describe plan-time expansion of a
> "virtual DLE" or a persisted `splits/` cut-file, read the spec instead; the *why*
> (snapshot-as-atom, the catch-all-by-exclusion, shape-aware suggestion) still holds.

## The problem

A DLE is monolithic: one `host`+`path` → one tar stream → one Archive per level,
scheduled by the planner as one lump. This scales badly three ways:

1. **The planner can't smooth one big DLE.** `planner.promote()` balances load by
   moving *whole fulls* between days *across* DLEs. A single DLE whose full exceeds a
   day's `RoomBytes` can't be split across days, or trips the structural-capacity
   warning.
2. **Fulls serialize on spanning media.** On tape (any `part_size` medium) dumpers
   clamp to 1, so several large DLEs full back-to-back → a very long window.
3. **Long incremental chains.** The planner now does Amanda-style level bumping (sit at
   L1, climb only on real savings — landed on main), so chains no longer climb
   blindly to L9; but a DLE you can rarely afford to *full* still accumulates a long,
   slow, risky chain off one aged base.

Spanning (parts) already solves "a DLE bigger than one volume" — it splits the
*compressed byte stream* across volumes for media-fitting. It does **not** help here:
the archive is still one snapshot, one tar process, one scheduling unit. Splitting for
*window and scheduling* must happen above the snapshot, in the namespace.

## The load-bearing constraint: the snapshot is the atom

NBackup's incrementals are GNU tar `--listed-incremental` against a per-DLE `.snar`.
That snapshot is the **indivisible unit of incremental coherence** — tar can't split
one snapshot into parts, nor compose several back into one.

> Every splitting technique reduces to the same thing: a policy for choosing and
> *stabilizing* a set of sub-boundaries, each of which owns exactly one snapshot.

Consequences that kill tempting ideas up front:

- **No transparent intra-DLE parallelism.** Fanning N tar processes over subtrees of
  one DLE means N snapshots — i.e. N sub-DLEs. There is no single-logical-chain
  parallel dump with stock tar.
- **Stability beats balance.** Optimal bin-packing is unstable: a file that changes
  buckets is re-dumped (new in its new shard, deleted in its old). Prefer
  stable-but-imbalanced over optimal-but-drifting; pay for rebalancing in *localized
  re-fulls*, not global ones.

The product framing that makes this "automatic" rather than "go edit your config into
8 lines" is a **virtual DLE**: the operator writes one entry, the engine expands it at
plan time into synthetic child DLEs (shards), each owning a snapshot, and
`recovery.BuildTree` (which already merges member lists across archives) recomposes
them into one tree at restore. The recovery layer is already shaped for "one logical
DLE = many archives," so the restore UX is free.

```yaml
sources:
  default:
    fileserver: [/data]        # auto_split: { target_shard: 200GB }
```

## Two node shapes, two partition functions

A single strategy cannot serve every DLE, because "large" hides opposite workloads.
The splitter **dispatches on the shape of the node** it is considering:

| Shape | Signal (from seal `FileCount` + estimate fan-out) | Strategy |
|---|---|---|
| **Branchy** | bytes spread across many child dirs | subtree frontier-cut + catch-all |
| **Flat, byte-heavy** | few huge files, low count | per-file/group shards; delta-method for hot big files |
| **Flat, count-heavy** | millions of small files, low fan-out | **do not namespace-split**; change the `archiver` |

The two hard parts are the branchy catch-all and the flat-count case. They are treated
in turn, then unified by the decision tree.

## Branchy: the frontier cut + catch-all

### The cut

Treat the shard set as a **frontier** through the directory tree: a cut where every
subtree below it is ≤ a target size T. Start with the whole DLE as one shard; when a
shard's estimate exceeds T, replace it with its children; recurse. Heavy deep
directories get finely split, sparse areas stay coarse. The persisted artifact is
tiny — the set of cut-paths. Rebalancing is *local*: a directory growing past T splits
only itself (a re-full of just its new children), never the whole DLE.

### The catch-all: define by exclusion, never by enumeration

The residue (loose files at the root, small un-promoted dirs, and **new top-level dirs
that appear later**) must land somewhere deterministically every run, or data is
silently dropped. The fix is one inversion:

- **Enumeration (the trap):** back up `projects` + `archive` + `media` as explicit
  includes. A new `/data/newthing` matches nothing → **silently dropped**. Violates
  the house "no silent caps" rule.
- **Exclusion (the fix):** the catch-all is `tar /data --exclude=projects
  --exclude=archive --exclude=media`. Anything not explicitly carved out is captured
  *by construction*. The default is "backed up"; the only question is *which* shard.

So the catch-all is the **ground state**, not an edge case: a fresh DLE is 100%
catch-all, zero shards. Splitting is the catch-all progressively shedding subtrees that
grew big enough to earn a snapshot. Every shard was once catch-all.

### Routing table mental model

Each file maps to one shard by **longest-prefix match**; the catch-all is the
zero-length default route. Nested shards (`/data/projects` and
`/data/projects/huge`) just work. Mechanically each shard excludes its immediate
descendant shards (`--exclude --anchored`); the catch-all excludes the top-most
shards. Every byte counted once, every run.

### The catch-all is the nursery, and self-heals

It is measured every run like any shard (seal `DumpStats`, and the pre-dump `Estimate`
pass). When it grows past T, that *is* the autotune signal — and because the estimate
runs *before* the dump, the trigger is **proactive**: `nb plan --suggest-splits` does a
depth-limited estimate of the catch-all, finds the child driving growth, and proposes
promoting it.

Reframe the apparent latency worry (a 500 GB dir appears and lands in the catch-all
before promotion): **the first full of new data is mandatory work no matter which shard
owns it.** Promotion never saves the first dump; it stops that subtree from dragging a
re-full *every cycle after*. So "absorb the new whale for one cycle, then graduate" is
the correct behavior, not a gap.

### Promotion mechanics and the one cost

When `/data/newthing` graduates out:

- The **new shard** does a level-0 full (fresh snapshot).
- The **catch-all** adds `--exclude=newthing`. GNU tar does **not** record the now-
  excluded subtree as a deletion — it marks it "present, not dumped", so the catch-all's
  existing chain **retains** the old copy of `newthing` (verified by
  `TestNewExcludeIsNotADeletion` in `internal/archiver/gnutar`).

This turns out to be the **fail-safe** direction, and it is what makes carving
mistake-proof at the tar level — see [the finding](split-sources-spec.md#snapshot--identity-continuity)
below. Carving can never *drop* data from the catch-all; the worst case is the carved
subtree exists in two places (the catch-all's pre-carve chain + the new shard) until the
catch-all's next natural full ages the stale copy out. Recovery resolves the overlap by
most-recent-wins (the shard's fresh copy shadows the catch-all's stale one), so a
point-in-time restore is correct across the carve boundary. A forced catch-all re-full
is therefore **optional storage/security hygiene, not a correctness requirement**.

> ✅ **Test landed.** `TestNewExcludeIsNotADeletion` pins the behavior (exclusion ≠
> deletion). It overturned the original "carving records a deletion, no re-full"
> assumption — but in the *safe* direction: see the spec's reconciled continuity
> section.

### Failure modes, handled

- **Catch-all blows the per-run ceiling** before a whale graduates → the planner's
  existing structural/`RoomBytes` warning fires (**loud, never silent**); recursive
  cut converges the residue under T in a cycle or two.
- **Empty catch-all is healthy**, not waste — it is the standing safety net every
  future new file lands in. Run it; it's cheap.
- **Stable identity.** The catch-all is a synthetic DLE with a *permanent* name (e.g.
  `data//rest`) that does **not** encode the shard set, so its snapshot chain survives
  every promote/merge around it.
- **Hardlinks across a boundary** get stored twice and may not restore as a link.
  Prefer cuts at top-level-dir boundaries (rarely crossed by links); the autotune
  should not cut inside a detected link cluster.

## Flat, count-heavy: the economics flip — change the archiver, don't split

For a branchy tree, subtree splitting **divides** the metadata walk. For one flat
directory of millions of files there are no subtree boundaries; the only partition key
is the filename, and any name/pattern bucket that keeps native incremental must
`--exclude` over a **full traversal of the whole directory**. So N shards = **N× the
metadata walk** — and for a flat dir, metadata *is* the cost. Namespace-splitting
doesn't just stop helping, it makes the dominant cost worse. The unsplit baseline (one
walk) beats it.

Why you can't have both: you get exactly two ways to bucket a flat set, each with a
fatal cost —

1. **Exclude-based name buckets, keep native incremental** → tar re-traverses the
   whole dir per shard → **N× walk**.
2. **Explicit hashed file lists (`--no-recursion -T`), walk once** → `--listed-
   incremental` is traversal-shaped, so a `--no-recursion` subset bypasses its
   change/deletion detection → **you lose native incremental**.

That dichotomy is the wall. There is no tar trick that gives one walk *and* native
incremental, because tar emits one stream and its incrementality is directory-shaped.

What actually works, in order:

1. **Change the archiver, not the split (the right answer, Amanda-faithful).** A flat
   dir of millions of small files is exactly the workload tar is worst at. Stop walking
   files: a **block-level snapshot** (LVM/btrfs/ZFS, `zfs send`, `xfs_dump`) captures a
   consistent point-in-time in ~O(changed blocks). This is what the new `archiver`
   abstraction (main renamed `method` → `archiver`, Amanda's Application API) is *for* —
   Amanda ships `amzfs-sendrecv`/`amxfsdump` alongside `amgtar` for exactly this.
   Routing a flat-count DLE to a different archiver is a registry registration (a new
   `archivers:` type), not a new concept.
2. **mtime split helps bytes, not the walk.** Cold files fall to near-empty
   incrementals (saves transfer + compression), but you still `stat` every file →
   no relief for the metadata storm. Useful for window/egress, not a fix.
3. **Inventive in-lane option — a single-walk, manifest-bucketed `filelist` method.**
   One traversal → a manifest (name, mtime, size, inode); pay the walk *once*. Assign
   each file to a bucket (hash for stable count-balance, or byte-pack with a persisted
   append-only assignment for window-balance). Per bucket, diff against its previous
   manifest → changed/new/deleted, feed changed+new to `tar -T --no-recursion`, record
   deletions in the seal. The per-bucket manifest becomes the precious state in place
   of the `.snar`. Restore stays stock `tar -T`. The price, stated plainly: you have
   **reimplemented `--listed-incremental`** — deletion-faithfulness now rides on your
   manifest, not tar's snapshot. That is why it is a *method*, not a splitter.

Note: hash/manifest bucketing is **total** (every name maps to a bucket), so there is
**no catch-all inside a flat dir** — the residue problem is a property of *tree*
routing (new subtrees appear). The two compose cleanly: tree-routing carries you *down
to* `/data/bigdir`, and the flat-dir partition function takes over *inside* it.

## Flat, byte-heavy: split by file

A few huge files, modest count → byte-bound, walk is cheap, and splitting works:

- Drive tar from an explicit file list (`-T --no-recursion`), one big file or a
  balanced group per shard → parallel dumpers on unbounded media (disk/S3) shrink the
  window.
- Native listed-incremental already re-dumps only *changed* files.
- A big file that *changes* re-dumps whole (tar is whole-file granularity) → route it
  to a delta/rsync-style `method`, not a splitter.

## The `nb plan --suggest-splits` decision tree

The feedback loop earns its keep by **refusing to split when splitting would hurt**,
and naming the method change instead. It runs on data already captured — seal
`FileCount` + `DumpStats`, and the pre-dump `Estimate` walk (extended with fan-out).

```
for each DLE (or catch-all / shard) whose estimate or last full exceeds T or RoomBytes,
or whose dump time exceeds the window:

  measure: bytes, file_count, fan_out (# child dirs), biggest_child

  if fan_out is high (branchy):
      → propose FRONTIER CUT: promote biggest_child to its own shard
        (recurse until residue ≤ T). Catch-all absorbs the rest by exclusion.

  elif file_count is high and fan_out is low (flat, count-heavy):
      → DO NOT namespace-split (a cut multiplies the metadata walk N×).
      → propose METHOD CHANGE: block snapshot (zfs/lvm/btrfs),
        or the manifest `filelist` method if file-level splitting is required.

  elif bytes is high and file_count is low (flat, byte-heavy):
      → propose PER-FILE SHARDS (explicit file lists, parallel on unbounded media);
        flag hot big files for a delta method.

  else (big but none of the above — e.g. one solid stream):
      → no split helps; recommend strategy:incronly / longer per-DLE dumpcycle
        (see "smaller levers" below).
```

Sample output:

```
DLE /data/projects   1.4 TB   branchy (fan-out 22)
  → split: promote /data/projects/datasets (920 GB, 88% of growth) to its own shard

DLE /data/maildir    310 GB   flat (4.2M files, fan-out 3)
  → a subtree cut multiplies the metadata walk ~Nx; do NOT split
  → recommend method: zfs/block-snapshot (or the manifest `filelist` method)

DLE /data/vms        2.1 TB   flat (12 files), 3 hot
  → per-file shards; route the 3 changing images to a delta method
```

`auto_split` (opt-in) applies the branchy/per-file proposals with forced fulls; method
changes are always operator-confirmed (they change the artifact and may need
filesystem support).

## Relationship to the smaller levers

Splitting is the heavy machinery. Amanda-style **level bumping already landed on main**
(the planner sits at L1 and climbs only on real savings), which covers the
incremental-chain-growth axis. Two cheaper, named-Amanda knobs remain to cover the
"big but unsplittable / rarely fullable" tail and should land before the splitter:

- **`strategy: incronly` / `nofull`** (per dumptype) — full a huge DLE once, then only
  incrementals. Amanda's `strategy`.
- **Per-dumptype `dumpcycle` override** — let a huge DLE full on a 90-day cycle while
  small DLEs stay at 7. Amanda's per-DLE `dumpcycle`.

Both are pure planner additions over the existing `DumpType` / planner params, no new
artifacts, and they make the splitter optional rather than mandatory.

## Recommendation / sequencing

1. **`strategy` + per-dumptype `dumpcycle`** — small, faithful, cover the tail; ship
   first.
2. **Shape-aware `nb plan --suggest-splits` (advisory only)** — reuses estimate +
   catalog + report machinery; the highest-value step because it stops bad splits and
   teaches the operator where to act. No engine mutation.
3. **Branchy frontier-cut + catch-all via the config-derived partition** (see
   [split-sources-spec.md](split-sources-spec.md), which supersedes the virtual-DLE
   expansion sketched here). The gnutar exclude-vs-deletion behavior is now **pinned**
   (`TestNewExcludeIsNotADeletion`: exclusion ≠ deletion — fail-safe), so the remaining
   work is the `Sources`/`DLE` schema change and the `recovery` recomposition, which
   must **order sibling DLEs globally by run** for most-recent-wins to resolve carve
   overlap.
4. **Flat-dir archiver routing** — wire a block-snapshot `archiver`; treat the manifest
   `filelist` archiver as a separate, later, opt-in type (it owns its own delta logic
   and sacrifices native-tar deletion semantics).

The through-line: automatic splitting's real payoff is **handing the planner small,
stable, independently-schedulable units** — which one giant DLE denies it today — and
**knowing when not to split**, routing to a different dump method instead.
