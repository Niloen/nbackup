# Implementation plan: partitioned sources

Companion to [partition-sources.md](partition-sources.md) (the spec). This is the code plan:
where each piece lands, what the codebase forces, and the phased order. Every claim is
anchored to real code.

## Flow, in real types

```
config.DLE{Host, Path, DumpType, Partition}          ← parsed; only specifiable fields
   │  (plan/dump only)
   ▼  Resolve(dles, ArchiverFor, ExcludesFor)          internal/scheduler/plan.go:18
   │    mapping (d.Partition != ""): SourcePattern{Base:d.Path, Pattern:d.Partition, ...}   (Base ⟹ rest)
   │    scalar  (d.Partition == ""): SourcePattern{Base:"",     Pattern:d.Path,      ...}
   │    arch.Expand(sp)                                             ← the one live step; fail loud
   ▼
[]Scope   (complete: Base set by the archiver; excludes baked in; a remainder carries the carve)
   ▼  wrap → []ResolvedDLE{Scope, Host, DumpType}       runtime (planner)
   ▼  planner.Build → Item(+level) → dump: BackupRequest{Scope, DLE, Level, BaseLevel}
```

Every other command (`status`, `report`, `check`, `recover`, `verify`) reads the DLE set from
the catalog (`dleDirectory.names()` over `catalog.Runs()`), never from `Resolve`.

## What the codebase forces (surprises, and how each resolves)

- **S2 — excludes are per-*dumptype*, not per-DLE** (`encode.go:174` fills `Exclude` from
  `d.exclude(dumptype)`). **Dissolved by the design:** `Expand` returns complete `Scope`s with
  excludes baked in (dumptype globs + the rest's carve). Estimate and dump consume the same
  `Scope`, so no per-DLE exclude plumbing and no double-count.
- **S3 — gnutar has no anchored-exclude support** (`gnutar.go:313` emits `--exclude=` bare).
  **Internalized:** gnutar *produces* the remainder's leading-`/` carve excludes in `Expand`
  and *consumes* them (→ `--anchored`) in `createArgs`. The convention never leaves the
  package; the engine moves opaque `[]string`.
- **S1 — overlap** (a child in both the rest's stale chain and its fresh DLE). Only a narrow
  find/dump race; recovery is per-DLE (`chain.go:52`). **Resolved by a planner guard:** force
  the rest to a full when its carve set changes. No cross-DLE recovery rewrite.
- **S4 — `config.DLEs()` feeds check/report/posture directly** (`check.go:92`, `report.go:222`,
  `posture.go:256`), which would try to stale-track the *base* `/data`. **Real work:** reroute
  those to `dleDirectory.names()` (the catalog set). `status` (run-scoped) and `recover`
  (already catalog) need nothing.
- **S5 — restore identity.** *Not* solved with `Owns()` (dropped — a fragile reverse config
  match). See the restore section: the artifact already carries the archiver **type**; add the
  archiver **name**; resolve load-bearing options from the named config definition with a clear
  error. For gnutar/postgres derived DLEs there is nothing to resolve (restore on defaults), and
  pipe never partitions — so the "derived DLE needs config" case is empty.

## The record-field change (backwards compatible)

Rename for clarity and add the name; **keep the wire key so nothing migrates**:

```go
type Archive struct {
    // ...
    ArchiverType string `json:"archiver"`               // WAS Archiver — same on-disk key
    ArchiverName string `json:"archiver_name,omitempty"` // NEW — additive, optional
    // Compress, Encrypt schemes unchanged
}
```

- Old footers/catalog entries parse unchanged (key `archiver` preserved) — no migration, in
  keeping with the repo's no-migrations rule (we simply don't change the format).
- `ArchiverName` absent on every pre-existing artifact → restore falls back to the current
  `restoreArchiver` derivation (`slug → dumptype → archiver`, `toolchain.go:201`) / bare-type
  default. New dumps write the name; restore resolves the named definition directly.
- The footer parser (`record.ParseCommit`) and `nb rebuild` (`catalog/scan.go`) read the new
  optional token; absent ⇒ empty ⇒ fallback.

## Review amendments (2026-07-08, after phases 1–2 landed)

An adversarial review against the implemented code added these requirements; they bind the
remaining phases.

- **R1 — `Resolve` MUST fail on duplicate resolved names.** Collisions are reachable three
  ways (an explicit `- /data/alice` beside `{path:/data, partition:"*"}`; nested partition
  bases where the outer's *match* slug equals the inner's *rest* slug; a selection overlapping
  a partition) and none are load-time decidable — they depend on what's on disk. A duplicate
  `Name()` after expansion means shared `.snar` state and interleaved catalog identity: hard
  error, naming both origins. (The "carve one child to its own dumptype" want this blocks is a
  legitimate future feature; v1 refuses it loudly.)
- **R2 — `encode.go`/`estimate.go` must consume the resolved `Scope` VERBATIM.** Both today
  rebuild `Scope{Source: item.DLE.Path, Exclude: d.exclude(dumptype)}`; left unchanged, the
  rest dumps *without its carves* — children silently double-dumped, no error. Phase 3 changes
  both call sites and adds a test asserting the rest's tar args contain the carve excludes.
- **R3 — landing routes resolve via `ResolvedDLE.DumpType`.** `Routes()`/`landingsForDLEName`
  scan config by slug; resolved slugs aren't there, so crash-recovery flush would fail to
  route a partition-derived archive. Also pin `ResolvedDLE.Name()` ≡ `config.DLE.Name()` for
  plain sources with a compat test (slug continuity for existing catalogs).
- **R4 — the re-baseline mechanism (REVISED 2026-07-09).** First shipped as a footer field
  (`record.Archive.Carves`) + a scheduler post-pass; Marcus's review moved it INTO GNUTAR:
  "excluded ≠ deleted" is tar-specific, so the archiver owns it. The snapshot library keeps
  a `.carves` sidecar per level (promoted with the snar); `HasBase(dle, level, scope)` —
  widened to be request-relative — reports a base unusable when the request carves a subtree
  the base did not, and the existing "no base ⇒ full" pass re-baselines. Subset test:
  additions unusable, removals fine (wholesale re-entry pinned), carve-free requests skip
  the sidecar read (no upgrade impact on plain DLEs), carves-wanted-but-none-recorded =
  the plain-to-partition migration, re-baselines once. No record field, no scheduler guard,
  no engine closure; the sidecar loss mode is a spurious full (fail-safe). Executor gained
  WriteFile (dual of ReadFile) to write it.
- **R5 — staleness needs the resolved set recorded per run.** Catalog-only staleness alone
  flags a removed-from-config DLE stale forever, and cannot distinguish a failing partition
  child from an intentionally deleted one. Record the run's resolved DLE set (additive run
  record); staleness keys on the latest resolved set, so retired DLEs drop out and
  resolved-but-not-dumped children flag.
