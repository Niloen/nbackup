# NBackup — Architecture & Decisions (agent orientation)

This is the internal map for anyone (human or agent) working *on* NBackup. The
[README](README.md) is the user-facing front page; [PRD.md](PRD.md) is the product
vision. This document carries what the code and README don't make obvious: how the
concepts nest, the load-bearing decisions and *why* they were made, and the
conventions for working in this repo.

NBackup is an **Amanda-inspired, slot-based backup system** in Go. It orchestrates
external tools (GNU tar, a compressor) as child processes — like Amanda — rather
than reimplementing them, and produces immutable, self-describing artifacts that
restore with stock tools.

## Vocabulary (how the concepts nest)

- **DLE** — a backup source (`host` + `path`). Amanda's disklist entry.
- **Run** — one planner execution (typically daily).
- **Slot** — the primary artifact: one **Run** produces exactly one Slot, an
  immutable, sealed set of archives. Addressable unit for copy / restore / list /
  retention. (`slot-YYYY-MM-DD`, `.2`/`.3` for same-day reruns.)
- **Archive** — one **DLE** image at one level inside a Slot (Amanda's "dump").
- **Cycle** — the safety/scheduling boundary: every DLE is fulled once per cycle.
- **Medium** — a named storage definition; opens as a **Volume**.
- **Volume** — an ordered sequence of header-framed, self-describing files
  addressed by position (Amanda's Device API). Disk, tape, object stores all map to it.
- **Catalog Entry / Placement / Part** — the catalog separates *what a slot is* (one
  medium-independent `Entry`, from the seal) from *where its copies live* (N
  `Placement`s, one per medium). A placement holds each archive's ordered **parts**
  (a part = `volume + position`; one part unless the archive spanned volumes) plus the
  seal's location.
- **Label** — logical identity written on a labeled volume's file 0 (magic, name,
  pool, epoch). A *capability* (`media.Labeled`); address-identified media skip it.
- **Bay** — a physical position in a robotic tape library (`bay-01…`), the durable
  cartridge identity. Distinct from the Label written inside it. A single-drive
  station has no bay inventory — its one mountable position is the drive itself, and
  its off-drive cartridges are **reels** (`reel-01…`) on a **shelf** (the
  environment, `media.Shelf`), loaded by an operator rather than a robot.

## Package map

Mechanism lives behind interfaces with named, registered implementations; one
orchestrator (`engine`) composes them. Adding a medium, archiver, or codec is a
registry registration, not a conditional in the core.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | disklist / dumptype / storage |
| `record` | the self-describing on-medium artifact records: `Header` framing + `Label` (volume id record) + `Archive` (commit-footer metadata + the in-memory `Slot` grouping/lifecycle) + the per-archive member index (`EncodeIndex`) + their (de)serialization | dumpfile_t / amar |
| `archiveio` | maps a slot's archives onto a `Volume`'s files — meter + split a payload into parts then write its index + commit footer (`WriteArchive`/`Commit`/`Finish`); concatenate + assert parts on read (`Expect`); knows nothing of compress/encrypt | taper / amrestore |
| `media` | `Volume` + `Labeled` + `Drive`/`Changer` (device) + `Shelf` (environment) + `Profile` + registry; reads/writes `record` artifacts | Device API |
| `librarian` | operates a medium's `Changer`/`Shelf` + label protocol (make-writable, advance, mount, label, load) | changer / amtape |
| `media/disk`, `media/tape`, `media/cloud` | Volume impls (disk sidecar headers; tape library; object store via gocloud.dev/blob) | vfs / tape / s3 devices |
| `media/fslike` | the slot layout shared by the address-identified media — clean payloads + `.hdr` sidecars over a small `Store` seam (disk = a directory, cloud = a bucket), so disk↔cloud copies are byte-identical | — |
| `archiver` + `archiver/gnutar` | `Archiver` interface + registry + named definitions; owns its incremental-state library; GNU tar impl | Application API / amgtar |
| `transform/compress` | external compressor child processes (zstd/gzip/none) + registry; `Filter(scheme)` returns the forward/reverse `programs.Cmd` | compress |
| `transform/crypt` | external encryptor child processes (gpg/none) + registry; `Filter(scheme)` returns the forward/reverse `programs.Cmd` | amcrypt/amgpgcrypt |
| `programs` | the base: a `Cmd` (external program to run) + an `Execution` transport that runs a pipe of commands on one host, transparently `Local` or `SSH`; the command/execution concept compress, crypt, and the archiver all build on | amandad (replaced by stock sshd) |
| `xfer` | the data-movement primitive: `Transfer(source, filters, sink)` moves one stream through three zones — a `Source` (a client's tar, or a medium read), local `Filters` (compress/encrypt or decrypt/decompress), and a `Sink` (a medium, a target's tar, a hash) — tagging faults by zone; plus the `Meter`/`Limiter` byte pieces | Amanda Xfer / netusage |
| `clerk` | the archive data path (both directions): composes each operation as one `xfer.Transfer` — `Backup`/`Copy` (write: archiver tar source → encode filters → medium sink), `Extract`/`ListMembers`/`VerifyChecksum` (read: copy selection + fail-over, mounting volumes via the librarian, decode from the record), and the shared `DecodeFilters` builder used by drill. Owns the two archiveio-coupled endpoints (archive `Source`, medium `Sink`) | Scribe + Recovery::Clerk |
| `progress` | live run-status model + status-file I/O + render | amdump log / amstatus |
| `report` | per-run history record + JSONL/summary file I/O + digest render | amreport |
| `notify` | pluggable alert backends (smtp/webhook) + registry + dispatch | amreport mailto |
| `catalog` | local cache of slot index + volume registry; derives `History` | catalog / curinfo / tapelist |
| `retention` | retention safety floor: protected slots — `Compute` returns a `Floor` (`.Keeps(id)`) (pure) | policy |
| `restore` | the archive chain to rebuild a DLE as of a slot (pure) | amrestore |
| `recovery` | as-of-date browse tree + per-archive file selection (pure) | amrecover |
| `drill` | recovery-drill ledger + risk-biased selection + failure taxonomy (pure) | amverify (orchestrated) |
| `planner` | multilevel level scheduling (pure) | planner |
| `engine` | the driver: parallel workers, retention, the slot session (open/finish/placement), drill selection; wires planner→`clerk`→media→catalog and delegates each operation's data movement to the `clerk` | driver |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, retention, archiver, xfer,
clerk, archiveio, catalog, config, progress, restore, recovery}` over leaf packages
`{media, programs, sizeutil}`, all bottoming out on `record` (the on-medium artifact
format that `media`, `archiveio`, and `catalog` read and write) (`recovery` builds on `restore`). The reporting
layer adds `cli → {report, notify}` with `notify → {report, config}` — `report` is a
pure leaf (record + render); the engine does **not** depend on either.
Domain packages stay pure; `archiver`/`media`/`transform/compress`/`transform/crypt` are
pluggable adapters; `engine` is the only component aware of all of them. A backup is an
**`xfer.Transfer`** the **`clerk`** composes (the engine orchestrates, the clerk moves the
bytes): a **Source** (`tar` via `archiver.Backup` on the DLE's host, fused with any
client-side compress/encrypt) → local **Filters** (server-side compress/encrypt) → a
**Sink** (the medium, via `archiveio` meter + split into parts). Restore is the same in reverse
(a medium-read Source → decrypt/decompress Filters → a target's `tar -x` Sink), and
copy/verify/drill are Transfers with different endpoints — all composed by the `clerk` from
the same two archiveio-coupled endpoints and the one `DecodeFilters` builder.

## Load-bearing decisions (the *why*)

**The archive is the commit unit; the slot is a grouping.** Each archive is made
durable by its own **commit footer** (`KindCommit`), written last — after its parts
and its member index — so the footer's presence proves the whole archive landed.
The footer holds the per-archive **integrity** the framing headers omit (`SHA256`,
sizes, part count); the member list rides in a separate per-archive **index**
(`KindIndex`, gzip), kept out of the footer so a scan reads only small footers. There
is **no per-slot seal**: a slot is just the run-id its archives carry in their
headers, reconstructed by grouping committed archives. This deliberately drops
all-or-nothing run atomicity (we considered and rejected keeping the seal): a crashed
run keeps every *committed* archive, and a rerun fills in the rest — "run complete?"
is a derivation (did every planned DLE commit?), not a stored bit.

**Partial writes are tolerated, never repaired.** A hard kill or power loss
mid-write can leave uncommitted bytes on a volume: a payload with no header
sidecar, a torn sidecar, a half-framed tape record, an archive's parts with no
commit footer. Two layers absorb this, neither by deleting anything (delete is
impossible on WORM): the **commit footer** is the archive's marker — an archive
with no footer is never assembled into the catalog (`assemble` iterates commits;
parts without one are orphans), so its parts are simply unreferenced. Beneath it,
each medium's `Files()` enumeration treats *any* artifact it cannot read or parse
as uncommitted and **skips it** — enumeration must always complete, so a single
torn file can never abort `nb rebuild`. The commit test differs per medium
(fslike: payload paired with its later-written sidecar; tape: a decodable framed
record), so it lives in each medium, not in a shared layer. Orphans are reclaimed
only when their slot/volume is, via prune/relabel — we never reap on read.
Integrity of files a footer *does* commit (bit-rot) is verify's job, not the
rebuild's.

**The catalog is a cache; the media are the source of truth.** Every file is
self-describing (header), every archive carries its own commit footer, every labeled
volume carries its label — so one `Files()` scan rebuilds everything (`nb rebuild`):
commit footers + indexes → archives grouped into slots, labels → volume registry. The catalog lives in its **own `workdir`**
(default `nbackup-catalog`), *independent of any medium* — it is a cache over the
whole pool, not part of one medium. The `Entry`/`Placement` model means a slot
copied disk→tape is one Entry with two Placements; restore/verify pick any
available copy. Run `History` is *derived* from cached slots (no second source to
drift) — the catalog holds **no** precious state, every byte of it rebuildable
from the media. (The one piece of non-derivable local state — an archiver's
incremental-state library — belongs to the **archiver**, not the catalog; see the
next decision.)

**Incremental state belongs to the archiver, not the catalog.** The base data an
incremental builds on (for gnutar, the listed-incremental `snapshots/…/L<n>.snar`
library) is the only state that is *neither* on the media *nor* derivable from it.
It was once parked in the catalog by proximity, which forced the "catalog = pure
cache" claim to carve out an exception. It is now owned by the `archiver` package: the
generic `Archiver` interface speaks only `DLE`/`Level`/`BaseLevel`/`HasBase` — never a
snapshot path — and each archiver resolves its own state under a `state_dir`
(Amanda's per-application `GNUTAR-LISTDIR`; the engine injects the default beneath
the workdir, an archiver definition may override). This sharpens the catalog's claim
to *no exception*, makes the `Archiver` interface genuinely pluggable (a future
`ampgsql`-style archiver keeps its own state however it likes), and gives **remote
sources** a seam: when `tar` runs on the client, its `state_dir` is a client path,
and nothing in catalog/engine/planner changes. The state is precious — losing a
DLE's base forces its next run to a full (the drill posture warns).

**Incrementals sit at a level and climb only on real savings (Amanda's bump).** A
DLE does not gain a level per run. After a full it sits at level 1, re-dumping
everything since the full each run, and climbs to the next level only when it has
held the current one for `bumpDays` runs *and* the next level would save at least
`bump_percent` of the full size (`planner.chooseIncrLevel`). The threshold is a
percentage so one knob fits every DLE regardless of size, and because the saving
from climbing shrinks as levels deepen, level 1 stays the common case and deep
levels are rare. Two payoffs over a naive per-run climb: restore chains stay short,
and consecutive same-level incrementals *overlap*, so losing one does not break the
chain. A level-`L` dump bases on the `L-1` snapshot, so repeating a level just
re-derives `L`.snar from the unchanged `L-1`.snar. `restore.Chain` is **Amanda's
per-level restore**: it replays exactly one archive per level — the tip (the most
recent dump at or before the target) walked back along each incremental's recorded
`BaseSlot` to the full — so a redundant same-level repeat is skipped, not replayed.
Replaying every same-level dump is not merely non-minimal: GNU tar's directory
directives (rename, delete) are *not* idempotent across independent incremental
extractions, so a second cumulative `L1` carrying the same `rename old → new` would
abort the chain (`tar: Cannot rename …`). Walking `BaseSlot` also keeps the chain
*consistent* — each step's base is the exact dump it derives from, never an unrelated
same-level dump — and a `BaseSlot` whose slot has been pruned is a **broken-chain
error**, never a silent partial restore. The overlap-redundancy property (fall back
to an earlier cumulative incremental when the tip's copy is unreadable) is a deferred
recovery feature, not the normal restore path.

**Recover is amrecover without an index server.** Amanda runs a separate index
server holding per-dump gzipped path lists so `amrecover` can browse without
reading tapes. NBackup keeps the equivalent: each archive's **member list is its own
gzipped index** (`KindIndex`) on the medium, which the catalog caches into
`Archive.Members` (read eagerly on scan today; a lazy server-side index cache is the
planned optimization), so `nb recover` browses by reading the catalog alone — media is
touched only on extract. `recovery.BuildTree`
merges the restore chain's member lists in run order (most-recent-wins), giving an
as-of-date filesystem where each path resolves to the archive that last held it;
`Collect` turns a selection into the fewest per-archive extractions (one tar run
per source archive, exact members via `--no-recursion`). Two deliberate splits
from whole-DLE `restore`: selected-file recovery extracts the named members in
*plain* tar mode and **never applies deletions** (you get exactly what you ask
for), whereas a chain `restore` uses `--listed-incremental` to honor them; and the
browse tree is a union, so a file deleted at a later incremental still appears
(the member index records additions, not deletions — that lives in the snapshot).

**Verify is the primitive; drill is the orchestration (`nb drill` = recoverability,
not just integrity).** `nb verify` stays atomic and **stateless** — it checks
individual slots/archives against the seal and writes nothing, keeps no ledger, makes
no selection. It gains one capability, a `--deep` structural mode: stream an archive
through the real read pipeline (decrypt → decompress → `tar -t`, list not extract) and
assert the pipeline completes and the members match the seal — proving the bytes are a
valid *restorable stream* and exercising the key + codec, still side-effect-free. It
emits a structured per-archive verdict (`engine.VerifyReport`, classified with
`drill.Class`) the drill layer consumes. `nb drill` is the layer on top: it *selects*
a risk-biased subset (rotate every DLE within a window; prioritize the longest
incremental chains and the oldest fulls; drill a point-in-time, not only the latest
slot), *exercises* each at a tier (checksum → structural → a real point-in-time
`chain` restore-to-scratch via the deletion-faithful `restore.Chain` path → `stock`,
the documented gpg/zstd/tar one-liner that proves recovery needs no NBackup), *records*
an inspectable ledger (`drill-ledger.json`, atomic temp+rename, no daemon — like the
catalog and run-status files), *classifies* failures (integrity / pipeline-key / chain
/ missing-copy — each a different remediation), and *exits non-zero* so a failed drill
is loud. Drill delivers the **"0 errors"** digit of 3-2-1-1-0; it also prints a posture
audit of the other digits. Pure parts (ledger, selection, taxonomy) live in package
`drill` (a leaf, like `retention`/`restore`); the I/O — verify, restore-to-scratch, the
WORM probe — lives in `engine`, which imports `drill`. Two run modes keep cron honest:
**attended** may prompt for a tape; **unattended** (auto when stdin is not a TTY)
attaches no operator and *skips* (not fails) any target whose copy would need a human
to load a reel — a coverage warning, never a non-zero exit, so a sampled nightly drill
rotates the fleet without paging on a tape that isn't loaded.

**Drill detects immutability; it never sets it (WORM probe).** The 3-2-1-1-0
"1 immutable" digit is verified, not configured, by NBackup: a drill keeps **one
fixed probe object** on the drilled medium and, each run, attempts to delete that same
object — a refused delete proves the storage enforces WORM/Object-Lock (the probe
persists, which *is* the proof); a successful delete proves it does not (the probe is
recreated next run, so an immutable medium accumulates exactly one undeletable probe,
not one per drill). Immutability is configured operator-side (S3 Object Lock, LTO
WORM) and NBackup runs least-privilege — it only detects and verifies it (see memory
`nbackup-immutability-cloud-side`). Append-only media (tape) are immutable by
construction and are reported without writing a probe. Honest cost: an
encrypted+compressed archive is all-or-nothing to read, so an offsite drill spends the
full bytes in egress — routine offsite drills default to the no-write `structural`
tier, and the dry-run forecasts the egress.

**Sync is batch copy, not a new subsystem (`nb sync` = Amanda's vault).** A
single-slot `CopySlot` already streams a slot from one medium to a target and
records a second `Placement` (idempotent: a slot already on the target is skipped).
`nb sync` is just that looped over a *selection* of source slots — every slot the
target is missing, **oldest-first** (a contiguous, replayable offsite copy; a slot's
full lands before its incrementals). It reuses `CopySlot` verbatim — same label
verification, same placement record, same per-slot atomicity — so an interrupted or
repeated sync resumes for free, and it **stops at the first hard error** (a full or
offline target won't fix itself by continuing). The source defaults to the landing
medium but is configurable (`--from` / rule `from:`): `CopySlot` resolves the
source placement and mounts it for reading via the same `Librarian.MountForRead`
path `readerFor` uses, so a tape/S3 source works (un-vaulting tape→disk, or a
second offsite tier), and copy-to-landing is now allowed (the old "target is the
source" guard became a `from == to` guard). The config `sync:` rules are the
declarative form (`{from, to, last}`) so a cron `nb dump && nb sync`
mirrors offsite hands-off. Sync and pruning are independent, not coupled:
retention is per-medium (`retention.Compute`'s floor is judged over one medium's own slots),
so a copy reaching another medium never makes the original prunable — double storage
keeps both copies, each retained on its own capacity and cycle. Tiering disk lean
while a cheap medium holds bulk is just a tighter disk `capacity`/`minimum_age`, which
`nb prune` enforces on disk alone.

**One mutating `nb` per config at a time** (`internal/lock`, Amanda's per-config
amflock). Rather than make the catalog concurrently writable, we serialize the
whole mutating run: every command that writes the catalog or media (`dump`,
`copy`, `label`, `load`, `rebuild`, `prune`) takes a
non-blocking advisory `flock` on `workdir/lock` before opening the engine, and a
second invocation fails fast (`ErrHeld`). flock is tied to the open fd, so a
crash releases it — no stale lockfiles. Read-only commands take no lock: catalog
writes land via atomic rename (write-tmp + `os.Rename`), so a reader always sees
a complete old-or-new cache. (Caveat: flock is unreliable over NFS; a workdir is
expected to be on a local filesystem. The lock is per *config workdir*, not per
medium — two configs sharing one physical volume are not yet guarded.)

**Encryption is source-tied and outermost (`package transform/crypt`).** Encryption is the
peer of compression, one stream transform further out: on write the pipeline is
**tar → compress → encrypt → meter → volume**; on read it reverses **decrypt →
decompress**. `transform/crypt` mirrors `transform/compress` — an external child (`gpg`),
selected by a registered scheme *name* (`gpg`/`none`), exposing the same reversible
`Filter` (a forward/reverse `programs.Cmd`) the engine places into a transfer. Three decisions carry their weight:
- **Outermost placement is load-bearing.** Because encryption sits *inside* the
  `xfer.Meter`, the seal's `SHA256` covers the *ciphertext* that lands on the
  volume. So `nb verify` and `CopySlot`/`nb sync` all
  operate on ciphertext and stay **keyless** — vaulting offsite, verifying
  integrity, and the medium-independent `Entry`/`Placement` identity (one slot,
  N byte-identical copies) are untouched. Only *extraction* needs the key.
- **Record the scheme name, never the key.** Each archive's header/seal carries
  `Encrypt: "gpg"` (a compiled-registry primitive, exactly like `Compress`), so
  restore reverses it from the artifact alone — config-free, the same
  rebuild-from-media property compression already has. The **key is never
  stored**: with a gpg public-key recipient the key-id travels inside the
  ciphertext and gpg resolves the private key from the operator's keyring, so a
  slot with archives under different keys (per-dumptype) just restores. Selection
  is config (`encrypt:` block, config-wide default or a whole-block per-dumptype
  override — no field merge); the *cipher* is a compiled scheme so the artifact
  never depends on config to be read.
- **The member index stays plaintext, deliberately.** Each archive's commit footer
  (checksums) and member index (filenames) are unencrypted, which is what lets `nb
  recover` and `nb rebuild` browse without the key (Amanda's plaintext-index property).
  The cost — filenames are readable on the medium — is a documented trade, not an
  oversight. (Deferred: per-medium at-rest encryption (S3 SSE / LTO hardware) for
  the "untrusted destination only" posture; client-side encryption with remote
  sources; an opt-in encrypted index for client-side-encrypted archives.)

**Media model.** A `Volume` is positional, self-describing files; framing differs
per medium (disk: a `.hdr` sidecar so the payload is a clean `.tar.<codec>`; tape:
a fixed 32 KB header block inline, since tape has no sidecars). `Open` is cheap;
`ReadFile` seeks by position; only `Files()` is a full scan (the rebuild path).
Normal ops resolve positions from the catalog and never scan.

**Cloud = an object store as a `Volume` (`media/cloud`).** One medium `type: cloud`
covers S3, GCS, Azure Blob, and any S3-compatible store, via the Go CDK
(`gocloud.dev/blob`); the backend is chosen by the bucket `url` scheme (`s3://`,
`gs://`, `azblob://`), with `file://`/`mem://` drivers making it fully testable
with no network or credentials. It is **address-identified, like disk** — a
bucket+key names a volume unambiguously, so it implements none of
`Labeled`/`Drive`/`Changer`/`Shelf` and runs no label/swap/spanning machinery, and
it registers `NewSizeProfile` (a byte budget reclaimed per slot). The on-store
layout is the disk medium's verbatim — `slots/<slot>/<NNNNNN>-<dle>-L<n>.tar.<ext>`
clean payload objects plus a `.hdr` sidecar — so a slot streams disk↔cloud
unchanged and a plain GET yields a stock-tool-restorable archive. Atomicity is the
same: payload object first, sidecar last, and a failed upload is aborted (not
committed), so an interrupted write leaves a sidecar-less orphan that scan/rebuild
ignores. Credentials come from each SDK's ambient environment, never the config.

