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
orchestrator (`engine`) composes them. Adding a medium, method, or codec is a
registry registration, not a conditional in the core.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | disklist / dumptype / storage |
| `slot` | slot metadata: pure data + lifecycle (`NewSlot`/`AddArchive`/`Seal`) | header / amar |
| `slotio` | maps a slot onto a `Volume`'s files (headers, seal, verify, `Expect`) | taper / amrestore |
| `media` | `Volume` + `Labeled` + `Drive`/`Changer` (device) + `Shelf` (environment) + `Profile` + registry | Device API |
| `librarian` | operates a medium's `Changer`/`Shelf` + label protocol (make-writable, advance, mount, label, load) | changer / amtape |
| `media/disk`, `media/tape`, `media/cloud` | Volume impls (disk sidecar headers; tape library; object store via gocloud.dev/blob) | vfs / tape / s3 devices |
| `media/fslike` | the slot layout shared by the address-identified media — clean payloads + `.hdr` sidecars over a small `Store` seam (disk = a directory, cloud = a bucket), so disk↔cloud copies are byte-identical | — |
| `method` + `method/gnutar` | dump `Method` interface + registry; GNU tar impl | Application API / amgtar |
| `filter` | external compressor child processes (zstd/gzip/none) + registry | compress |
| `crypt` | external encryptor child processes (gpg/none) + registry | amcrypt/amgpgcrypt |
| `streamproc` | the shared "external stream-transform child" plumbing (stdin→stdout, optional `nice`) that `filter` and `crypt` both run on | — |
| `xfer` | in-process stream metering: checksum + byte counting | Xfer API |
| `progress` | live run-status model + status-file I/O + render | amdump log / amstatus |
| `catalog` | local cache of slot index + volume registry + snapshot library; derives `History` | catalog / curinfo / tapelist |
| `policy` | retention safety floor: protected slots (pure) | policy |
| `restore` | the archive chain to rebuild a DLE as of a slot (pure) | amrestore |
| `recovery` | as-of-date browse tree + per-archive file selection (pure) | amrecover |
| `drill` | recovery-drill ledger + risk-biased selection + failure taxonomy (pure) | amverify (orchestrated) |
| `planner` | multilevel level scheduling (pure) | planner |
| `engine` | the driver: parallel dumpers, wires planner→method→filter→media→catalog | driver / taper |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, policy, method, filter,
crypt, slotio, catalog, config, progress, restore, recovery}` over leaf packages
`{media, xfer, slot, sizeutil}` (`recovery` builds on `restore`).
Domain packages stay pure; `method`/`media`/`filter`/`crypt` are pluggable adapters;
`engine` is the only component aware of all of them. A backup is a pipeline of
processes: **source** (`tar` via `method.Backup`) → **filter** (compressor child)
→ **crypt** (encryptor child) → **dest** (`media.Volume`), metered by `xfer`,
composed by `slotio`.

## Load-bearing decisions (the *why*)

**Slot is the addressable run + commit boundary.** The seal record (written last)
is the atomic "this run completed" marker. It is *not* merely a cache of the
archive headers: it holds the per-archive **integrity and content** that the
on-volume framing headers deliberately omit — `SHA256`, member list, sizes. So a
slot's *shape* is reindexable from headers, but its *trust* and *contents* are not;
the manifest stays. (Slot earns its keep; we considered and rejected dropping it.)

**The catalog is a cache; the media are the source of truth.** Every file is
self-describing (header), every slot sealed, every labeled volume carries its
label — so one `Files()` scan rebuilds everything (`nb rebuild`): seals →
slots, labels → volume registry. The catalog lives in its **own `workdir`**
(default `nbackup-catalog`), *independent of any medium* — it is a cache over the
whole pool, not part of one medium. The `Entry`/`Placement` model means a slot
copied disk→tape is one Entry with two Placements; restore/verify pick any
available copy. Run `History` is *derived* from cached slots (no second source to
drift). The only non-derivable local state is the GNU tar snapshot library
(`snapshots/…/L<n>.snar`) — precious; losing it forces a full.

**Recover is amrecover without an index server.** Amanda runs a separate index
server holding per-dump gzipped path lists so `amrecover` can browse without
reading tapes. NBackup needs none: the **member list is already in every seal**
(`slot.Archive.Members`), which the catalog caches, so `nb recover` browses by
reading the catalog alone — media is touched only on extract. `recovery.BuildTree`
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
`drill` (a leaf, like `policy`/`restore`); the I/O — verify, restore-to-scratch, the
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
retention is per-medium (`policy.Protected` is judged over one medium's own slots),
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

**Encryption is source-tied and outermost (`package crypt`).** Encryption is the
peer of compression, one stream transform further out: on write the pipeline is
**tar → compress → encrypt → meter → volume**; on read it reverses **decrypt →
decompress**. `package crypt` mirrors `filter` — an external child (`gpg`),
selected by a registered scheme *name* (`gpg`/`none`), with the same proc
plumbing. Three decisions carry their weight:
- **Outermost placement is load-bearing.** Because encryption sits *inside* the
  `xfer.Meter`, the seal's `SHA256` covers the *ciphertext* that lands on the
  volume. So `nb verify` and `CopySlot`/`nb sync` all
  operate on ciphertext and stay **keyless** — vaulting offsite, verifying
  integrity, and the medium-independent `Entry`/`Placement` identity (one slot,
  N byte-identical copies) are untouched. Only *extraction* needs the key.
- **Record the scheme name, never the key.** Each archive's header/seal carries
  `Encrypt: "gpg"` (a compiled-registry primitive, exactly like `Codec`), so
  restore reverses it from the artifact alone — config-free, the same
  rebuild-from-media property compression already has. The **key is never
  stored**: with a gpg public-key recipient the key-id travels inside the
  ciphertext and gpg resolves the private key from the operator's keyring, so a
  slot with archives under different keys (per-dumptype) just restores. Selection
  is config (`encrypt:` block, config-wide default or a whole-block per-dumptype
  override — no field merge); the *cipher* is a compiled scheme so the artifact
  never depends on config to be read.
- **The seal stays plaintext, deliberately.** It holds the member list
  (filenames) and checksums; keeping it unencrypted is what lets `nb recover` and
  `nb rebuild` browse without the key (Amanda's plaintext-index property). The
  cost — filenames are readable on the medium — is a documented trade, not an
  oversight. (Deferred: per-medium at-rest encryption (S3 SSE / LTO hardware) for
  the "untrusted destination only" posture; client-side encryption with remote
  sources; an opt-in encrypted seal.)

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
  tapelist) and `policy.Protected`, never from a physical scan: a one-run-per-tape
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
  self-describing file (header carries the part *index*; the seal carries the part
  *count*). An archive is always a list of parts (one in the common case). Splitting
  is **proactive**: the operator sets `volume_size`, so the writer (`slotio.Writer`
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
  message — we do not recover. The seal (written last, on the final volume) commits
  the whole slot; an interrupted span leaves seal-less orphan parts, ignored by
  scan/rebuild and reclaimed by relabel — the same atomicity as a single-volume slot.
  Because a single drive cannot interleave two archives' parts, a spanning-capable
  landing **clamps dumpers to 1** (a single tape writes serially). Reads
  **auto-mount** the volume holding each part, in order — `slotio`'s concatenating
  reader drains part *k* fully before mounting *k+1*, then reverses the codec over the
  concatenation. The roll/mount lives in `package librarian` (`WriteSink`,
  `Advance`, `MountVolume`), the one place that dispatches on medium shape.
  Real-drive (`device:`) spanning is proactive-via-`part_size` only and structurally
  complete but untested; the `dir:`-emulated library/station spans and is tested.

**Labels as a capability.** Verified before every write (refuse foreign / blank
unless `auto_label` / wrong-pool / relabeled-since-cached). Address-identified
media (disk, S3) carry no label and skip the whole dance.

**Medium-neutral vocabulary.** The generic media/changer/config layer must not say
"tape": `bays`, `volume_size`, `media.ErrNoVolume`,
`media.Drive`/`Changer`/`Shelf`/`VolumeStatus`, `nb medium`, `nb load`.
Tape specifics (`type: tape`, the `tape` package, `mt`, `vtape`, the `reel`
vocabulary) stay local, so a future `usb`/removable-disk medium reuses the vocabulary.

**Run monitoring is a status file, not a daemon.** `nb dump` drives a
`progress.Tracker` whose dumpers report start / live bytes / finish; the tracker
flushes a single JSON snapshot to `<workdir>/run-status.json` (atomic temp+rename,
byte updates throttled to 1 s, state changes forced). `nb status` is a *separate*
process that just reads and renders that file — Amanda's amdump-log + amstatus
split, minus the daemon, which fits "state lives in inspectable files." It needs no
engine (no media scan), so it is cheap to poll, and the final `done`/`failed`
snapshot is left in place as the last-run record. Progress reporting never blocks
or fails a backup (a write error is a stderr warning). **Faithful adaptation:**
NBackup has no holding disk — each DLE streams source→compressor→volume in one
pass — so Amanda's separate dumper/taper queues collapse to one `dumping` state
per DLE, metered by uncompressed bytes against the planner estimate. The new
measurement point is an uncompressed `xfer.Counter` on the tar→compressor stream
in `slotio.WriteArchive`; compressed bytes come from the existing `xfer.Meter`
(now atomic so it can be polled live).

**Reclamation asymmetry.** Disk/S3 reclaim per slot (`RemoveSlot`); tape reclaims a
whole volume (relabel — `tape.RemoveSlot` errors, and `volumeProfile.Reclaim`
returns nothing, so `nb prune` never deletes a slot from a tape). Pruning has a
safety floor (`policy.Protected`: younger than `minimum_age`, or the last recovery
path for some DLE) plus a per-medium capacity strategy. Both are per-medium: the
floor's rule is shared but is judged over one medium's own slots, so a copy on
another medium never makes a slot reclaimable.

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
  noun** (`nb slot`, `nb medium`), **act with a flat verb** (`nb dump`, `nb verify`,
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
- **Remote sources** — `host` is metadata; `path` is read locally.
- Real `mtDevice` hardware validation — also the only spanning path not exercised
  (real-drive spanning is proactive-via-`part_size` and structurally complete but
  untested; the `dir:` emulator spans and is tested).

For user-facing usage, config, and the restore-with-stock-tools story, see the
[README](README.md).