- **R6 — resolve once per command invocation.** `Validate` and `Plan` each read the DLE set
  today; both must share one `Resolve` result per invocation (consistency + one enumeration).
- **R7 — `nb check` resolves too (DECIDED).** Shipped check is the live amcheck analogue (SSH
  executors, `CheckSource` per DLE), so it expands patterns like plan/dump and probes each
  resolved source; a resolution error is a failed check line, not an abort. The catalog-only
  rule covers check's *staleness* section plus status/report/recover/web. Spec updated.
- **R8 — operator DLE refs resolve against the resolved set on live paths.**
  `resolveConfigured` (`dles.go:128`) resolves `--dle`/force-full/reset refs against config;
  a partition child's slug isn't there, so per-child operations would fail to resolve. On
  plan/dump paths, resolve refs against the expansion result (which exists by then);
  catalog-backed read paths already accept catalog slugs via `dleDirectory`.
- **R9 — leading-`/` dumptype excludes change behavior (document).** Previously an absolute
  `exclude: /var/tmp` never matched (members are `./`-prefixed) — a silent no-op. The
  anchoring split now makes it an anchored root-relative exclude — strictly closer to author
  intent, but a behavior change for existing configs; call it out in the example config and
  docs. Also document that `*` matches dot-directories (Go `path.Match`, unlike shell) — for
  a backup tool over-matching is the right divergence, but say it. And the example config
  should lead with the partition form, so the coverage-preserving spelling is the one people
  copy; a selection's plan output shows no rest row and no coverage line — the visible cue
  that loose files under its prefix are not covered by that source.
- **R10 — R5's run-level resolved-set record is non-rebuildable.** Footers are per-archive; a
  run-level resolved set lives only in the store, joining the usage-ledger class of
  non-rebuildable extras. After `nb rebuild`, staleness degrades gracefully to
  catalog-archives-only until the next run records a fresh set. Accepted.

Fixed immediately during the review (in-tree, green): the gnutar `Expand` literal-token bug
(relative Source + dropped rest), pipe/postgres refusing the partition form instead of
mangling it, wildcard partition bases rejected at load, partition base paths `path.Clean`ed at
decode (mapping form only — scalar sources are never cleaned; a conninfo must pass through).

## Status (2026-07-09)