**Tape = volumes behind one drive.** The `device` seam (the `mt` analogue, one
mounted tape) is shared by all shapes; the positioning surface differs and is what
the three medium-neutral interfaces capture. `dir:` is a directory-backed library
(each bay a subdir, finite per-bay `volume_size`, fully tested); `dir:` +
`mode: manual` is the disk-emulated single-drive station (reels are subdirs);
`device:` is a real single drive (`mt`+`/dev/nst0`; structurally complete, untested
without hardware).

- **Device vs environment — `Drive`, `Changer`, `Shelf`.** The shapes split on what
  *real hardware's software* can do, across three small seams. `media.Drive` is the
  device read both changer shapes share — `Loaded` (what volume is in the drive),
  embedding `media.Volume`. `media.Changer` **is** the robotic library: a `Drive`
  that also enumerates its bays and positions the robot (`Bays` + `Mount`). The shape
  is **one assertion** — *a `Changer` is a robotic library; anything that is not a
  `Changer` is a single-drive station or a plain volume.* A single drive is **not** a
  `Changer`: it has no robot and no bays, so it is a `Drive` plus a `Shelf`.
  `media.Shelf` is the **environment** — the operator-managed room (`Shelf` to
  enumerate the reels, `Insert` to load one) — because loading a reel a human keeps on
  a shelf is a physical act with no device API. The librarian consults `Shelf` **only
  to actually do a swap** (prompt over the room, then `Insert` the choice), never as a
  general shape marker. The disk-emulated station (`mode: manual`) implements `Shelf`
  functionally (its reels are subdirs it enumerates and inserts in-process); a real
  `device:` drive degenerately (empty room, `Insert` errors — only a human loads it).
  Reels are addressed by their own ids (`reel-01…`), never a synthetic "drive"
  position — `"drive"` is CLI presentation only. Media-shape dispatch lives behind
  the `media` shape interfaces: the librarian owns *positioning* (mount / advance /
  swap with the label protocol), and the one read-only *walk* the catalog rebuild
  needs — "every non-blank bay, else the loaded reel" — is `media.WalkReadable`, kept
  next to the `Changer`/`Drive` interfaces it asserts on so the catalog never
  type-asserts a `Volume` itself. The rest of the engine stays shape-agnostic.
