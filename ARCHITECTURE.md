# NBackup — Architecture & Decisions (agent orientation)

This is the internal map for anyone (human or agent) working *on* NBackup. The
[README](README.md) is the user-facing front page; [PRD.md](PRD.md) is the product
vision. This document carries what the code and README don't make obvious: how the
concepts nest, the load-bearing decisions and *why*, and the conventions for working
in this repo.

NBackup is a run-based backup system in Go whose design descends from Amanda. It
orchestrates external tools (GNU tar, a compressor) as child processes rather than
reimplementing them, and produces immutable, self-describing artifacts that restore
with stock tools. The package map below maps NBackup concepts to their Amanda
equivalents (the "Amanda analogue" column) for readers coming from Amanda; the rest
of this document otherwise describes NBackup in its own terms.

## Vocabulary (how the concepts nest)

- **DLE** — a backup source (`host` + `path`).
- **Run** — one planner execution (typically daily) *and* the immutable, sealed set
  of archives it produces: the primary artifact, the addressable unit for copy /
  restore / list. (The verb and its result share one name because they map one-to-one
  — "last night's run" is both.) (`run-YYYY-MM-DD.HHMMSS` — the run's local calendar
  date plus its fixed-width start-of-run wall-clock time, Amanda's dump datestamp. Minted
  from the clock, never allocated: a pruned run's id is never reused, and the ids sort
  chronologically as plain text even as an object-store key.)
- **Archive** — one **DLE** image at one level inside a Run. The unit of
  **retention/pruning**: the floor and disk/cloud reclamation are per-archive, so an
  old run can shed one DLE's image while keeping a run-mate the chain needs. Browse
  the per-DLE timeline with `nb dle`.
- **Cycle** — the safety/scheduling boundary: every DLE is fulled once per cycle.
- **Medium** — a named storage definition; opens as a **Volume**.
- **Volume** — an ordered sequence of header-framed, self-describing files
  addressed by position. Disk, tape, object stores all map to it.
- **Catalog Entry / Placement / Part** — the catalog separates *what a run is* (one
  medium-independent `Entry`, from the seal) from *where its copies live* (N
  `Placement`s, one per medium). A placement holds each archive's ordered **parts**
  (a part = `volume + position`; one part unless the archive spanned volumes) plus the
  seal's location.
- **Label** — logical identity written on a labeled volume's file 0 (magic, name,
  pool, epoch). A *capability* (`media.Labeled`); address-identified media skip it.
- **Slot / Drive / Barcode** — a tape changer's physical elements (unrelated to a
  **Run** — this is the Amanda/library sense of "slot", the only place the word
  survives). A **slot** (storage
  element) holds one cartridge; a **drive** (data-transfer element) reads/writes the
  cartridge currently loaded in it; a robot (or a human) moves cartridges between them.
  Each slot reports a physical **barcode** — the scanner's identity, read *without*
  loading — distinct from the Label written inside the cartridge (read only once it is
  in a drive). A real single drive a human hand-loads (`device:`) has no addressable
  slots — its off-drive cartridges sit on a shelf the software cannot see.

## Package map