DONE (committed, race-green, e2e-proven): phases 1–4 plus R1, R2, R3 (routing), R4
(gnutar `.carves` sidecar + request-relative HasBase, e2e-proven one-shot re-baseline), R6, R7
(check resolves + probes resolved sources; directory-only `CheckSource`), R8 (ForceFull
accepts catalog slugs), plan rendering with partition/selection groups + coverage line.
An interim guard keeps `checkStaleness` from false-warning on selection sources.

ALSO DONE: phase 6 — `record.Archive.Archiver` renamed `ArchiverType` (wire key
"archiver" kept, so every existing footer/catalog entry parses unchanged) + additive
`ArchiverName` (the config definition name, an inert lookup key). Dumps record the name;
`restoreArchiver(type, name, dle, host)` prefers it — resolving load-bearing options
(pipe's restore_command) for ANY DLE, configured or not — with the old slug-scan and
bare-type fallbacks for pre-name artifacts, and a retyped definition never silently
applies (type must match). Pinned by TestRestoreArchiverPrefersRecordedName.

REMAINING: R5 (record the resolved set per run; staleness for pattern children + retire
removed DLEs; `report`'s staleness/drill reroute), postgres database enumeration
(selection), R9 doc pass (example config, README, `*` matches dot-dirs, leading-`/`
exclude behavior change + anchored-exclude-additions re-baseline, directory-only rule).

## Phased build order

Each phase is independently testable; earlier phases don't depend on later ones.

1. **Config surface.** `config.DLE.Partition` (`entities.go`); `Sources.UnmarshalYAML`
   accepts a scalar *or* `{path, partition}` mapping (restructure the innermost `[]string`
   decode to a per-item `UnmarshalYAML` switching on `node.Kind`); update `MarshalYAML`;
   enforce KnownFields by hand (yaml.v3 doesn't propagate it to nested `node.Decode`); validate
   (no duplicate base, base ≠ `/`, `partition` relative, no `**`). Pure, no I/O.

2. **`Scope` + `Expand`.** Add `Scope{Base, Source, Exclude}`; embed it in `BackupRequest`
   (update literals: `estimate.go:94`, `encode.go:169`, tests — `r.Source`/`r.Exclude` keep
   working via promotion). `SourcePattern` is its own struct (`{Base, Pattern, Exclude}`;
   `Base != ""` is the remainder signal — no `Remainder` bool), not an embedded `Scope`. Add
   `Expand(SourcePattern) ([]Scope, error)` to the `Archiver` interface; the archiver sets
   `Base` on each result. Implement
   gnutar (wildcard-free → 1 scope, `Base:""`; selection → `find`, `Base:`root; remainder →
   `Source==Base`, anchored carve, incl. `--anchored` support in `createArgs`) and pipe
   (identity). Testable with a local fixture tree.

3. **`Resolve` + the resolved unit.** `ResolvedDLE` in the planner layer (embeds `Scope`, owns
   `Name()`/`ID()`); `Resolve(dles, ArchiverFor, ExcludesFor)` wired into `scheduler.Plan`
   (`plan.go:18`), `Simulate`, `Validate`; fail loud. `ArchiverFor` is already in
   `scheduler.Deps`. The SSH path is untested in CI (no sshd) — cover the local-executor path;
   road-test SSH by hand.

4. **Plan rendering.** Extend `fprintPlanItems` (`cli/plan.go:128`, shared with
   `nb dump --dry-run`) to group a partition's Items under a `path — partition "…"` header with
   the `└ the rest` row and the `✓ covers 100%` line.

5. **Catalog-only reroute (S4).** Point check/report/posture staleness at
   `dleDirectory.names()` instead of `config.DLEs()`.

6. **Restore.** `record.Archive.ArchiverType`/`ArchiverName` (back-compat as above); dumps write
   the name; `restoreArchiver` prefers the recorded name → named definition, falling back to
   the current derivation when the name is absent (old artifacts) or the definition is gone;
   sharpen the load-bearing-missing error message.

7. **Rest re-baseline (S1).** Record the rest's effective carve set in its incremental state;
   force a full when it changes (near `forceFullWhereBaseMissing`, `plan.go`).

**Not doing:** `config.Source` (redundant with `config.DLE`); a stored `Restore`/command or
options in the artifact (RCE-shaped); `Owns()` (fragile reverse config match); a cross-DLE
recovery merge (the re-baseline guard covers overlap).

## Postgres, for reference

A postgres source uses the selection form (`"app_*"`); its `Expand` queries the catalog of
databases and returns one `Scope` per match, no remainder (enumeration is total). It restores
to an operator-given target, so nothing per-DLE is resolved from config at restore — the
partition machinery is archiver-neutral because enumeration and the remainder both live behind
`Expand`.
