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
  addressed by position (Amanda's Device API). Disk, tape, S3 all map to it.
- **Catalog Entry / Placement** — the catalog separates *what a slot is* (one
  medium-independent `Entry`, from the seal) from *where its copies live* (N
  `Placement`s, each a volume + the file position of every archive).
- **Label** — logical identity written on a labeled volume's file 0 (magic, name,
  pool, epoch). A *capability* (`media.Labeled`); address-identified media skip it.
- **Bay** — a physical position in a tape library (`bay-01…`), the durable
  cartridge identity. Distinct from the Label written inside it.

## Package map

Mechanism lives behind interfaces with named, registered implementations; one
orchestrator (`engine`) composes them. Adding a medium, method, or codec is a
registry registration, not a conditional in the core.

| Package | Responsibility | Amanda analogue |
|---|---|---|
| `config` | config + domain entities: `DLE`, `Media`, `DumpType` | disklist / dumptype / storage |
| `slot` | slot metadata: pure data + lifecycle (`NewSlot`/`AddArchive`/`Seal`) | header / amar |
| `slotio` | maps a slot onto a `Volume`'s files (headers, seal, verify, `Expect`) | taper / amrestore |
| `media` | `Volume` + `Labeled` + `Changer` + `Profile` + registry | Device API |
| `media/disk`, `media/tape`, `media/s3` | Volume impls (disk sidecar headers; tape library; s3 stub) | vfs / tape / s3 devices |
| `method` + `method/gnutar` | dump `Method` interface + registry; GNU tar impl | Application API / amgtar |
| `filter` | external compressor child processes (zstd/gzip/none) + registry | compress |
| `xfer` | in-process stream metering: checksum + byte counting | Xfer API |
| `progress` | live run-status model + status-file I/O + render | amdump log / amstatus |
| `catalog` | local cache of slot index + volume registry + snapshot library; derives `History` | catalog / curinfo / tapelist |
| `policy` | retention safety floor: protected slots (pure) | policy |
| `planner` | multilevel level scheduling (pure) | planner |
| `engine` | the driver: parallel dumpers, wires planner→method→filter→media→catalog | driver / taper |
| `cli` | thin command wiring | amdump / amadmin |

Dependencies flow one way: `cli → engine → {planner, policy, method, filter,
slotio, catalog, config, progress}` over leaf packages `{media, xfer, slot,
sizeutil}`.
Domain packages stay pure; `method`/`media`/`filter` are pluggable adapters;
`engine` is the only component aware of all of them. A backup is a pipeline of
processes: **source** (`tar` via `method.Backup`) → **filter** (compressor child)
→ **dest** (`media.Volume`), metered by `xfer`, composed by `slotio`.

## Load-bearing decisions (the *why*)

**Slot is the addressable run + commit boundary.** The seal record (written last)
is the atomic "this run completed" marker. It is *not* merely a cache of the
archive headers: it holds the per-archive **integrity and content** that the
on-volume framing headers deliberately omit — `SHA256`, member list, sizes. So a
slot's *shape* is reindexable from headers, but its *trust* and *contents* are not;
the manifest stays. (Slot earns its keep; we considered and rejected dropping it.)

**The catalog is a cache; the media are the source of truth.** Every file is
self-describing (header), every slot sealed, every labeled volume carries its
label — so one `Files()` scan rebuilds everything (`nb catalog rebuild`): seals →
slots, labels → volume registry. The catalog lives in its **own `workdir`**
(default `nbackup-catalog`), *independent of any medium* — it is a cache over the
whole pool, not part of one medium. The `Entry`/`Placement` model means a slot
copied disk→tape is one Entry with two Placements; restore/verify pick any
available copy. Run `History` is *derived* from cached slots (no second source to
drift). The only non-derivable local state is the GNU tar snapshot library
(`snapshots/…/L<n>.snar`) — precious; losing it forces a full.

**One mutating `nb` per config at a time** (`internal/lock`, Amanda's per-config
amflock). Rather than make the catalog concurrently writable, we serialize the
whole mutating run: every command that writes the catalog or media (`dump`,
`copy`, `label`, `changer load`, `catalog rebuild`, `prune --apply`) takes a
non-blocking advisory `flock` on `workdir/lock` before opening the engine, and a
second invocation fails fast (`ErrHeld`). flock is tied to the open fd, so a
crash releases it — no stale lockfiles. Read-only commands take no lock: catalog
writes land via atomic rename (write-tmp + `os.Rename`), so a reader always sees
a complete old-or-new cache. (Caveat: flock is unreliable over NFS; a workdir is
expected to be on a local filesystem. The lock is per *config workdir*, not per
medium — two configs sharing one physical volume are not yet guarded.)

