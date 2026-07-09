# Spec: partitioned sources (plan-time discovered DLEs)

Status: design proposal, not implemented. **This is the main design for turning one config
source into many DLEs.** A source may name a wildcard; at plan time the archiver expands it
into the concrete DLEs it currently matches. The set is re-discovered every plan, so DLEs
appear and disappear as directories (or databases) come and go — no config edit when a new
one shows up.

The whole design reduces to **one shared addressing type** (`Scope`) that the archiver
produces and the dumper consumes, and **one archiver method** (`Expand`) that turns a pattern
into a list. Supersedes [split-sources-spec.md](split-sources-spec.md) (declared-shard list)
and demotes [large-dle-splitting.md](large-dle-splitting.md) to background reasoning. See
ARCHITECTURE.md for vocabulary (DLE / Run / Archive / snapshot / Estimate / planner).

## Config — three forms, one rule

A `sources:` item is **either** a scalar path **or** a `{path, partition}` mapping:

```yaml
sources:
  bigdata:
    fileserver:                    # gnutar
      - /var/log                   # plain      → one whole DLE
      - /srv/web-*                 # selection  → a DLE per match, NO rest
      - path: /data                # partition  → matches + the rest of /data
        partition: "*"
    pg01:                          # postgres
      - "app_*"                    # selection  → a DLE per database matching app_*
```

The rule a reader learns once: **the remainder ("the rest") exists exactly when you name a
base.** A glob in the path is a dynamic *list* — nothing to be the rest *of*. The mapping
names a base with `path:`, so it is fully covered and the leftover is a guaranteed cell.

| form | parses to (`config.DLE`) | result |
|---|---|---|
| `/var/log` | `Path:"/var/log"` | one whole DLE |
| `/srv/web-*` | `Path:"/srv/web-*"` | selection — a DLE per match, no rest |
| `{path:/data, partition:"*"}` | `Path:"/data", Partition:"*"` | partition — matches + the rest |

`config.DLE` gains one field, and only a *specifiable* one (the struct carries yaml tags, so
it is 1:1 with the file):

```go
type DLE struct {
    Host      string `yaml:"host,omitempty"`
    Path      string `yaml:"path"`
    DumpType  string `yaml:"dumptype"`
    Partition string `yaml:"partition,omitempty"` // NEW; its presence is the mapping form
}
```

There is **no** `Remainder` field — nobody writes `remainder: true`. Remainder is *derived*
downstream (`Partition != ""`), not stored. `Name()`/`ID()` are **not** meaningful on a
pattern declaration (a glob has no single slug); identity is a property of a resolved unit
(below), so those methods move there.

## The archiver seam — `Expand` and `Scope`

`Scope` is the shared addressing model — what one archive covers, in the archiver's own
vocabulary. It is produced by `Expand`, embedded in a dump request, and embedded in the
resolved unit: one model, three uses. (`SourcePattern`, the *input* to `Expand`, is a distinct
struct — see below.)

```go
// Scope: what one archive covers. Base disambiguates the remainder (see taxonomy) and groups
// a partition; the archiver derives it (only it can split its opaque pattern into root+glob).
type Scope struct {
    Base    string   // enumeration/partition root; "" for a plain source
    Source  string   // concrete — a dir (gnutar) · database (postgres) · command token (pipe)
    Exclude []string // content globs; a remainder also carries its carved subtrees
}
// Base == ""                    → standalone (plain) DLE
// Base != "" && Source == Base  → the rest (the remainder)
// Base != "" && Source != Base  → a match (matched child, or a selection member)

type BackupRequest struct {           // CHANGED — embeds Scope (r.Source / r.Exclude still work)
    Scope
    DLE       string
    Level     int
    BaseLevel int
}

// SourcePattern: the INPUT to Expand. Base is the named base (config path:) — its presence IS
// the remainder signal (Base != "" ⟹ produce the rest over it), so there is no Remainder bool.
type SourcePattern struct {
    Base    string   // named base (mapping form's path:); "" → selection/plain, no rest
    Pattern string   // what to enumerate; relative to Base when set, else the whole source
    Exclude []string // configured excludes to bake into every result Scope
}

// Expand — a core Archiver method (every archiver implements it; no optional capability,
// no type assertion). Resolves a SourcePattern into the concrete Scopes to dump, setting Base
// on each. Base is transient plan-time provenance — it drives plan rendering and the S1
// re-baseline, and is NOT recorded (the catalog keys on slug/host/path).
Expand(p SourcePattern) ([]Scope, error)
```