Mechanism lives behind interfaces with named, registered implementations; one
orchestrator (`engine`) composes them. Adding a medium, archiver, or compression scheme is a
registry registration, not a conditional in the core.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | disklist / dumptype / storage |
| `record` | the self-describing on-medium artifact records: `Header` framing + `Label` (volume id record) + `Archive` (commit-footer metadata, self-locating via its `Run` tag) + the run-id vocabulary (`IDFromTime`/`ParseID`/`RunIDLess`) + the per-archive member index (`EncodeIndex`) + their (de)serialization — only what is bytes on a medium | dumpfile_t / amar |
| `archiveio` | the block layer: maps a run's archives onto a `Volume`'s files. Two bound ends — `Writer` (one run's write end, bound to a `PartAllocator` + `Recorder`; `NewArchive` → the `ArchiveWriter` SDK) and `Reader` (one medium's read end, bound to a `PartOpener`; `Open(ref, parts)` → `io.ReadCloser`) — that meter + split a payload into parts then write its index + commit footer, and concatenate + assert parts on read; knows nothing of compress/encrypt. Home of the value objects `Ref` (an archive's logical identity — the fs "filename", asserted against part headers on read) and `FilePos`/`ArchivePos` (where an archive's files landed) | taper / amrestore |
| `media` | `Volume` + `Labeled` + `Drive`/`Changer` (slots + drives, `Load`/`Unload`/`Manual`) + `Profile` + registry; reads/writes `record` artifacts | Device API |
| `librarian` | operates a medium's `Changer` + label protocol (make-writable, advance, load-for-read, label, load); prompts an operator when the changer is `Manual`; its drive-bound `Allocator` implements `archiveio.PartAllocator` (part sizing + the volume roll) | changer / amtape |
| `depot` | where media are opened — open(2) + the mount table: resolves a configured medium to a typed face (`ReadMedium`/`WriteMedium`/`AdminMedium`, the access rule as a method set), owns the run window's exclusive write claims, the lazily opened landing volume + one-time catalog bootstrap, and the per-medium write knobs (part size, bandwidth cap) | — |
| `media/disk`, `media/tape`, `media/cloud`, `media/gdrive` | Volume impls (disk sidecar headers; tape library; object store via gocloud.dev/blob; Google Drive via google.golang.org/api/drive/v3) | vfs / tape / s3 devices |
| `media/fslike` | the run layout shared by the address-identified media — clean payloads + `.hdr` sidecars over a small `Store` seam (disk = a directory, cloud = a bucket, gdrive = a Drive folder tree), so disk↔cloud↔gdrive copies are byte-identical | — |
| `archiver` + `archiver/gnutar`, `archiver/pipe` | `Archiver` interface + registry + named definitions; owns its incremental-state library; GNU tar impl + the command archiver (operator-supplied producer/consumer commands, full-only, opaque stream) — the archiver roadmap and capability model are in docs/design/archivers.md | Application API / amgtar, amraw |
| `transform/compress` | external compressor child processes (zstd/gzip/none) + registry; `Filter(scheme)` returns the forward/reverse `programs.Cmd` | compress |
| `transform/crypt` | external encryptor child processes (gpg/none) + registry; `Filter(scheme)` returns the forward/reverse `programs.Cmd` | amcrypt/amgpgcrypt |
| `programs` | the base: a `Cmd` (external program to run) + an `Execution` transport that runs a pipe of commands on one host, transparently `Local` or `SSH`; the command/execution concept compress, crypt, and the archiver all build on | amandad (replaced by stock sshd) |
| `xfer` | the data-movement primitive: `Transfer(source, filters, sink)` moves one stream through three zones — a `Source` (a client's tar, or a medium read), local `Filters` (compress/encrypt or decrypt/decompress), and a `Sink` (a medium, a target's tar, a hash) — tagging faults by zone | Amanda Xfer / netusage |
| `archivefs` | the archive fs (file layer): a logical `archiveio.Ref` to a byte endpoint and back, nothing more. Its two faces live beside their implementations — `ReadStore` (implemented by `FS`: `OpenArchive` with copy selection + fail-over, `OpenArchives` one-pass ordered reads, `Members`) and `WriteStore` (implemented by `Session`, one run's write handle: `Record` + staged read-back/`Reclaim`) — plus `Ingest` (the producer's writer intake, implemented by the spool). Mounts volumes via depot read media; owns the member index. No schemes, no tar, no transfers — those live in the operations (dumper, restorer, verifier) | Scribe + Recovery::Clerk |
| `progress` | live run-status model + status-file I/O + render | amdump log / amstatus |
| `report` | per-run history record + JSONL/summary file I/O + digest render | amreport |
| `notify` | pluggable alert backends (smtp/sendmail/webhook) + registry + dispatch | amreport mailto |
| `catalog` | local cache of run index + volume registry; owns the in-memory `Run` grouping (id + its archives, with derived `Date`/`TotalBytes`/`LastArchiveAt`) and exposes archive projections (`Archives`/`ArchivesOn`) for the policy layer; derives `History` | catalog / curinfo / tapelist |
| `retention` | retention safety floor: protected archives — `Compute` over `[]record.Archive` returns a per-`(run,DLE)` `Floor` (`.KeepsArchive`, plus run-level `.Keeps`/`.First`) (pure) | policy |
| `recovery` | pure recovery planning: the archive chain to rebuild a DLE as of a run (`Chain`), as-of date/time resolution (`AsOf`), the browsable as-of tree + per-archive file selection, and the interactive browse session state | amrestore / amrecover (planning) |
| `restorer` | the read-side operation package (mirror of `dumper`): `Extract(Request)` — the one whole-DLE chain restore behind `nb recover --all` *and* the drill's chain tier — plus `ExtractSelection` (file-level recovery) and the decode primitives verify reuses. Written over `archivefs.ReadStore` + narrow resolution funcs, so it tests over fakes | amrestore / amrecover (execution) |
| `drill` | recovery-drill ledger + risk-biased selection + failure taxonomy (pure) | amverify (orchestrated) |
| `planner` | multilevel level scheduling (pure) | planner |
| `dletree` | path-arranged display grouping of DLE identities (pure leaf): recovers a partitioned source's structure from names alone, so the report, `nb dle`/`nb plan`, and the web fold its many long-pathed DLEs under one `host:base` header with relative labels | amreport columnspec (display concern) |
| `accounting` | medium capacity/retention/prune arithmetic: what a medium holds against its capacity, the protected residual a prune can't reclaim, the expected next volume + per-run room, and the prune/reclaim mutators — plus the dollar-cost overlay (`accounting/cost.go`) that prices the same bytes | (driver, capacity half) |
| `scheduler` | the engine-side **driver** that feeds the pure `planner` its config/history/capacity inputs + the parallel size estimates, then applies the impure force-full post-pass `planner` can't (it probes the archiver's on-disk incremental state); also validates a run's config for previews | driver (planner front-end) |
| `spool` | the run's concurrent write seam: routes each archive direct to its landing or through the holding-disk `Pool`, per-landing `writers` permits, one orchestrator goroutine as the sole catalog writer | taper (holding-disk half) |
| `conductor` | the backup-run lane: executes one plan into one sealed run — flush leftovers (`Flush`, the crash-recovery drain), pre-flight tools, alloc run id, open the landing writer, run the `dumper` + `spool`, seal | driver (dump half) / amflush |
| `engine` | the composition root + command facade: wires everything and owns almost no behavior. One in-package resolution service — the `toolchain` (hosts/executors/archivers/transform options) — plus the constructed `depot` serve in-package lanes (`verifier`, `copier` incl. sync, `driller` incl. posture, `checker`) and the split-out lanes (`accounting`, `scheduler`, `conductor`, `dumper`, `restorer`, `archivefs`) | driver |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, retention, archiver, xfer,
archivefs, archiveio, depot, catalog, config, progress, recovery, restorer}` over leaf
packages `{media, programs, sizeutil, dletree}`, all bottoming out on `record` (the on-medium
artifact format that `media`, `archiveio`, and `catalog` read and write; `archiveio`
adds the value objects — `Ref`, the archive identity both the block layer asserts and
the fs resolves, and `FilePos`/`ArchivePos`, its file locations; `restorer` executes
what `recovery` plans, over the `archivefs.ReadStore` the FS implements). The reporting layer adds `cli → {report, notify}` with `notify → {report,
config}` — `report` is a pure leaf (record + render); the engine depends on neither.
Domain packages stay pure; `archiver`/`media`/`transform/compress`/`transform/crypt`
are pluggable adapters; `engine` is the only component aware of all of them. A backup
is an **`xfer.Transfer`** composed over the **`archivefs`** endpoints (engine
orchestrates, the fs moves the bytes): a **Source** (`tar`) → local **Filters**
(compress/encrypt) → a **Sink** (the medium, via `archiveio` meter + split into parts);
restore reverses it, and copy/verify/drill are Transfers with different endpoints — all
composed from the same two archiveio-coupled endpoints and the one `DecodeFilters`
builder. (The
zone model and remote placement are detailed under "Execution is an injected transport"
below.)

## Load-bearing decisions (the *why*)

**The archive is the commit unit; the run is a grouping.** Each archive is made
durable by its own **commit footer** (`KindCommit`), written last — after its parts
and its member index — so the footer's presence proves the whole archive landed.
The footer holds the per-archive **integrity** the framing headers omit (`SHA256`,
sizes, part count); the member list rides in a separate per-archive **index**
(`KindIndex`, gzip), kept out of the footer so a scan reads only small footers. There
is **no per-run seal**: a run is just the run-id its archives carry (the `Run` tag
on every header and commit footer), reconstructed by grouping committed archives. The
`Run` grouping itself is an in-memory `catalog` type, not an on-medium record; the
policy layer (retention/recovery/drill/reclaim) reasons over the self-locating
`[]record.Archive` directly, so a run is purely a catalog + tagging concept. This drops all-or-nothing run
atomicity (we considered and rejected keeping the seal): a crashed run keeps every
*committed* archive, a rerun fills in the rest — "run complete?" is a derivation (did
every planned DLE commit?), not a stored bit.

**Partial writes are tolerated, never repaired.** A hard kill or power loss
mid-write can leave uncommitted bytes on a volume: a payload with no header
sidecar, a torn sidecar, a half-framed tape record, an archive's parts with no
commit footer. Two layers absorb this without deleting anything (delete is
impossible on WORM): the **commit footer** is the archive's marker — an archive
with no footer is never assembled into the catalog (`assemble` iterates commits;
parts without one are orphans), so its parts are simply unreferenced. Beneath it,
each medium's `Files()` enumeration treats *any* artifact it cannot read or parse
as uncommitted and **skips it**, so a single torn file can never abort `nb rebuild`.
The commit test differs per medium (fslike: payload paired with its later-written
sidecar; tape: a decodable framed record), so it lives in each medium, not a shared
layer. Orphans are never reclaimed on read. On a per-file medium (disk, cloud) `nb
prune` sweeps them: it removes the footer-less parts and torn files no committed
archive references, detected from the medium's *own* commit footers (never the cache,
so a stale catalog can never make a committed archive look orphaned) and bounded by
the same `minimum_age` retention uses, with a refused delete tolerated so it never
fights WORM/Object-Lock. On a whole-volume medium (tape) orphans wait for relabel.
Integrity of files a footer *does* commit (bit-rot) is verify's job, not the rebuild's.

**The catalog is a cache; the media are the source of truth.** Every file is
self-describing (header), every archive carries its own commit footer, every labeled
volume carries its label — so one `Files()` scan rebuilds everything (`nb rebuild`):
commit footers + indexes → archives grouped into runs, labels → volume registry.
The **volume set is self-describing too**: the footer carries the archive's
**part map** (`Archive.PartMap` — each part's volume label, epoch, and position,
the archive's TOC), so a scan holding only ONE tape of a spanned archive still
records the complete placement, restores prompt for absent reels **by label**, and
`nb rebuild` is **additive by default** — feed tapes one at a time and each pass
merges in (a scanned reel is whole-volume truth: records referencing its label at
an older epoch drop; unscanned reels keep their records) while the report lists
the tapes still missing (`Catalog.MissingVolumes`, from TOC references without a
registry entry) and the runs whose footer tape has not been fed yet
(`RebuildReport.OrphanRuns`). `--full` wipes first — the reconciliation of last
resort. The
catalog lives in its **own `workdir`** (default `nbackup-catalog`), a cache over the
whole pool *independent of any medium*. The `Entry`/`Placement` model means a run
copied disk→tape is one Entry with two Placements; restore/verify pick any available
copy. Run `History` is *derived* from cached runs (no second source to drift) — the
catalog holds **no** precious state, every byte rebuildable from the media. (The one
piece of non-derivable local state — an archiver's incremental-state library — belongs
to the **archiver**; see next.)

**Incremental state belongs to the archiver, not the catalog.** The base data an
incremental builds on (for gnutar, the listed-incremental `<state_dir>/gnutar/…/L<n>.snar`
library) is the only state *neither* on the media *nor* derivable from it. It is owned by
the `archiver` package: the generic `Archiver` interface speaks only
`DLE`/`Level`/`BaseLevel`/`HasBase` — never a snapshot path — and each archiver keeps its
state under a root the engine hands it. This sharpens the catalog's claim to *no
exception*, makes the `Archiver` interface pluggable (a future `ampgsql`-style archiver
keeps its own state however it likes), and gives **remote sources** a seam: when `tar`
runs on the client, that root is a client path, and nothing in catalog/engine/planner
changes. The state is precious — losing a DLE's base forces its next run to a full (the
drill posture warns).

**A dump's new state enters the library only when its archive commits.** tar writes the
next snapshot to a `.new` side file seeded from the committed base, which it reads but
never mutates; the `BackupSource.Promote` hook renames `.new` over the live snapshot only
after the archive is durably committed to the dump medium (Amanda's rename-on-success,
bound — as in Amanda — to the dump landing, not the later flush). So a dump killed
mid-stream (out of space, a signal) leaves the base intact and a retry at the same level
still works, and `HasBase` rejects a present-but-empty snapshot (the corpse a killed dump
of an earlier design left behind) so a dead base can never masquerade as usable. When a
planned incremental has no usable base — missing, empty, or a moved `state_dir` — the
engine **forces a full** with a warning rather than failing or dumping a full-sized
"incremental" onto nothing (`forceFullWhereBaseMissing`, again Amanda's level-0 fallback).

`nb reset <dle>` is the deliberate version, and it is **archiver-independent**: rather than
reaching in to delete a snapshot, it records a per-DLE **force-full directive in the
catalog** (`DLEMeta.ForceFull`) that the planner honors as a mandatory level 0 — the peer
of Amanda's `amadmin force`, expressed as catalog state instead of a `curinfo` flag. The
directive is the one piece of catalog state that is *not* media-derived (operator intent
can't be scanned back), so it is kept in the cache file beside the entries and preserved
across `nb rebuild`; a run consumes it. Forcing the level rather than deleting state means
the archiver reseeds itself on the L0, and — with commit-bound promotion — the existing
chain stays intact until the new full actually commits.

*Where* the root lives is a **host** property, not the archiver's: a `state_dir`
configured per host (`hosts.<h>.state_dir`), with a fleet-wide default
(`state_dir:`) and, unset, `nbackup-state`. It is deliberately a dedicated location
**beside** the catalog workdir, never beneath it — the workdir is the disposable,
rebuild-from-media cache, so nesting the one piece of precious, non-rebuildable state
inside the thing operators wipe to rebuild would invite its loss. The engine gives each
archiver a private subtree of the host root, namespaced by archiver type
(`<state_dir>/gnutar`), so multiple archivers sharing a host cannot collide. This is a
**deliberate divergence from Amanda**, which keeps the listed-incremental dir as a
per-application property (`GNUTAR-LISTDIR`): NBackup makes it host-level and
archiver-agnostic so a multi-archiver host configures it once, matching the `Archiver`
interface that already generalizes incremental state via `HasBase`. Archiver-specific
*format* knobs whose value still varies per host (gnutar's `tar_path`) stay archiver
properties, overridable per host under `hosts.<h>.archivers.<type>` — Amanda's
property-bag model, expressed server-side.

**Incrementals sit at a level and climb only on real savings.** A DLE does not gain a
level per run. After a full it sits at level 1, re-dumping everything since the full
each run, and climbs only when it has held the current level for `bumpDays` runs *and*
the next level would save at least `bump_percent` of the full size
(`planner.chooseIncrLevel`). The threshold is a percentage so one knob fits every DLE
regardless of size, and since the saving from climbing shrinks as levels deepen, level
1 stays the common case and deep levels are rare. The runs-held guard is *hysteresis* —
a climb rests on a level's demonstrated steady state, not one run's estimate — not a
redundancy mechanism: redundancy is copies to a second medium (multi-landing routes,
`nb sync`), which protect the full and incrementals alike. Two payoffs over a naive
per-run climb: restore chains stay short, and consecutive same-level incrementals
*overlap* and stay independent, so losing one costs at most its own restore point,
never the runs after it. A level-`L` dump bases on the `L-1` snapshot,
so repeating a level just re-derives `L`.snar from the unchanged `L-1`.snar.
`recovery.Chain` is a **per-level restore**: it replays exactly one archive per level —
the tip (most recent dump at or before the target) walked back along each incremental's
recorded `BaseRun` to the full — so a redundant same-level repeat is skipped. This is
not merely non-minimal: GNU tar's directory directives (rename, delete) are *not*
idempotent across independent incremental extractions, so a second cumulative `L1`
carrying the same `rename old → new` would abort the chain (`tar: Cannot rename …`).
Walking `BaseRun` keeps the chain *consistent* (each step's base is the exact dump it
derives from), and a `BaseRun` whose run has been pruned is a **broken-chain error**,
never a silent partial restore. The overlap fallback (restore an earlier cumulative
incremental's point in time when the tip's copy is unreadable) is a deferred
recovery feature, not the normal restore path.

**Recover needs no index server.** Browsing per-dump path lists without reading the
medium is done by keeping each archive's **member list as its own gzipped index**
(`KindIndex`) on the medium, which the catalog caches into `Archive.Members` (read
eagerly on scan today; a lazy server-side index cache is the planned optimization), so
`nb recover` browses by reading the catalog alone — media is touched only on extract.
`recovery.BuildTree` merges the restore chain's member lists in run order
(most-recent-wins), giving an as-of-date filesystem where each path resolves to the
archive that last held it; `Collect` turns a selection into the fewest per-archive
extractions (one tar run per source archive, exact members via `--no-recursion`). Two
splits from a whole-DLE restore: selected-file recovery extracts the named members in
*plain* tar mode and **never applies deletions** (you get exactly what you ask for),
whereas a chain restore uses `--listed-incremental` to honor them; and the browse
tree is a union, so a file deleted at a later incremental still appears (the member
index records additions, not deletions — that lives in the snapshot).

**Verify is the primitive; drill is the orchestration (`nb drill` = recoverability,
not just integrity).** `nb verify` stays atomic and **stateless** — it checks
individual runs/archives against the seal and writes nothing, keeps no ledger, makes
no selection. Its one `--deep` structural mode streams an archive through the real read
pipeline (decrypt → decompress → `tar -t`, list not extract) and asserts the pipeline
completes and the members match the seal — proving the bytes are a valid *restorable
stream* and exercising the key + scheme, still side-effect-free. It emits a structured
per-archive verdict (`engine.VerifyReport`, classified with `drill.Class`) the drill
layer consumes. `nb drill` is the layer on top: it *selects* a risk-biased subset,
*exercises* each at a tier (the `chain` tier restores to scratch via the same
`restorer.Extract` that `nb recover --all` runs — the drill rehearses the *actual*
restore path, never a copy of it, and classifies the outcome purely from the
returned error: the missing-copy/volume sentinels via `errors.Is`, the role-tagged
`xfer.Error` via `errors.As` (a Sink fault is a composition fault, Chain; the rest
Pipeline); `stock` runs the documented one-liner, deliberately *not* NBackup's code),
*records* an inspectable ledger (`drill-ledger.json`, atomic temp+rename, no daemon),
*classifies* failures, and *exits non-zero*. Drill delivers the **"0 errors"** digit of
3-2-1-1-0. (The tiers, selection criteria, failure classes, and attended/unattended
modes are documented user-side in the README.) Pure parts (ledger, selection, taxonomy)
live in package `drill` (a leaf, like `retention`/`recovery`); the tier I/O runs
through `restorer` and the verifier, orchestrated by `engine` (which imports `drill`);
the tier orchestration is the engine's in-package `driller` lane; the WORM probe + posture audit ride with it in `engine/posture.go`. The
architectural point of the two run modes: an unattended drill that would need a tape
swap *skips* (coverage warning) rather than exiting non-zero, so a sampled nightly drill
rotates the fleet without paging on a tape that isn't loaded.

**Drill detects immutability; it never sets it (WORM probe).** The 3-2-1-1-0
"1 immutable" digit is verified, not configured, by NBackup: a drill keeps **one fixed
probe object** on the drilled medium and each run attempts to delete it — a refused
delete proves the storage enforces WORM/Object-Lock (the persisting probe *is* the
proof); a successful delete proves it does not (the probe is recreated next run, so an
immutable medium accumulates exactly one undeletable probe, not one per drill).
Immutability is configured operator-side (S3 Object Lock, LTO WORM) and NBackup runs
least-privilege, only detecting it (see memory `nbackup-immutability-cloud-side`).
Append-only media (tape) are immutable by construction and reported without a probe.
Cost: an encrypted+compressed archive is all-or-nothing to read, so an offsite drill
spends the full bytes in egress — routine offsite drills default to the no-write
`structural` tier, and the dry-run forecasts the egress.

**Sync is batch copy, not a new subsystem (`nb sync`).** A single-run `CopyRun`
already streams a run from one medium to a target and records a second `Placement`
(idempotent: a run already on the target is skipped). `nb sync` is that looped over a
*selection* of source runs — every run the target is missing, **oldest-first** (a
contiguous, replayable offsite copy; a run's full lands before its incrementals). It
reuses `CopyRun` verbatim — same label verification, placement record, per-run
atomicity — so an interrupted or repeated sync resumes for free, and it **stops at the
first hard error** (a full or offline target won't fix itself by continuing). The
source defaults to the landing medium but is configurable (`--from` / rule `from:`):
`CopyRun` resolves the source placement and mounts it via the same
`Librarian.MountForRead` path `readerFor` uses, so a tape/S3 source works (un-vaulting
tape→disk, or a second offsite tier), and copy-to-landing is allowed (the old "target
is the source" guard became a `from == to` guard). The config `sync:` rules are the
declarative form (`{from, to, last}`) so a cron `nb dump && nb sync` mirrors offsite
hands-off. An auto-source sync (no `--from`/`from:`) is additionally bounded by the
*target's own retention window* (`engine.owesTo`: within the target's `minimum_age`,
or on the DLE's live recovery chain) — media of different sizes keep different
depths, and an archive the target's prune may delete is never copied back (that
would churn: sync restores it, the next make-room reclaims it). Coverage judges the
same bound (`CopyAged`), so a pruned copy reads as rotation, not "missing"; an
explicit `--from` remains a whole-source mirror. Sync and pruning are otherwise
independent (see Reclamation asymmetry): a copy reaching another medium never makes
the original prunable. (The user-facing
oldest-first / `--from` / tiering recipe is in the README.)

**Holding disk = a marked medium, the orchestrator on the main goroutine (`media.<m>.holding:
true`, `engine/engine.go` + `engine/flush.go`).** Amanda's holding disk — a fast scratch disk
that absorbs parallel dumps and feeds one tape drive at disk speed — is opt-in by **marking one
or more disk/cloud media `holding: true`**. It is a **write-path buffer**, not a retention tier:
the configured `landing` (tape/S3) stays the authoritative, catalogued/retained/planned medium;
the dump just flows *through* the holding disk(s). In a holding-disk run the **dump workers
become background goroutines** that write each archive to a holding disk (an unbounded fslike
sink — the only kind safe for concurrent `WriteArchive`; that is also why the medium must be
disk/cloud, not a spanning tape), and the **run's main goroutine is the orchestrator** consuming
the committed archives over a channel and handing each to a **drainer** goroutine that
copies it to the landing (the medium's `writers` cap — one write-concurrency lever per medium,
shared by drains, direct dumps, and holding-disk staging alike; default: a serial library's
drive count, else the worker count — bounds how many write it at once); the orchestrator
then records the landing placement and reclaims the disk copy. With several holding disks the **`spool.Pool`** allocates a disk per dump round-robin,
so the dumpers spread across spindles (more aggregate write bandwidth + a larger combined
buffer); the handoff carries which disk so the drainer reads, reclaims, and releases the right
one. The pool, sized to each disk's `capacity`, back-pressures the dumpers (the next allocation
blocks while every disk is full); a landing failure aborts it so the dumpers stop and the run
fails — never overfilling a disk or dropping data. A DLE estimated too big for **every** disk is
**size-routed** straight to the landing (the orchestrator dumps it via the drainer), so an
oversized DLE never monopolizes the buffer. The load-bearing concurrency decision: **the
orchestrator is the sole catalog writer.** The drainer does only catalog-free byte I/O; the
landing Writer's control calls (`NextPart`/`PlaceFile`, where a volume roll touches the
catalog and drives the librarian) **funnel back to the orchestrator through routed
`PartAllocator`/`Recorder` seams**, so all catalog writes — holding placements, landing placements, volume labels —
stay on one goroutine. Since the dumpers only queue (never touch the catalog), the catalog needs
**no lock and no per-run actor**; it stays the single-threaded plain store "One mutating `nb`"
already assumes (the flock still serializes *processes*). The orchestrator records each archive's
**holding placement** as it commits and removes it on drain, so the holding disk is **visible in
the catalog live** and a crashed run's un-flushed archives are recoverable **without a media
scan**: `nb flush` (and an auto-flush at the next `nb dump`) gathers the staged runs across
every holding disk and drains them to the landing. The default direct-to-landing path is
untouched (no holding disk = no drainer, no goroutine hand-off, record-at-`Finish` as before).

**A run may write several landings at once — per-DLE routing (`dumptypes.<t>.landing`).** A
dumptype can name a `landing` medium (validated against `media`, holding media rejected) that
overrides the config-wide `landing` for its DLEs (`config.LandingFor`), so heterogeneous sources reach
heterogeneous storage in one run — bulky media to cheap cloud, databases to fast disk/tape. This fills
a gap `nb sync` cannot: sync *adds* offsite copies, but only a landing override *withholds* a big DLE
from an expensive medium. The run keeps **one identity**: one run id across every landing, each
archive recorded as a per-medium `Placement` (the `Entry`/`Placement` split already models a run whose
archives live on different media — routing is the easy case where each archive has exactly one home).
The spool generalizes from one landing to a **set of landing lanes**: the dumper resolves
each DLE's landing and asks the spool for `Ingest(landing)`; per-landing state (permit `Writers`, pending
copies/permits) lives on a `*lane` value object the **one** orchestrator owns exclusively — still
the sole catalog writer across every landing, no per-lane goroutine, no map lookups on the hot path
(each request carries its `*lane`). Drain copies to independent landings run in parallel (a copy
goroutine per dispatch, bounded by each lane's `Writers`); the global worker clamp is gone — a serial
tape's single permit parks its producers (off the dumper's gate, holding no worker) while cloud-bound
producers run. Crash-recovery `nb flush` re-resolves each staged archive's landing route from config
(`landingsForDLEName`) so a multi-landing crash drains each DLE back to its own media. *Deferred*: a
manual tape swap mid-roll still freezes the orchestrator (hence all landings) because the librarian
fuses the operator prompt with the catalog reconcile in one critical section; the non-blocking fix
(invert `archiveio.PartAllocator`→`UseVolume` so the orchestrator drives a parkable roll, with a
spool-side operator goroutine serializing prompts) is planned but not built.

**A DLE may fan out to several landings — `landing: [s3, gdrive]` (`config.MediumList`,
`spool.drainTo`, `archiveio.Tee`).** The landing route (config-wide and per-dumptype) is a
**list of media, primary first**; every archive on the route is written to all of them **from
local data** — Amanda 3.4's multi-storage model, and the answer to "add a second cloud without
paying the first cloud's egress" (`nb sync` from S3 re-downloads what was local moments
earlier; sync remains the backfill tool for *history*). The **primary** (first entry) stays
"the landing" for every single-medium consumer — accounting/capacity/cost, read preference,
posture, drill, sync's default `--from`, prune classification — so fan-out touches only
routing, drain, and flush. Two write paths: **staged** (a holding disk absorbs the dump once,
then one drain per landing copies it out in parallel; the staged copy is reclaimed by the LAST
drain, so it outlives the fan-out until every landing is served — `spool.drainSet`), and
**direct** (`archiveio.Tee` fans the stream into a bare `ArchiveWriter` per lane, all cutting
parts at the SAME boundaries — the minimum cap — so no re-parting and identical per-part seals
across copies, which is precisely `OpenRange`'s aligned-seals condition; drives lease in
sorted-name order so overlapping routes cannot deadlock). The `Ingest` seam returns the
**`archivefs.ArchiveSink`** interface (the five methods a producer drives) so the bare writer
and the tee are interchangeable. **Failure is any-lane-suffices**: a failed landing is
*tripped* for the rest of the run (no per-archive retries against a down medium), the run
continues and succeeds with a loud warning naming the repair (`nb sync --to <landing>` — the
per-medium placements already record exactly what is missing, so nothing new is persisted);
only an archive whose ENTIRE route is dead aborts the run (staged copy kept for `nb flush`).
**Coverage is judged against the current config, never recorded** (`engine.RunCoverage`): a
medium owes a run only the archives whose landing route names it, a sync rule's target is
merely *behind* (lag, not error) until `nb sync` catches up, and any other copy is a bonus —
so a route split never reads as a partial copy, and adding a landing deliberately makes old
runs read as missing there until a sync backfills them. Auto-source `nb sync --to <medium>`
is scoped the same way: a landing target is topped up with its routed archives only; a
non-landing target (or an explicit `--from`) is a whole mirror.
Flush generalizes the same way: copy to each landing still missing the archive, reclaim after
the whole route is served.

**One mutating `nb` per config at a time** (`internal/lock`). Rather than make the
catalog concurrently writable, we serialize the whole mutating run: every command that
writes the catalog or media (`dump`, `copy`, `label`, `load`, `rebuild`, `prune`) takes
a non-blocking advisory `flock` on `workdir/lock` before opening the engine, and a
second invocation fails fast (`ErrHeld`). flock is tied to the open fd, so a crash
releases it — no stale lockfiles. Read-only commands take no lock: catalog writes land
via atomic rename (write-tmp + `os.Rename`), so a reader always sees a complete
old-or-new cache. (Caveat: flock is unreliable over NFS; a workdir is expected to be on
a local filesystem. The lock is per *config workdir*, not per medium — two configs
sharing one physical volume are not yet guarded.)

**Encryption is source-tied and outermost (`package transform/crypt`).** Encryption is
the peer of compression, one stream transform further out: on write the pipeline is
**tar → compress → encrypt → meter → volume**; on read it reverses **decrypt →
decompress**. `transform/crypt` mirrors `transform/compress` — an external child
(`gpg`), selected by a registered scheme *name* (`gpg`/`none`), exposing the same
reversible `Filter` (a forward/reverse `programs.Cmd`) the engine places into a
transfer. Three decisions carry their weight:
- **Outermost placement is load-bearing.** Because encryption sits *inside* the
  `xfer.Meter`, the seal's `SHA256` covers the *ciphertext* that lands on the volume.
  So checksum `nb verify` and `CopyRun`/`nb sync` all operate on ciphertext and stay
  **keyless** — vaulting offsite, verifying integrity, and the medium-independent
  `Entry`/`Placement` identity (one run, N byte-identical copies) are untouched. Only
  *extraction* — and `nb verify --deep`, which decrypts to list the stream — needs the
  key.
- **Record the scheme name, never the key.** Each archive's header/seal carries
  `Encrypt: "gpg"` (a compiled-registry primitive, like `Compress`), so restore reverses
  it from the artifact alone — config-free, the same rebuild-from-media property
  compression has. The **key is never stored**: with a gpg public-key recipient the
  key-id travels inside the ciphertext and gpg resolves the private key from the
  operator's keyring, so a run with archives under different keys (per-dumptype) just
  restores. Selection is config (`encrypt:` block, config-wide default or a whole-block
  per-dumptype override — no field merge); the *cipher* is a compiled scheme so the
  artifact never depends on config to be read.
- **The member index stays plaintext.** Each archive's commit footer (checksums) and
  member index (filenames) are unencrypted, which lets `nb recover` and `nb rebuild`
  browse without the key. The cost — filenames are readable on the medium — is a
  documented trade, not an oversight. (Deferred: per-medium at-rest encryption (S3 SSE / LTO hardware) for
  the "untrusted destination only" posture; client-side encryption with remote
  sources; an opt-in encrypted index for client-side-encrypted archives.)

**Media model.** A `Volume` is positional, self-describing files; framing differs
per medium (disk: a `.hdr` sidecar so the payload is a clean `.tar.<scheme>`; tape:
a fixed 32 KB header block inline, since tape has no sidecars). `Open` is cheap;
`ReadFile` seeks by position; only `Files()` is a full scan (the rebuild path).
Normal ops resolve positions from the catalog and never scan.

**Cloud = an object store as a `Volume` (`media/cloud`).** One medium `type: cloud`
covers S3, GCS, Azure Blob, and any S3-compatible store, via the Go CDK
(`gocloud.dev/blob`); the backend is chosen by the bucket `url` scheme (`s3://`,
`gs://`, `azblob://`), with `file://`/`mem://` drivers making it fully testable with no
network or credentials. It is **address-identified, like disk** — a bucket+key names a
volume unambiguously, so it implements none of `Labeled`/`Drive`/`Changer` and
runs no label/swap machinery, and registers `NewSizeProfile` (a byte budget reclaimed
per run). The on-store layout is the disk medium's verbatim —
`runs/<run>/<NNNNNN>-<dle>-L<n>.tar.<ext>` clean payload objects plus a `.hdr`
sidecar — so a run streams disk↔cloud unchanged and a plain GET yields a
stock-tool-restorable archive. Atomicity is the same: payload object first, sidecar
last, a failed upload aborted (not committed), so an interrupted write leaves a
sidecar-less orphan that scan/rebuild ignores. Credentials come from each SDK's ambient
environment, never the config.