**Media model.** A `Volume` is positional, self-describing files; framing differs
per medium (disk: a `.hdr` sidecar so the payload is a clean `.tar.<codec>`; tape:
a fixed 32 KB header block inline, since tape has no sidecars). `Open` is cheap;
`ReadFile` seeks by position; only `Files()` is a full scan (the rebuild path).
Normal ops resolve positions from the catalog and never scan.

**Tape = a library of bays behind one drive**, two internal seams: `device` (the
`mt` analogue, one mounted tape) and `changer` (the robot analogue, which bay is
loaded). `dir:` is a directory-backed library (each bay a subdir, finite per-bay
`volume_size`, fully tested); `device:` is a real single drive (`mt`+`/dev/nst0`,
a one-bay library; structurally complete, untested without hardware).

- **Two changer kinds — robot vs single drive.** They differ on one axis: *who
  picks the reel.* A **robotic library** (`dir:`, the default) reports many bays
  and a command moves the mounted *position* between them. A **single-drive
  station** (`dir:` + `mode: manual`, a `media.ManualChanger`) reports exactly one
  bay (`drive`) whose *content* the operator swaps by hand; the other reels sit on
  an offline **shelf** the changer can't enumerate (no barcode reader off the
  drive). `Insert` swaps a shelf reel into the one drive — the bay's content
  changes, the bay never does. The disk emulator covers both; a real `device:`
  drive is the single-drive case in hardware.
- **Operator seam.** A single-drive station can't change its own tape, so when the
  loaded reel won't do, the engine asks an `Operator` (CLI: stdin) to swap and
  retries — on writes (`verifyWritable`: blank/foreign/wrong-pool/full → load a
  writable reel, auto-labeled if `auto_label`) and on reads (`mountForRead`: load
  the reel holding the needed label). Unattended (no operator) it degrades to an
  actionable error instead of blocking. A `reloadable` error marks the cases a swap
  can fix (vs a stale catalog, which a swap can't).
- **Bay (physical) vs Label (logical) are distinct.** The `changer` is
  **label-agnostic** — like a real robot it mounts bays and reads barcodes, never
  the magnetic label; the engine reads the label *after* mounting. A blank
  cartridge has a bay but no label; relabel rewrites the label, same bay. The
  catalog references **labels** (durable data identity); bays stay internal.
- **Finite volumes.** A write past `volume_size` hits `media.ErrVolumeFull`
  (end-of-tape), the partial file is discarded.
- **Append vs one-run-per-tape.** `appendable: true` (default) is **Bacula-style**
  (pack many runs per tape until full); `appendable: false` is **Amanda-style**
  (one run per tape). This is a deliberate, named lineage choice — real tapes are
  physically appendable; Amanda chooses not to, Bacula does.
- **Manual switching (no auto-advance).** On a robotic library a full/foreign/wrong
  tape is refused and the operator `nb changer load`s the next bay (or `nb label
  --relabel`s an aged one); reads **auto-mount** the bay holding each placement's
  label. On a single-drive station the same situations prompt for a physical reel
  swap (above). Either way switching is operator-driven — the engine never advances
  a tape on its own (see deferred auto-advance).

**Labels as a capability.** Verified before every write (refuse foreign / blank
unless `auto_label` / wrong-pool / relabeled-since-cached). Address-identified
media (disk, S3) carry no label and skip the whole dance.

**Medium-neutral vocabulary.** The generic media/changer/config layer must not say
"tape": `bays`, `volume_size`, `media.ErrNoVolume`, `media.Changer`/`BayStatus`,
`nb changer`. Tape specifics (`type: tape`, the `tape` package, `mt`, `vtape`)
stay local, so a future `usb`/removable-disk medium reuses the vocabulary.

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

**Reclamation asymmetry.** Disk reclaims per slot (`RemoveSlot`); tape reclaims a
whole volume (relabel). Pruning has a shared safety floor (`policy.Protected`:
younger than `minimum_age`, or the last recovery path for some DLE) plus a
per-medium capacity strategy.

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
  dispatch (`slot show`, `changer load`) keys on the first arg.

## Deferred / known next steps

- Tape **auto-advance & whole-volume recycle** on EOT (today: manual switching —
  a run must fit the loaded tape; end-of-tape mid-run aborts the run uncommitted,
  retry on a fresh tape). The swap prompt fires at tape *selection*, not mid-write.
- **Tape spanning** — one archive (or a run's archives) split across two tapes.
  Blocked on the catalog `Placement` model, which binds one slot copy to one
  volume + the seal that commits it; spanning means reworking Placement, the
  cross-tape seal/commit, and per-volume rebuild. Deliberately deferred.
- **S3** Volume implementation (registered stub today).
- **Budget-driven retention** — budget is reported; pruning is cycle-based.
- **Remote sources** — `host` is metadata; `path` is read locally.
- Real `mtDevice` hardware validation.

For user-facing usage, config, and the restore-with-stock-tools story, see the
[README](README.md).