Without `Base` the remainder is ambiguous: an empty-match remainder (`partition: "*"` over a
dir with no subdirs) is byte-identical to a plain whole-`/data` source, and even a non-empty
`Scope{Source:"/data", Exclude:[…]}` can't say whether it's "the rest" or "a whole DLE with
content excludes." `Base` settles it, and — because only the archiver can split its own opaque
pattern — it must come *out* of `Expand`.

Properties:

- **Wildcard-free `Source` → exactly one `Scope`, no I/O.** So a plain path and a pattern take
  the *same* path through the system — the source layer never branches on static-vs-dynamic.
- **Each returned `Scope` is complete.** The configured excludes are baked in, and a
  remainder `Scope` also carries the carve excludes — so a dump hands a `Scope` straight into
  a `BackupRequest` with no downstream merge.
- **Enumeration is the archiver's vocabulary.** gnutar lists directories (a `find` over its
  executor); a db archiver queries its catalog. The mapping form hands `Base` explicitly, so no
  splitting is needed there; a selection's `Pattern` is split by the archiver to find *where* to
  enumerate (Postgres has no dir to find — `app_*` is a name glob).

Per-archiver behaviour of the one call:

| `SourcePattern` | gnutar (tree) | postgres (discrete) | pipe (opaque) |
|---|---|---|---|
| plain (`/var/log`, `mydb`) | 1 scope, no I/O | 1 scope, no I/O | 1 scope, no I/O |
| selection (`/srv/web-*`, `app_*`) | scope per directory | scope per database | literal → 1 scope |
| `Remainder:true` (`/data/*`) | dirs + base w/ carve | databases, no extra scope (total) | literal → 1 scope |

The remainder is a **tree** concept. A discrete archiver is already total under enumeration,
so `Remainder` is *allowed* but simply yields no extra scope — no error, no capability gate.
pipe never partitions (its `Expand` is identity), which matters for restore (below).

## Resolution — plan/dump only, fail loud

The existing `dumptype → host → [paths]` form flattens to `[]DLE` at config load, purely. A
pattern cannot: its matches are only knowable live. So the plan/dump path calls `Resolve`,
which replaces the bare `s.d.DLEs()` at `internal/scheduler/plan.go:18` (and `Simulate`,
`Validate`):

```go
func Resolve(dles []config.DLE, archFor ArchiverFor, exclFor ExcludesFor) ([]ResolvedDLE, error) {
    // per DLE (no join, no split):
    //   mapping form (d.Partition != ""): SourcePattern{Base: d.Path, Pattern: d.Partition, ...}
    //   scalar form  (d.Partition == ""): SourcePattern{Base: "",     Pattern: d.Path,      ...}
    // arch := archFor(d.DumpType, d.Host); arch.Expand(sp) → []Scope (archiver sets Base);
    // wrap each Scope as a ResolvedDLE. A failed listing FAILS the plan — no fallback.
}
```

> **Only the live-acting commands — `plan`, `dump`, and `check`'s live probes — resolve
> patterns. Everything retrospective reads the DLE set from the catalog.** `nb check` is the
> amcheck analogue and already probes hosts/sources over executors, so it resolves too and
> probes each resolved source; its *staleness* section, like status/report/recover/web, stays
> catalog-fed. Resolving needs an executor; catalog-only surfaces don't have one, so the rule
> is structural, not a convention to remember. On a resolution error `plan`/`dump` fail and
> surface it — no last-known fallback — which also removes any "empty listing → the rest
> swallows the tree" hazard by construction (`check` reports the resolution error as a failed
> check line rather than aborting).

The resolved unit is **runtime, not config** — it carries a concrete `Scope` (from the
`archiver` layer) and owns identity, so it lives in the planner layer, never in `config`:

```go
type ResolvedDLE struct {   // runtime (planner); embeds the archiver's Scope
    Scope
    Host     string
    DumpType string
    // Name() / ID() — identity lives here; a concrete unit has a slug, a declaration does not
}
```

## How the resolved DLEs are remembered — the catalog, not a stored list

There is **no separate "resolved list".** Every resolved DLE that gets dumped writes archives
into the catalog tagged with its slug + host + path (`record.Archive`), in the self-describing
commit footer (so `nb rebuild` reconstructs it from media). That *is* the record.
`dleDirectory.names()` derives "which DLEs exist" from `catalog.Runs()`. So `status`, `report`,
`recover`, `verify` read the catalog and never expand config; expansion only decides what to
back up *next*. (`report`/`check`/posture currently read `config.DLEs()` — that reroute to the
catalog set is the one plumbing chore this imposes on them.)

## How the user sees it — the rest is legible

Config is explicit; `nb plan` makes the partition and its remainder concrete, leading with the
split framing and ending with a coverage line:

```
$ nb plan

/data  —  partition "*"
  ├─ alice          full   1.2G
  ├─ bob            incr    80M
  └─ the rest       incr    40M   loose files & non-matches
  ✓ covers 100% of /data  (2 matched + the rest)
```

"the rest" is the user-facing name everywhere (plan, status, report, web) — never "catch-all".
Its stored slug is the bare base name (collision-safe: a real match is always `/data/<x>`).
A *selection* prints no rest and no coverage line (it's a dynamic list; nothing to warn about).

## Identity & incremental continuity — exclusion is not deletion

DLE names are path-derived, and incremental state is keyed by name (`HasBase(dle, level)`):

- **A new match** is a new DLE name → `HasBase` false → a free level-0 full on first
  appearance. Until catalogued its bytes lived in the rest, so it was never uncovered.
- **A removed match** stops being emitted; its archives age out under `retention.Floor`.
- **The rest's exclude set changes** as matches come and go. GNU tar does *not* treat a
  newly-excluded on-disk subtree as a deletion (`TestNewExcludeIsNotADeletion`): it records
  "present, not dumped", so the rest's chain retains the pre-carve copy — the fail-safe
  direction (carving can never *drop* data).
- Overlap only arises from a narrow find/dump race, and is resolved by **re-baselining the
  rest when its carve set grows** — implemented entirely inside gnutar: the snapshot library
  keeps a `.carves` sidecar beside each `.snar` (promoted atomically with it), and `HasBase`
  judges a base built with fewer carves than the request unusable, which routes through the
  existing "no base ⇒ full" path. "Excluded ≠ deleted" is a tar-semantics fact, so the
  knowledge lives with tar; the generic layers never compare carve sets, and nothing is
  recorded in the artifact. Additions only — a REMOVED carve re-enters the chain wholesale
  (pinned by test) — and a plain DLE converted to a partition base re-baselines once (its
  base has no recorded carves). Cheaper than, and in place of, any cross-DLE recovery merge.

## Restore — the artifact is self-describing; config only for load-bearing options

Restore reads almost everything off the artifact, no config: the compress/encrypt **schemes**,
the stream **shape**, the **chain**, every byte's **placement**, and the archiver **type**
(which plugin reverses the stream). gpg public-key decrypt needs nothing from config (key from
the keyring); a symmetric passphrase is operator-supplied.

The one thing not self-contained is a **load-bearing archiver option** — pipe's
`restore_command`. We deliberately do **not** store it (or any command) in the artifact: data
that becomes an executed command at restore is an RCE-shaped foot-gun regardless of checksums.
Instead the artifact records the archiver **name** — an inert lookup key — and restore resolves
the option from that named config definition, or fails with a clear error:

```go
type Archive struct {
    // ...
    ArchiverType string `json:"archiver"`             // RENAMED from Archiver; wire key kept
    ArchiverName string `json:"archiver_name,omitempty"` // NEW, additive/optional
    Compress     string // scheme (exists)
    Encrypt      string // scheme (exists)
}
```

- **Type** is intrinsic and always present → pick the plugin. gnutar/postgres restore on
  defaults, so a partitioned child restores with zero config lookup.
- **Name** disambiguates *which* named definition's options apply (several `pipe` definitions
  can share the type). Restore resolves options from it; if a load-bearing definition is
  absent → *"produced by pipe archiver `X`; its `restore_command` isn't in the current config."*
- Because pipe never partitions, a pipe DLE is always 1:1 with config anyway — so the
  intersection of "derived DLE" and "needs a config option at restore" is empty. The name +
  clear-error stance is really for renamed/removed definitions and fresh-machine restores.

**Backwards compatibility (field rename).** Keeping the wire key (`json:"archiver"`) means old
footers and catalog entries parse unchanged — nothing to migrate. `ArchiverName` is additive;
old artifacts lack it, and restore falls back to the current `slug → dumptype → archiver`
derivation for them. The fallback stays until no un-named artifacts remain.

## When *not* to partition

Partitioning divides a **tree** into subtree DLEs. It does not help — and can hurt — one flat
directory of millions of small files (N buckets = N× the metadata walk). That workload wants a
different `archiver` (block snapshot, or a manifest archiver), a registry registration, not a
partition. The two compose: a partition carries you down to `/data/bigflatdir`, a flat-dir
archiver takes over inside it. Full shape reasoning in
[large-dle-splitting.md](large-dle-splitting.md).

## No-catch-all (selection) — coverage is opt-in

The scalar-glob form deliberately has no remainder: it's a dynamic list of exactly the matches,
and — like a hand-written list — it does not warn about what it omits. Coverage is opt-in by
naming a base (the mapping form); a selection is the explicit "just these" choice, and the
`find` returns only the matches (we never list the parent, so there's nothing to warn from).