- **Librarian — the operator-facing changer service.** Package `librarian` turns
  intents (make writable, advance, mount-for-read, label, load, inventory) into
  positioning, and runs the label protocol on top. The single unified algorithm —
  *try the mountable `Bays`, else ask the operator over the `Shelf`* — produces both
  user experiences from the inventory data: a robotic library iterates its many bays
  and rarely prompts; a single drive has one bay, so it prompts immediately. It is a
  shared service (dump, copy/sync, restore, rebuild, label, load all use it), so the
  future sub-engine split is mechanical.
- **Operator seam.** A single-drive station can't change its own tape, so when the
  loaded reel won't do, the librarian asks a `librarian.Operator` (CLI: stdin) to
  swap and retries — on writes (`PrepareWrite`/`Advance`: blank/foreign/wrong-pool/
  full → load a writable reel, auto-labeled if `auto_label`) and on reads
  (`MountForRead`: load the reel holding the needed label). Unattended (no operator)
  it degrades to an actionable error instead of blocking. A `reloadable` error marks
  the cases a swap can fix (vs a stale catalog, which a swap can't).
- **Expected tape (Amanda's "amdump will expect tape X").** `Engine.ExpectedTape`
  names the volume the next run will write to, derived from the catalog (the
  tapelist) and `retention.Compute`, never from a physical scan: a one-run-per-tape
  (non-appendable) run reuses the **oldest volume whose every run is unprotected**
  (past `minimum_age`, with a newer recovery path) — exactly Amanda's taper picking
  the oldest reusable tape — or a *fresh tape* when none is reusable; an appendable
  run extends the most recently written volume. `nb plan` prints it, and it seeds
  the swap prompt's suggestion (`SwapRequest.Expect`) so the operator is told *which*
  reel to load, not just "a fresh tape". This is **guidance only** — the engine
  still won't overwrite a reusable tape on its own; recycling it is a deliberate
  `nb label --relabel` (see deferred whole-volume recycle).
- **Bay/reel (physical) vs Label (logical) are distinct.** A `Changer` is
  **label-agnostic** — like a real robot it mounts bays and reads barcodes, never
  the magnetic label; the librarian reads the label *after* mounting. A blank
  cartridge has a bay but no label; relabel rewrites the label, same bay. The
  catalog references **labels** (durable data identity); bays/reels stay internal.
- **Finite volumes.** A write past `volume_size` hits `media.ErrVolumeFull`
  (end-of-tape), the partial file is discarded. Spanning sizes each part to fit
  *before* writing, so this is a backstop, not the normal path (see Spanning below).
- **Append vs one-run-per-tape.** `appendable: true` (default) is **Bacula-style**
  (pack many runs per tape until full); `appendable: false` is **Amanda-style**
  (one run per tape). This is a deliberate, named lineage choice — real tapes are
  physically appendable; Amanda chooses not to, Bacula does.
- **Spanning: a slot (and one archive) can cross volumes, proactively.** Both a
  **dump** and a **copy/sync** split work across tapes mid-archive — one DLE's
  compressed byte stream may itself span several tapes (Amanda's part/chunk model).
  The unit is the **part**: a contiguous byte-range of an archive's payload, its own
  self-describing file (header carries the part *index*; the archive's commit footer
  carries the part *count*). An archive is always a list of parts (one in the common case). Splitting
  is **proactive**: the operator sets `volume_size`, so the writer (`archiveio.Writer`
  via a `librarian.WriteSink`) sizes each part to the loaded volume's known remaining
  capacity (optionally capped by `part_size`) and rolls onto the next writable volume
  *between* parts — a robotic library mounts the next writable bay (blank →
  auto-labeled, or an empty in-pool tape — never a tape holding runs); a single-drive
  station prompts for a reel swap; an unbounded or changer-less medium writes one
  part. There is **no reactive "keep what fit on EOT"** and no holding-disk buffer
  (NBackup streams source→compressor→volume in one pass, so a part already on tape
  cannot be re-read to rewrite it). If a sized part *still* overflows (a wrong
  estimate, or a real drive whose remaining capacity software cannot see),
  `media.ErrVolumeFull` discards the partial and the run **fails** with an actionable
  message — we do not recover. The commit footer (written last, after the archive's
  parts and index) commits that archive; an interrupted span leaves an
  uncommitted, orphan set of parts, ignored by scan/rebuild and reclaimed by relabel —
  the same per-archive atomicity as a single-volume archive.
  Because a single drive cannot interleave two archives' parts, a spanning-capable
  landing **clamps workers to 1** (a single tape writes serially). Reads
  **auto-mount** the volume holding each part, in order — `archiveio`'s concatenating
  reader drains part *k* fully before mounting *k+1*, then reverses the codec over the
  concatenation. The roll/mount lives in `package librarian` (`WriteSink`,
  `Advance`, `MountForRead`), the one place that dispatches on medium shape.
  Real-drive (`device:`) spanning is proactive-via-`part_size` only and structurally
  complete but untested; the `dir:`-emulated library/station spans and is tested.

**Labels as a capability.** Verified before every write (refuse foreign / blank
unless `auto_label` / wrong-pool / relabeled-since-cached). Address-identified
media (disk, S3) carry no label and skip the whole dance.

**Network politeness is a per-medium throughput cap (the `nice` of bandwidth).**
NBackup already runs its child processes under `nice` for CPU politeness; a
cloud-first user needs the network analogue, or an uncapped `nb dump`/`nb sync` to
S3 saturates the office uplink during business hours (and once remote sources land,
the read side will too). The cap is a medium config knob — `throughput: 50MB/s`
(bytes/sec, `/s` optional) — because the thing being protected is the uplink *to a
given medium*. It is enforced by a token-bucket `xfer.Limiter`, a new in-process
stream stage that wraps the **medium-facing** stream: on write it paces the bytes
landing on the volume (inside `archiveio.Writer.drainParts`), which back-pressures the
one-pass pipeline through its pipe — no holding-disk buffer, and the wait is a timer
sleep so it cannot deadlock; on read it paces each part stream the medium hands back
(wrapped in `engine.partOpener`, the single choke point every restore / un-vault /
drill / sync-source read flows through). One `Limiter` is built per medium and
**shared**, so several concurrent workers to one medium draw from a single budget —
Amanda's global *netusage* ceiling, which here collapses into the per-medium cap
because a run writes a single landing medium (no separate global knob needed). The
default is uncapped: a nil `Limiter` returns the stream untouched, so a run with no
`throughput` set behaves byte-for-byte as before. It composes with `nice` (CPU) and
stays medium-neutral — it lives in `xfer`/`engine`, never in a medium package.
(Deferred: time-of-day awareness — a tighter cap during business hours.)

**Medium-neutral vocabulary.** The generic media/changer/config layer must not say
"tape": `bays`, `volume_size`, `throughput`, `media.ErrNoVolume`,
`media.Drive`/`Changer`/`Shelf`/`VolumeStatus`, `nb medium`, `nb load`.
Tape specifics (`type: tape`, the `tape` package, `mt`, `vtape`, the `reel`
vocabulary) stay local, so a future `usb`/removable-disk medium reuses the vocabulary.

**Archiver-neutral vocabulary (the same discipline, one layer up).** The generic
`archiver`/`catalog`/`engine`/`planner` layer must not say "tar" or "snapshot":
`Archiver`, `BackupRequest{DLE, Level, BaseLevel}`, `HasBase`, the recorded
`Archiver`/`archiver:` field, the `archivers:` config block, "incremental state". GNU tar
specifics (`tar`, `.snar`, the listed-incremental snapshot, `state_dir`,
`tar_path`) stay inside `archiver/gnutar`, so a future `ampgsql`/`amraw`-style archiver
reuses the vocabulary. The concurrency unit is a **worker** (`parallelism.workers`),
not a "archiver" — "archiver" means only the plugin.

**Execution is an injected transport; a backup/restore is one `xfer.Transfer` (`programs` + `xfer`).**
A backup is a chain of external programs — `tar → compress → encrypt` — then the in-process
server-side `meter → drainParts → volume`, then each archive's index + commit footer. The compressor and encryptor are *just
programs*, like `tar`, so there is **one** path. `package programs` is the base: a `Cmd`
plus an `Execution` (the host it runs on — `Local` or `SSH`) whose `RunPipe` runs a pipe of
commands on that one host. `package xfer` composes those single-host pipes into a
`Transfer(source, filters, sink)`: each transform lands in the **Source** zone (a client's
host, fused with tar), the local **Filters** zone (the server), or the **Sink** zone (the
medium, or a target's host) — so a transform never runs on a third *remote* host, and the
wire is crossed only between zones. A fault is tagged with its zone, which is exactly the
Pipeline-vs-Chain/Structural split the drill and verify layers classify on. So a local dump
(everything `Local`), a thin client (tar on `SSH`, transforms in local Filters), and a fully
client-side dump (tar+compress+encrypt all in the `SSH` Source — only ciphertext crosses to
the server Sink) are the *same* code with different zone placement. The
transport lives in one neutral package and is injected into archivers via
`archiver.Open(name, opts, ex)` — SSH is part of **no** archiver, so a new archiver gets
remote execution for free as long as its binaries are on the client. The meter stays
server-side, so each archive's commit footer still covers the bytes that land (verify/copy/sync stay keyless).
This is the source-side peer of the medium-neutral discipline; see
[docs/design/remote-sources.md](docs/design/remote-sources.md) for the configurable
`compress`/`encrypt.at` point and the three key-trust postures.

**Run monitoring is a status file, not a daemon.** `nb dump` drives a
`progress.Tracker` whose workers report start / live bytes / finish; the tracker
flushes a single JSON snapshot to `<workdir>/run-status.json` (atomic temp+rename,
byte updates throttled to 1 s, state changes forced). `nb status` is a *separate*
process that just reads and renders that file — Amanda's amdump-log + amstatus
split, minus the daemon, which fits "state lives in inspectable files." It needs no
engine (no media scan), so it is cheap to poll, and the final `done`/`failed`
snapshot is left in place as the last-run record. Progress reporting never blocks
or fails a backup (a write error is a stderr warning). **Faithful adaptation:**
NBackup has no holding disk — each DLE streams source→compressor→volume in one
pass — so Amanda's separate dumper/taper queues collapse to one `dumping` state
per DLE, metered by uncompressed bytes against the planner estimate. The
measurement point is the source stage's byte tap (`programs.Cmd.Tap`) on the
tar→compressor stream, wired in the engine's `backupItem`; compressed bytes come
from `archiveio`'s streaming meter as the payload drains into parts (both feed the same
per-DLE counters, throttled so they can be polled live).

**Reporting + alerting make an unwatched failure loud (`nb report`, `notify:`).**
Where `progress` is the *live* run-status of one in-flight dump, `report` is the
*historical* record of finished runs across every command, and `notify` pushes a
failure to a human — the "0 errors" half of 3-2-1-1-0 only matters if a non-zero
result reaches someone. Three load-bearing choices, all mirroring existing stances:
- **One seam, not per-command.** Every run-producing command (`dump`, `sync`,
  `prune`, `verify`, `drill`) runs its body through `cli.runReported`, which stamps
  the outcome, appends a uniform `report.Run` to `<workdir>/run-log.jsonl` (one
  compact JSON line; the latest also written as `run-summary.json` for a monitor to
  scrape), and dispatches notifications. The engine is **unchanged** — it already
  returns rich reports and already exits non-zero on failure; recording is pure CLR
  glue over two new leaf packages. Dry-runs record nothing.
- **Recording is best-effort, exit codes are sacred.** A summary-write or
  notification error is a stderr warning and never changes — nor suppresses — the
  run's own exit code (the `progress.NewFileSink` contract). `runReported` returns
  the body's error verbatim.
- **Failures are always loud; a successful `dump` is loud too, by default.** Any
  command alerts on failure (every backend unless `on_failure` narrows it). Routing
  also notifies on a *successful* `dump` by default — the nightly "backups happened"
  signal, so a silent inbox reads as "cron didn't run" rather than "all is well";
  the other commands' success stays opt-in via `on_success` (which, when set, applies
  to every command). This is the one place routing keys on the command, kept in
  `notify.routeFor`, not smeared across the seam.
- **History is append-only JSONL; alerts are a registry; secrets are env-refs.**
  The log appends (O(1)) and compacts to a bounded tail, and a reader tolerates a
  torn trailing line (the one unlocked writer, `nb verify`, may race `nb report`).
  A notify backend is a registered name (`smtp`/`webhook`) like `transform/compress`/`crypt`, so
  adding a channel is a registration. Secrets (SMTP password, webhook URL) are named
  environment variables resolved at send time, never stored — and a literal
  `password:`/`token:` key is rejected structurally by `KnownFields(true)`. `nb
  report` (read-only, no engine) renders the recent history plus a live
  drill-ledger recovery audit (failing / degrading / stale / never-drilled DLEs via
  `drill.Ledger.Coverage`); `nb report --notify` mails the same digest.
- **Per-DLE dump report (Amanda's nightly statistics).** A dump's record carries a
  per-DLE `DumpStats` row (level, original/output size, files — from the seal — plus
  dump time read from the just-flushed `run-status.json`), captured by the CLI at seal
  time so the report is historical, not just the last live run. `nb report --dump`
  (latest, or `--slot <id>`) renders the Amanda-style table — sizes, compression %,
  time, rate, full/incremental totals — via the one `report` renderer the dump
  *notification* also uses, so a configured dump alert *is* the nightly report, not a
  bare "it worked". (A slot older than the history shows sizes via `nb slot show`.)

**Reclamation asymmetry.** Disk/S3 reclaim per slot (`RemoveSlot`); tape reclaims a
whole volume (relabel — `tape.RemoveSlot` errors, and `volumeProfile.Reclaim`
returns nothing, so `nb prune` never deletes a slot from a tape). Pruning has a
safety floor (`retention.Compute`: younger than `minimum_age`, or part of a DLE's
**live recovery chain** — its last full plus every later incremental, since
`restore.Chain` replays them all; a recent slot also pins the older base its restore
needs, so reclamation never breaks a chain it leaves restorable) plus a per-medium
capacity strategy. Both are per-medium: the floor's rule is shared but is judged over
one medium's own slots, so a copy on another medium never makes a slot reclaimable.

**Capacity model (`media.Profile`).** A profile exposes two numbers that the
planner keeps distinct. `TotalBytes` is the **pool** — the retainable capacity
(`volumes × volume_size` for a tape library, `capacity` for an object store) — and
drives reclamation and the structural cycle check (can a complete recovery set be
retained at all). `VolumeSize` is one **reel**, the basis of the per-run ceiling:
a run fills the reel it lands on before spilling to the next, so a single run can
never exceed one reel. The engine's `capacityRoom` feeds the planner the tighter
of the two — pool free room (`capacity − protected`) and the landing reel's
remaining room (`volume_size −` what's already on it). They are genuinely
separate: a **bare drive** (`type: tape`, `device:`) has an unbounded pool (the
operator's shelf is unknowable, `TotalBytes == 0`) but a finite reel. The volume
profile reads the same count key the changer does — `bays` for a library, `reels`
for a manual single-drive station — so the planner's capacity never disagrees with
the medium it lands on.

**Cost model (`media.Cost`) — a medium prices itself, in dollars, offline.** The
persona reasons in dollars per month, not bytes, and the bill's surprises are the
non-storage charges (chiefly egress on a restore). Because NBackup already accounts
bytes precisely — estimates, forecast, capacity — pricing is a **thin pure
calculation on top**, the dollar peer of `media.Profile`'s bytes. `media.Cost` is
one medium's *flat* rate table: a storage `$/GiB-month`, an egress `$/GiB`, and a
GET `$/1000`. Four decisions carry it:
- **A medium prices itself, like it sizes itself.** Pricing is a per-medium concern
  registered exactly like capacity: `media.RegisterCost`/`OpenCost` mirror
  `RegisterProfile`/`OpenProfile`, and `media/cloud` owns the provider rate tables
  (`aws-s3`/`gcs`/`azure-blob`/`generic-cloud`) and the URL-scheme inference that
  picks one. The core never learns provider pricing; the engine just calls
  `OpenCost` and consumes a dollar number the way it consumes profile bytes. A medium
  type with no registered cost (disk, tape) is **unpriced** — the zero `Cost`, no
  recurring cloud bill — and the CLI suppresses its cost output, mirroring how an
  unregistered profile is unbounded.
- **Zero-config, with overrides.** With no `cost:` block a cloud medium reads its
  bucket URL scheme (`s3://` = AWS, `gs://` = GCS, `azblob://` = Azure; anything else
  → a generic cloud table) so `nb plan` shows a monthly bill out of the box. The
  optional `cost:` block names a different provider table or overrides individual
  rates (a region's egress, an S3-compatible provider) — flattened into the factory's
  options like `ProfileOptions`, validated at load.
- **No lifecycle tiers, deliberately.** NBackup does **not** model storage-class
  transitions (Glacier/Deep Archive). Which tier bytes physically sit in is
  operator-configured bucket-side; a forecast of it would more often be wrong than
  useful, and the machinery (per-class rates, retrieval fees + latency, minimum-
  retention floors, an age→class schedule) is complexity for accuracy NBackup can't
  deliver. Flat pricing is the honest estimate. (Considered and removed.)
- **Estimation only, fully offline; the overlay reuses the byte machinery.** It is a
  pure calculation over the catalog and the rate table — **no billing API** — so it
  runs wherever planning runs and never touches a slow/offline volume (the provider
  invoice stays authoritative; the tables are list-price estimates). The dollar
  overlay lives in `engine/cost.go` (`CostSummary`, `ForecastCost`,
  `RestoreCost`/`SelectionCost`), mirroring the capacity overlay
  (`StoredBytes`/`CapacityStatus`). `ForecastCost` walks the existing run simulation
  day by day, growing a footprint with each simulated run and evicting it with the
  medium's own `Reclaim` + `retention.Compute` (the primitives `nb prune` uses), then
  reprices the survivors — so the `$/month` curve reflects fulls/incrementals landing
  and pruning reclaiming. The read paths (`nb restore`/`recover`/`drill`) price the
  egress of the chain they will read off the chosen copy and, when material, warn —
  prompting interactively, never blocking a cron read (it prints the estimate and
  proceeds, like the unattended drill). An offsite drill spends the full bytes
  (encrypted+compressed is all-or-nothing), so its dry-run egress now carries a `$`.

## Conventions for working here

- **Commits:** only when the user explicitly says so. **Never push** (no
  credentials). End commit messages with the `Co-Authored-By` trailer.
- **Amanda-faithful:** research upstream behavior before inventing; prefer
  orchestrating external tools as processes. See memory `nbackup-amanda-faithful`.
- **Greenfield, pre-release:** no back-compat shims, no migrations; don't add
  concepts or layers speculatively. See memory `nbackup-greenfield`.
- **Verify** every change: `gofmt -l`, `go vet ./...`, `go test -race ./...`.
- **Test environment:** `zstd` is **not** installed — tests use codec `none`;
  `tar`, `gzip`, `nice` are present. Tests that need GNU tar `t.Skip` when absent.
- **CLI:** flags may appear before or after positionals (`parseArgs`); subcommand
  dispatch (`slot show`) keys on the first arg. The convention is **inspect with a
  noun** (`nb slot`, `nb medium`), **act with a flat verb** (`nb dump`, `nb check`, `nb verify`,
  `nb drill`, `nb prune`, `nb rebuild`, …) — so the nouns carry only read subcommands and every
  mutation is a top-level verb. Per-medium status (incl. bays / drive + shelf) lives
  in `nb medium <name>`; `nb load` is the one physical action verb (sibling of
  `nb label`). `--catalog` has no short flag (a case-only `-C`/`-c` pair is too easy
  to slip).

## Deferred / known next steps

- **Whole-volume recycle** on EOT. Spanning rolls onto the next *blank / empty
  in-pool* tape; auto-recycling an aged-out tape (vs. relabeling it) is still manual
  (`nb label --relabel`). (Capacity-driven retention is otherwise implemented:
  `sizeProfile.Reclaim` already prunes object stores and disk to fit `capacity`;
  only whole-*volume* tape recycle remains.)
- **Remote sources over SSH** — the dump path is implemented (see below and
  [docs/design/remote-sources.md](docs/design/remote-sources.md)); a remote DLE dumps over
  SSH with no NBackup software on the client. A source host is remote **by default** (local
  means exactly `localhost`); a top-level `ssh:` block sets global SSH defaults that a
  per-host `hosts:` entry overrides. **`nb check`** is the amcheck analogue — it verifies
  the server and every host (connecting to remote clients by default, `--offline` to skip),
  every probe running through the host's executor so local and remote checks share one code
  path. Whole-DLE restore is opt-in onto a client (`nb recover --all --to host:path`), and
  for an `encrypt.at: client` DLE the decode runs on the client (the key never leaves it); a
  server-side restore of a client-only symmetric key fails fast. The remaining follow-on is
  the drill recoverability tiers and file-level recover on the client. SSH paths are untested
  in CI (no sshd; the client-side encrypt+decode round-trip is covered locally with real
  gpg/gzip/tar).
- Real `mtDevice` hardware validation — also the only spanning path not exercised
  (real-drive spanning is proactive-via-`part_size` and structurally complete but
  untested; the `dir:` emulator spans and is tested).

For user-facing usage, config, and the restore-with-stock-tools story, see the
[README](README.md).