**Google Drive = an `fslike.Store` over the Drive API (`media/gdrive`).** Drive is *not* an
object store and has *no* `gocloud.dev/blob` driver, so it cannot fold into `type: cloud`; it
is its own `type: gdrive` over `google.golang.org/api/drive/v3`. But it is the *same shape* as
disk and cloud — address-identified (a folder+path names a file), no `Labeled`/`Drive`/`Changer`,
the `media/fslike` run layout verbatim — so it is a third `fslike.Store` beside the disk directory
and the cloud bucket, and disk↔cloud↔gdrive copies stay byte-identical. What is Drive-specific
lives behind a narrow `driveAPI` seam (an in-memory fake replaces it in tests, since there is no
`mem://` for Drive): opaque **file-id** addressing (Drive has no paths and permits same-named
siblings, so the store maps its `runs/<run>/<file>` keys to ids and caches them, serializing folder
creation through one mutex so concurrent dumpers never mint a duplicate run folder), a streaming
upload over an `io.Pipe`, and **ranged reads via the HTTP `Range` header** (so selective restore
pays for the covering frames, like the cloud object's ranged GET). It splits large archives into
`≤ part_size` part-files like cloud (default 10 GiB; Drive has no S3 part-count ceiling, so the cap
is only for resumability) and stays fully concurrent. Two **auth** modes, auto-detected from the
one ambient credential file (`GOOGLE_APPLICATION_CREDENTIALS`, never the config):
`google.CredentialsFromJSON` parses *both* a **service-account key** (unattended; a Workspace
**Shared Drive** — a bare service account has 0 GB usable My-Drive quota) and an **OAuth
authorized-user token** (a personal Google Drive), the latter minted by the headless `nb login
<medium>` flow (no browser, no callback port — print the URL, paste the code back). The scope is
**`drive.file`** (non-sensitive: only app-created files, dodging Google's restricted-scope audit and
letting an operator publish their own consent screen to Production so the token doesn't expire).
`nb login` is a medium-neutral capability (`media.Spec.Login`), the source-side peer of `nb label`:
only a type that needs a bootstrap registers one.

The cloud medium does, however, **split a large archive into `≤ part_size` part-objects** (default
10 GB), so an 84 GB archive becomes several objects rather than one — keeping each
object's S3 multipart upload well under the 10000-part ceiling (~48.8 GB at the default
5 MiB buffer; the buffer is unchanged, so process memory stays flat regardless of
archive size). A split archive's part objects take a **`.pNNN` part-index suffix after
the payload extension** (`…-L<n>.tar.gz.p000`, `.p001`, …; carried by `record.Header.Split`,
set per part once a `part_size` cap applies) so no fragment poses as a directly-openable
`.tar.gz`, the slices group and order by name, and `cat …tar.gz.p* | tar xz` reconstructs;
the `.hdr` sidecar keeps its plain name (each part has its own position-prefixed sidecar).
The suffix rides on the `part_size` cap *being engaged*, not on the final part count — a
small cloud archive that fits in one part still lands as `.p000` (the total is unknown
when the name is minted, and renaming an object store key is copy+delete). Only an
unbounded medium with no cap (disk) keeps the bare, stock-openable `…-L<n>.tar.<ext>` name. This is the **same proactive part machinery tape spanning uses** (the
`archiveio.Writer`'s parts, under `fslike`), but **decoupled from serial volume
rolling**: splitting (`CanSpan` / part_size) is independent of being a *serial
single-drive* medium (`Serial`, keyed off `media.ConcurrentWrite`). Cloud is
part-splitting but **not serial** — its parts are independent objects with ascending
positions on the one logical volume — so it stays **fully concurrent** (workers are not
clamped to 1, landing writers stay = workers), unlike a tape drive that splits *and* rolls
one shared reel serially. `part_size` defaults and is bounded by a per-medium
`media.PartSizePolicy` (cloud: 10 GB default, 40 GB max, the cap enforced so the knob
can't silently reproduce the 10000-part failure); disk stays single-file (unbounded,
one stock-usable payload) and still rejects `part_size`.

**Tape = a changer: drives fed from slots.** The `device` seam (the per-cartridge I/O
core — positioning + block I/O of one mounted tape) is shared by every backend; what
differs is the **loader** that inventories cartridges and binds one to a drive. `dir:`
is an emulated library over a gocloud bucket — a plain directory or any bucket URL
(`s3://`, `gs://`), with `slots: N` key prefixes and `drives: K` persisted load-pointers,
fully tested — add `manual: true` to make it behave like a hand-loaded drive while still
loading in-process, so the operator-swap UX is exercisable without hardware. `device:` is
a real single drive (Linux st(4) ioctls + `/dev/nstN`, variable-block; validated against a
tape emulator) — one drive, no addressable slots, a human loads it.

- **`Drive`, `Changer`, and the `Manual` capability.** A tape medium is one
  `media.Changer`: a set of **drives** fed from a set of **slots**. `media.Drive` is a
  drive's byte handle — `Loaded` (the cartridge in it, by barcode/label/fill), embedding
  `media.Volume` — and a later `Load` rebinds the bytes under the same handle, so a write
  sink spans cartridges without being re-pointed. `media.Changer` adds the logistics —
  `Slots` (each cartridge's barcode, read without loading), `Drives`, `Drive(i)`,
  `Load(slot, drive)`, `Unload(drive)`, `Manual()` — and never reads the on-tape label
  (that needs a load). The shape is **one assertion**: *a `Volume` that is also a
  `Changer` is a tape library; anything else (disk, s3) is a single directly-addressed
  volume.* The `tapeChanger` is also a `Volume` (it embeds drive 0), so the medium handle
  is a `Volume` above the librarian while the librarian uses the `Changer` facet below. A
  directly-addressed medium is wrapped (librarian-internal) in a trivial one-drive
  `directChanger`, so the librarian has one shape for everything. `Manual()` is the cue
  to prompt an operator rather than load a slot itself: a real drive returns it true (and
  `Load` returns `media.ErrManualLoad` — only a human moves the reel); a robot returns it
  false. The one read-only *walk* the catalog rebuild needs — "every loaded drive, then
  (for a robot) each occupied slot loaded in turn" — is `media.WalkReadable`, kept next to
  the `Changer` interface it asserts on so the catalog never type-asserts a `Volume`
  itself. The rest of the engine stays shape-agnostic.
- **Librarian — the operator-facing changer service.** Package `librarian` turns
  intents (make writable, advance, load-for-read, label, load, inventory) into
  positioning, and runs the label protocol on top. One algorithm — *load the next
  writable slot yourself, else ask the operator* — produces both experiences from the
  changer's `Manual()`: a robot iterates its slots and rarely prompts; a hand-loaded
  drive prompts immediately. It is a shared service (dump, copy/sync, restore, rebuild,
  label, load all use it), so the future sub-engine split is mechanical.
- **Operator seam.** A hand-loaded drive can't change its own tape, so when the loaded
  cartridge won't do, the librarian asks a `librarian.Operator` (CLI: stdin) to swap and
  retries — on writes (`PrepareWrite`/`Advance`: blank/foreign/wrong-pool/full → load a
  writable cartridge, auto-labeled if `auto_label`) and on reads (`MountForRead`: load the
  cartridge holding the needed label). The emulated sim effects the operator's choice
  in-process; a real drive's choice is the human's physical insert, which the librarian
  re-reads. Unattended (no operator) it degrades to an actionable error instead of
  blocking. A `reloadable` error marks the cases a swap can fix (vs a stale catalog, which
  a swap can't).
- **Expected tape.** `Engine.ExpectedTape` names the volume the next run will write
  to, derived from the catalog's volume registry and `retention.Compute`, never from
  a physical scan: a one-run-per-tape (non-appendable) run reuses the **oldest volume
  whose every run is unprotected** (past `minimum_age`, with a newer recovery path) —
  the oldest reusable tape — or a *fresh tape* when none is reusable; an appendable
  run extends the most recently written volume. `nb plan` prints it, run output
  announces it, and it seeds the swap prompt's suggestion (`SwapRequest.Expect`) so the
  operator is told *which* reel to load.
- **Whole-volume recycle on write (label rotation, Amanda's *tapecycle*).** When a run
  needs a fresh writable volume and no blank/empty in-pool tape is available, NBackup
  **reuses the oldest reusable tape automatically** rather than refusing: it rewrites
  that volume's file-0 label in place — same `Name` and `Pool`, `++Epoch`, fresh
  `WrittenAt` — physically wiping it (`WriteLabel` resets first), and reconciles the
  catalog (prior-epoch placements are dead, dropped; a run that loses its last copy
  leaves). Because placements pin `archiveio.FilePos{Label, Epoch, Pos}`, the epoch bump
  alone retires every prior-epoch placement; the physical reset means a rebuild sees the
  new epoch only. This lives entirely in `package librarian` (the media-shape seam):
  `advanceViaLibrary` recycles a robot's oldest Floor-cleared cartridge after its
  blank/empty slots are exhausted; `acceptOrRecycle` recycles an aged-out cartridge a
  hand-loaded-drive operator loads; both reuse the `nb label --relabel` reconcile path. The
  **retention `Floor` is the safety gate** — a tape holding *any* kept archive (counting
  spanned parts) is never reusable — so reuse is **automatic and unconfigured** (the
  floor makes it safe). If every tape is still protected and none is blank, the run
  **fails loud** (`librarian.ErrAllVolumesProtected`: "every volume within retention;
  oldest ages out on …") rather than overwriting one — recoverability outranks capacity.
  `nb label --relabel` remains the manual early-recycle override. The selection applies
  the **same rule** as the Expected-tape announcement (`retention.Compute` over the
  medium's own runs, pool oldest-`WrittenAt` first), so the tape a run recycles is the
  one `nb plan` said it would; the in-progress write tape is held out of the candidate
  set so a span never recycles the cartridge it is writing.
- **Barcode (physical) vs Label (logical) are distinct.** A `Changer` is
  **label-agnostic** — like a real library it reports each slot's barcode without
  loading, never the magnetic label; the librarian reads the label *after* loading the
  cartridge into a drive. A blank cartridge has a barcode but no label; relabel rewrites
  the label, same barcode. The catalog references **labels** (durable data identity);
  slots and barcodes stay internal. (To locate a label among the slots the librarian
  loads each occupied slot and reads it — a real library would instead cache
  barcode→label to skip the scan; that cache is a future optimization.)
- **Finite volumes.** A write past `volume_size` hits `media.ErrVolumeFull`
  (end-of-tape), the partial file is discarded. Spanning sizes each part to fit
  *before* writing, so this is a backstop, not the normal path (see Spanning below).
- **Append vs one-run-per-tape.** `appendable: true` (default) packs many runs per
  tape until full; `appendable: false` writes one run per tape. Real tapes are
  physically appendable; one-run-per-tape is a deliberate retention choice, not a
  hardware limit.
- **Spanning: a run (and one archive) can cross volumes, proactively.** Both a
  **dump** and a **copy/sync** split work across tapes mid-archive — one DLE's
  compressed byte stream may itself span several tapes. The unit is the **part**: a
  contiguous byte-range of an archive's payload, its own self-describing file (header
  carries the part *index*; the archive's commit footer carries the part *count*). An
  archive is always a list of parts (one in the common case). Splitting is
  **proactive**: the operator sets `volume_size`, so the writer (`archiveio.Writer` via
  a `librarian.Allocator`) sizes each part to the loaded volume's known remaining
  capacity (optionally capped by `part_size`) and rolls onto the next writable volume
  *between* parts — a robot loads the next writable slot (blank → auto-labeled,
  or an empty in-pool tape, or — once blanks are exhausted — the oldest Floor-cleared
  tape, recycled in place; never a tape holding a *kept* run); a hand-loaded drive
  prompts for a cartridge swap; an unbounded or changer-less medium writes one part. There is **no
  reactive "keep what fit on EOT"** and no holding-disk buffer (the one-pass stream
  means a part already on tape cannot be re-read to rewrite it). If a sized part *still*
  overflows (an optimistic `volume_size`, or orphans the catalog cannot see),
  `media.ErrVolumeFull` discards the partial and the run **fails** with an
  actionable message — no recovery. A tape cannot report its own fill, so remaining
  room is **computed, never read**: the catalog keeps each reel's fill
  (`VolumeRecord.Used`), priced by the **medium's own cost rule**
  (`media.FileCoster` / `Spec.FileCost`: framing plus payload from part seals and
  the footer's `IndexSize`, with the two unrecorded payloads — label, commit
  footer — charged at an upper bound, so the figure errs toward an early roll,
  never toward EOT; all size constants live in the tape package). The rule is
  applied only inside the catalog's own mutators — every placement add/remove and
  the relabel reset move their own charge (`applyFill`, resolver injected by the
  engine via `PriceWith`) and the rebuild scan reconstructs `Used` through that
  same path — so **no reader ever meets a cost function**: plan, `nb medium`, and
  the librarian's accept snapshot just read the field. The librarian spends the
  declared `volume_size` against that snapshot plus each file it lands after,
  priced by the same rule (`volumeFill`/`countedVol`), so the record-time figure,
  the fit check, and the landing can never disagree. One arithmetic for real
  drives and the `dir:` sim alike — the sim's enforced capacity is only the
  physical-EOT stand-in. The commit
  footer (written last) commits the archive, giving an interrupted span the same
  per-archive atomicity as a single-volume archive (orphan parts ignored by
  scan/rebuild, reclaimed by relabel). Because a single drive cannot interleave two
  archives' parts, a spanning-capable landing **clamps workers to 1**. Reads
  **auto-mount** the volume holding each part, in order — `archiveio`'s
  concatenating reader drains part *k* before mounting *k+1*, then reverses the scheme
  over the concatenation. The roll/mount lives in `package librarian` (`Allocator`,
  `Advance`, `MountForRead`), the one place that dispatches on medium shape.

**Labels as a capability.** Verified before every write (refuse foreign / blank
unless `auto_label` / wrong-pool / relabeled-since-cached). Address-identified
media (disk, S3) carry no label and skip the whole dance.

**Network politeness is a per-medium throughput cap (the `nice` of bandwidth).**
NBackup runs its child processes under `nice` for CPU politeness; a cloud-first user
needs the network analogue, or an uncapped `nb dump`/`nb sync` to S3 saturates the
office uplink during business hours (and once remote sources land, the read side too).
The cap is a medium config knob — `throughput: 50MB/s` (bytes/sec, `/s` optional) —
because the thing protected is the uplink *to a given medium*. It is enforced by a
token-bucket `ratelimit.Limiter`, an in-process stream stage wrapping the **medium-facing**
stream: on write it paces the bytes landing on the volume (inside
`archiveio.Writer.drainParts`), back-pressuring the one-pass pipeline through its pipe —
no holding-disk buffer, and the wait is a timer sleep so it cannot deadlock; on read it
paces each part stream the medium hands back (wrapped in `engine.partOpener`, the single
choke point every restore / un-vault / drill / sync-source read flows through). One
`Limiter` is built per medium and **shared**, so several concurrent workers to one
medium draw from a single budget — a global bandwidth ceiling, which here collapses into
the per-medium cap because a run writes a single landing medium. The default is
uncapped: a nil `Limiter` returns the stream untouched, so a run with no `throughput`
set behaves byte-for-byte as before. It composes with `nice` (CPU) and stays
medium-neutral — it lives in `ratelimit` (wired in by `engine`), never a medium package. (Deferred:
time-of-day awareness — a tighter cap during business hours.)

**Medium-neutral vocabulary.** The generic media/changer/config layer must not say
"tape": `slots`, `drives`, `volume_size`, `throughput`, `media.ErrNoVolume`,
`media.ErrManualLoad`, `media.Drive`/`Changer`/`SlotStatus`/`DriveStatus`/`VolumeStatus`,
`nb medium`, `nb load`. Tape specifics (`type: tape`, the `tape` package, the st(4)
ioctls, `vtape`, the `reel`/`cartridge` vocabulary) stay local, so a future
`usb`/removable-disk medium reuses the vocabulary.

**Archiver-neutral vocabulary (the same discipline, one layer up).** The generic
`archiver`/`catalog`/`engine`/`planner` layer must not say "tar" or "snapshot":
`Archiver`, `BackupRequest{DLE, Level, BaseLevel}`, `HasBase`, the recorded
`Archiver`/`archiver:` field, the `archivers:` config block, "incremental state", and the
host-level `state_dir` that roots it (a host property, not a format one — see above). GNU
tar specifics (`tar`, `.snar`, the listed-incremental snapshot, `tar_path`) stay inside
`archiver/gnutar`, so a future `ampgsql`/`amraw`-style archiver reuses the vocabulary. The concurrency unit is a **worker** (`parallelism.workers`),
not a "archiver" — "archiver" means only the plugin.

**Execution is an injected transport; a backup/restore is one `xfer.Transfer`
(`programs` + `xfer`).** The compressor and encryptor are *just programs*, like `tar`,
so there is **one** path. `package programs` is the base: a `Cmd` plus an `Execution`
(the host it runs on — `Local` or `SSH`) whose `RunPipe` runs a pipe of commands on that
one host. `package xfer` composes those single-host pipes into a `Transfer(source,
filters, sink)`: each transform lands in the **Source** zone (a client's host, fused
with tar), the local **Filters** zone (the server), or the **Sink** zone (the medium, or
a target's host) — so a transform never runs on a third *remote* host, and the wire is
crossed only between zones. A fault is tagged with its zone, exactly the
Pipeline-vs-Chain/Structural split the drill and verify layers classify on. So a local
dump (everything `Local`), a thin client (tar on `SSH`, transforms in local Filters),
and a fully client-side dump (tar+compress+encrypt all in the `SSH` Source — only
ciphertext crosses to the server Sink) are the *same* code with different zone
placement. The transport lives in one neutral package and is injected
into archivers via `archiver.Open(name, opts, ex, stateRoot)` — SSH is part of **no** archiver, so
a new archiver gets remote execution for free as long as its binaries are on the
client. The meter stays server-side, so each archive's commit footer still covers the
bytes that land (verify/copy/sync stay keyless). This is the source-side peer of the
medium-neutral discipline; see
[docs/design/remote-sources.md](docs/design/remote-sources.md) for the configurable
`compress`/`encrypt.at` point and the three key-trust postures.

**Run monitoring is a status file, not a daemon.** `nb dump` drives a
`progress.Tracker` whose workers report start / live bytes / finish; the tracker
flushes a single JSON snapshot to `<workdir>/run-status.json` (atomic temp+rename,
byte updates throttled to 1 s, state changes forced). `nb status` is a *separate*
process that just reads and renders that file — a log-writer + status-reader split,
minus the daemon, which fits "state lives in inspectable files." It needs no engine (no
media scan), so it is cheap to poll, and the final `done`/`failed` snapshot is left in
place as the last-run record. The file spans the **whole** cycle: a run opens it in the
`estimating` phase (sizing every DLE — a slow archiver pass the operator would otherwise
watch in silence), then the dump phase takes over the same file under the real run ID
(`running` → `sealing` → `done`/`failed`). The estimate phase is deliberately non-terminal
so a `--watch` poll never stops on the gap between sizing and the first dumped byte; the
estimate tracker's terminal "done" (which a live display uses to erase its region) is
rewritten to `estimating` for the file by the conductor's `keepEstimating`. Progress reporting never blocks or fails a backup (a write
error is a stderr warning). With no holding disk (the one-pass stream), there is no
separate dumper/taper split, just one `dumping` state per DLE, metered by uncompressed
bytes against the planner estimate. The measurement point is the source stage's byte tap
(`programs.Cmd.Tap`) on the tar→compressor stream, wired in the dumper's `dumpArchive`;
compressed bytes come from `archiveio`'s streaming meter as the payload drains into parts
(both feed the same per-DLE counters, throttled so they can be polled live).

**Reporting + alerting make an unwatched failure loud (`nb report`, `notify:`).**
Where `progress` is the *live* run-status of one in-flight dump, `report` is the
*historical* record of finished runs across every command, and `notify` pushes a failure
to a human — the "0 errors" half of 3-2-1-1-0 only matters if a non-zero result reaches
someone. The choices, all mirroring existing stances:
- **One seam, not per-command.** Every run-producing command (`dump`, `sync`,
  `prune`, `verify`, `drill`) runs its body through `cli.runReported`, which stamps the
  outcome, appends a uniform `report.Run` to `<workdir>/run-log.jsonl` (one compact JSON
  line; the latest also written as `run-summary.json` for a monitor to scrape), and
  dispatches notifications. The engine is **unchanged** — it already returns rich reports
  and exits non-zero on failure; recording is pure CLI glue over two leaf packages.
  Dry-runs record nothing.
- **Recording is best-effort, exit codes are sacred.** A summary-write or
  notification error is a stderr warning and never changes — nor suppresses — the
  run's own exit code (the `progress.NewFileSink` contract). `runReported` returns
  the body's error verbatim.
- **Failures are always loud; a successful `dump` is loud too, by default.** Any
  command alerts on failure (every backend unless `on_failure` narrows it). Routing also
  notifies on a *successful* `dump` by default — the nightly "backups happened" signal,
  so a silent inbox reads as "cron didn't run" rather than "all is well"; other commands'
  success is opt-in via `on_success` (which, when set, applies to every command). This is
  the one place routing keys on the command, kept in `notify.routeFor`.
- **History is append-only JSONL; alerts are a registry; secrets are env-refs.**
  The log appends (O(1)) and compacts to a bounded tail, and a reader tolerates a
  torn trailing line (the one unlocked writer, `nb verify`, may race `nb report`).
  A notify backend is a registered name (`smtp`/`sendmail`/`webhook`) like
  `transform/compress`/`crypt`, so adding a channel is a registration (`sendmail` pipes
  the same RFC 5322 message the SMTP path builds to a local MTA binary — postfix,
  sendmail, exim — needing no relay host or secret). A literal `password:`/`token:` key
  is rejected structurally by `KnownFields(true)` (neither is a field), so an SMTP password
  is a named environment variable (`password_env`) resolved at send time; a webhook URL is
  either a literal `url:` or a named env var (`url_env`, preferred when the URL is itself a
  secret). `nb
  report` (read-only, no engine) renders the recent history plus a live
  drill-ledger recovery audit (failing / degrading / stale / never-drilled DLEs via
  `drill.Ledger.Coverage`); `nb report --notify` mails the same digest.
- **Per-DLE dump report.** A dump's record carries a per-DLE `DumpStats` row (level,
  original/output size, files — from the seal — plus dump time read from the
  just-flushed `run-status.json`), captured by the CLI at seal time so the report is
  historical, not just the last live run. `nb report --dump` (latest, or `--run <id>`)
  renders the per-DLE table — sizes, compression %, time, rate, full/incremental totals
  — via the one `report` renderer the dump *notification* also uses, so a configured
  dump alert *is* the nightly report, not a bare "it worked". (A run older than the
  history shows sizes via `nb run <id>`.)

**Reclamation asymmetry, and it is per-archive.** Disk/S3 reclaim per **archive**
(a DLE's image within a run), not per run; tape reclaims a whole volume — by label
rotation **on write** (the oldest Floor-cleared tape recycled when a run needs a fresh
volume; see "Whole-volume recycle on write") or a manual `nb label --relabel` — never by
a prune pass: `tape.RemoveFile` errors, and `volumeProfile.Reclaim` deliberately returns
nothing, so `nb prune` never deletes from a tape (tape capacity is structural — the depth
of the label pool *is* the retention). Per-archive is the granularity the **floor already uses**:
`retention.Compute` returns a floor keyed by `(run, DLE)` (younger than `minimum_age`,
or part of a DLE's **live recovery chain** — its last full plus every later incremental,
since `recovery.Chain` replays them all; a recent run also pins the older base its
restore needs, so reclamation never breaks a chain it leaves restorable). Because a
DLE's chain is independent of its run-mates', an old run routinely holds one DLE
whose chain has moved past it (reclaimable) beside another the chain still needs —
walking archives oldest-first (`sizeProfile.Reclaim`) frees exactly the dead ones,
where a run-granular pass would strand them behind a single still-pinned DLE. The
floor still answers run-level queries (`Keeps`/`First` = "is *any* archive pinned")
for the whole-volume reclaimers (tape relabel, `ExpectedTape`) and the cost forecast.
Both floor and capacity strategy are per-medium: the rule is shared but judged over one
medium's own runs, so a copy on another medium never makes an archive reclaimable.
The `Volume` seam stays purely positional — one `RemoveFile(pos)` (the peer of
`ReadFile`); the engine resolves an archive's positions from the catalog and removes
them one by one, and fslike reclaims an emptied run directory itself, so the medium
interface never names "run" or "archive". `nb dle` browses the same catalog grouped by
DLE (one row per source, then its archive timeline across runs) — the per-DLE
counterpart of the run-grouped `nb run` view.

**Capacity model (`media.Profile`).** A profile exposes two numbers that the
planner keeps distinct. `TotalBytes` is the **pool** — the retainable capacity
(`volumes × volume_size` for a tape library, `capacity` for an object store) — and
drives reclamation and the structural cycle check (can a complete recovery set be
retained at all). `VolumeSize` is one **reel**, the basis of the per-run ceiling:
a run fills the reel it lands on before spilling to the next, so a single run can
never exceed one reel. The accountant's `CapacityRoom` feeds the planner the tighter
of the two — pool free room (`capacity − protected`) and the landing reel's
remaining room (`volume_size −` what's already on it). They are truly
separate: a **real drive** (`type: tape`, `device:`) has an unbounded pool (the
operator's shelf is unknowable, `TotalBytes == 0`) but a finite reel. The volume
profile reads the same count key the changer does — `slots` for an emulated library —
so the planner's capacity never disagrees with the medium it lands on.

**Cost model (`media.Cost`) — a medium prices itself, in dollars, offline.** The
persona reasons in dollars per month, not bytes, and the bill's surprises are the
non-storage charges (chiefly egress on a restore). Since NBackup already accounts bytes
precisely — estimates, forecast, capacity — pricing is a **thin pure calculation on
top**, the dollar peer of `media.Profile`'s bytes. `media.Cost` is one medium's *flat*
rate table: a storage `$/GiB-month`, an egress `$/GiB`, and a GET `$/1000`. Four
decisions carry it:
- **A medium prices itself, like it sizes itself.** Pricing is a per-medium concern
  registered like capacity: `media.RegisterCost`/`OpenCost` mirror
  `RegisterProfile`/`OpenProfile`, and `media/cloud` owns the provider rate tables
  (`aws-s3`/`gcs`/`azure-blob`/`generic-cloud`) and the URL-scheme inference that picks
  one. The core never learns provider pricing; the engine calls `OpenCost` and consumes
  a dollar number the way it consumes profile bytes. A medium type with no registered
  cost (disk, tape) is **unpriced** — the zero `Cost`, no recurring cloud bill — and the
  CLI suppresses its cost output, mirroring how an unregistered profile is unbounded.
- **Zero-config, with overrides.** With no `cost:` block a cloud medium reads its
  bucket URL scheme (`s3://` = AWS, `gs://` = GCS, `azblob://` = Azure; anything else
  → a generic cloud table) so `nb plan` shows a monthly bill out of the box. The
  optional `cost:` block names a different provider table or overrides individual
  rates (a region's egress, an S3-compatible provider) — flattened into the factory's
  options like `ProfileOptions`, validated at load.
- **No lifecycle tiers.** NBackup does **not** model storage-class transitions
  (Glacier/Deep Archive). Which tier bytes physically sit in is operator-configured
  bucket-side; a forecast of it would more often be wrong than useful, and the machinery
  (per-class rates, retrieval fees + latency, minimum-retention floors, an age→class
  schedule) is complexity for accuracy NBackup can't deliver. Flat pricing is the honest
  estimate. (Considered and removed.)
- **Estimation only, fully offline; the overlay reuses the byte machinery.** It is a
  pure calculation over the catalog and the rate table — **no billing API** — so it
  runs wherever planning runs and never touches a slow/offline volume (the provider
  invoice stays authoritative; the tables are list-price estimates). The dollar
  overlay lives in `accounting/cost.go` (`CostSummary`, `ForecastCost`,
  `RestoreCost`/`SelectionCost`), mirroring the capacity overlay
  (`StoredBytes`/`CapacityStatus`). `ForecastCost` walks the existing run simulation
  day by day, growing a footprint with each simulated run and evicting it with the
  medium's own `Reclaim` + `retention.Compute` (the primitives `nb prune` uses), then
  reprices the survivors — so the `$/month` curve reflects fulls/incrementals landing
  and pruning reclaiming. The read paths (`nb restore`/`recover`/`drill`) price the
  egress of the chain they will read off the chosen copy and, when material, warn but
  never block a cron read (it prints the estimate and proceeds, like the unattended
  drill). An offsite drill spends the full bytes (encrypted+compressed is
  all-or-nothing), so its dry-run egress now carries a `$`.

## Conventions for working here

The working rules (commit discipline, Amanda-faithfulness, the verify commands, and
the test environment's missing `zstd`) live in [CLAUDE.md](CLAUDE.md); they are not
repeated here. The one convention that is architectural rather than procedural:

- **CLI:** flags may appear before or after positionals (`parseArgs`). The convention is
  **inspect with a noun** (`nb run`, `nb dle`, `nb medium`), **act with a flat verb**
  (`nb dump`, `nb check`, `nb verify`, `nb drill`, `nb prune`, `nb rebuild`, …) — so every
  mutation is a top-level verb. Each inspection noun is bare-noun + optional positional:
  no argument lists, an id details that one (`nb run <id>`, `nb dle <dle>`, `nb medium <name>`),
  rather than `list`/`show` subcommands — uniform across the three (matches restic's
  `snapshots`, kubectl's name-presence). Per-medium status (incl. drives + slots)
  lives in `nb medium <name>`; `nb load` is the one physical action verb (sibling of
  `nb label`). `--catalog` has no short flag (a case-only `-C`/`-c` pair is too easy to
  slip).

## Deferred / known next steps

- **Remote sources over SSH** — the dump path is implemented; a remote DLE dumps over
  SSH with no NBackup software on the client, `nb check` verifies every host through its
  executor, and whole-DLE restore is opt-in onto a client (`nb recover --all --to
  host:path`, decode running client-side for a client-held key). The remaining follow-on
  is the drill recoverability tiers and file-level recover on the client. SSH paths are
  untested in CI (no sshd); see [docs/design/remote-sources.md](docs/design/remote-sources.md)
  for the transport, the client-vs-server encryption point, and the trust postures.
- Mixed-generation libraries: a roll only tries the allocator's own drive, so a
  library whose remaining blanks are loadable only by ANOTHER drive's generation
  fails the run (cleanly — catalog consistent) instead of hopping drives. Problem
  + constraints in [docs/design/mixed-generation-drives.md](docs/design/mixed-generation-drives.md)
  (hardware-confirmed 2026-07-07; the per-slot skip on refused loads works).

For user-facing usage, config, and the restore-with-stock-tools story, see the
[README](README.md).
