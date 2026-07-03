# FS restructure — packages, layers, and names

NBackup's storage stack deliberately mirrors a filesystem: media are opened like
devices, archives are mapped onto volume files like blocks, and operations read
and write archives by logical name like files. The structure was sound; the
naming and package boundaries had drifted — fs contracts stranded in the block
layer, the device-open layer split between `librarian` (interfaces) and a
private `engine` struct (the depot), and interface names (`Store`,
`WriteStore`, `Deps`, `Medium`) that didn't say which layer or scope they
belonged to. This doc records the corrected structure and names; it is the map
the restructure was executed against.

## The layer × scope matrix

Top is closest to the user; bytes flow down on write, up on read. Each cell:
**interface — implementer** (granularity of one instance in parens).

| Layer *(fs analogue)* | Read side | Write side | Admin side |
|---|---|---|---|
| **operations**: dumper, restorer, verifier, copier *(userspace programs)* | consume `ReadStore` | dumper consumes `Ingest`; copy/flush consume `WriteStore` | checker, driller, label/load consume `AdminMedium` |
| **spool** *(writeback / IO scheduler)* | — | `archivefs.Ingest` — **Spool** (one run, all lanes); routes each lane's `PartAllocator` + `Recorder` through one orchestrator | — |
| **archivefs** *(file layer / VFS)* | `ReadStore` — **FS** (global: all runs, all media) | `WriteStore` = `Recorder` + read-back/reclaim — **Session** (one run on one medium) | — |
| **archiveio** *(block layer)* | `Reader` — bound to a `PartOpener` (one medium end); `Open(ref, parts)` → `io.ReadCloser` (one archive) | `Writer` — bound to a `PartAllocator` + `Recorder` (one run); `NewArchive(spec)` → `*ArchiveWriter` (one archive) | — |
| **depot** *(open(2) + mount table + claims)* | `ReadMedium` — minted by **Depot** | `WriteMedium` — minted by **Depot**, takes the window's write claim | `AdminMedium` — minted by **Depot** |
| **librarian** *(volume manager / autoloader)* | `MountForRead` / `ReadFileAt` on **Librarian** (methods, no named object) | `Allocator` (one drive) — implements `PartAllocator` | `Label` / `Load` / `Advance`; `Operator` (the human) |
| **media** *(devices, /dev)* | `Volume.ReadFile` | `Volume` write path | `Labeled`, `Changer.Load/Unload`, `Drive` |
| **catalog** *(inode + volume table — sidecar, not on the byte path)* | `View` (window snapshot) | `Window` (the run's write claim) | rebuild / scan |
| **record** *(on-medium format)* | shared vocabulary both directions: `Ref`, `Header`, `FilePos`, `Archive`, `Label` | | |

## The naming grid

Scope prefix × layer noun — the noun tells you the layer, the prefix tells you
the side:

| noun *(layer)* | Read | Write | Admin |
|---|---|---|---|
| `…Medium` *(depot faces)* | `ReadMedium` | `WriteMedium` | `AdminMedium` |
| `…Store` *(archivefs faces)* | `ReadStore` | `WriteStore` | — |
| `…Map` *(archivefs's catalog slices)* | `ReadMap` | `WriteMap` | — |
| `…er` *(archiveio bound ends)* | `Reader` | `Writer` | — |
| down-seams *(named by exchanged unit)* | `PartOpener` | `PartAllocator`, `Recorder` | — |
| per-archive handle | `io.ReadCloser` (anonymous) | `ArchiveWriter` | — |

## The two paths, vertically

```
WRITE (bytes flow down)                      READ (bytes flow up)

dumper                                       restorer / verifier
  │ Ingest.NewArchive(spec)                    │ ReadStore.Open(ref)
spool ── routed PartAllocator+Recorder         │
  │      (one orchestrator goroutine)          │
archivefs.Session ─ WriteStore               archivefs.FS ─ ReadStore
  │ implements archiveio.Recorder              │ resolves ref via ReadMap (catalog View)
  │ Record → WriteMap (catalog Window)         │ implements archiveio.PartOpener
archiveio.Writer → ArchiveWriter             archiveio.Reader → io.ReadCloser
  │ PartAllocator.NextPart                     │ PartOpener(FilePos)
librarian.Allocator (one drive)              depot.ReadMedium (mount + ReadFileAt)
  │ opened as depot.WriteMedium                │
media.Volume ── the device ──────────────── media.Volume
```

## The down-seams, unit-aligned

The block layer's seams are named by **what crosses them**, not by the
implementer's scope (the old `VolumeSink`/`WriteStore` named the librarian's
volumes and the run — but the Writer thinks in parts and archives; and no bytes
ever flow through them, so "Sink" was wrong twice — `Sink` is reserved for true
byte sinks, `xfer.Sink`):

| exchanged unit | read | write | implemented by |
|---|---|---|---|
| part | `PartOpener` — open the part at a `FilePos` | `PartAllocator` — `NextPart`/`PlaceFile`/`Bounded` | fs over `depot.ReadMedium` / `librarian.Allocator` (one drive) |
| archive commit | — *(reads don't mutate the catalog)* | `Recorder` — `Record(CommitResult)` | `archivefs.Session` (→ `WriteMap`) |
| bound end | `Reader(open, lim)` | `Writer(alloc, rec, spec, lim, now)` | — |

`PartOpener` is deliberately not `PartSource`: a Source implies a stream, but
the opener is *addressed* (open the part at this position) while the allocator
is *allocating* (where does the next part go). The asymmetric verbs encode that
reads are random-access and writes are append-ordered.

### Amendment to concurrent-writes.md

The Writer used to be built over one glued interface (`WriteStore` =
volume alloc + `Record`), with the clerk `Session` proxying the alloc calls to
the librarian. That glue had no honest name because it joined two seams with
different owners: allocation belongs to the device side (librarian/depot),
recording to the fs side (catalog). `NewWriter` now takes the two seams
separately — `PartAllocator` from the opened `WriteMedium`, `Recorder` from the
fs `Session` — and the Session's forwarding boilerplate is gone. The invariant
that mattered is untouched: on the concurrent path the spool still routes
*both* seams through its single orchestrator goroutine, which remains the sole
owner of rolls and catalog writes.

## Asymmetries that are design, not drift

1. **Admin exists only at the depot/librarian/media layers.** Label, load,
   inventory, posture never touch archive bytes — no `AdminStore`, no admin
   `…er`. Same as mkfs/mount living below the VFS.
2. **`ReadStore` is global, `WriteStore` is one opened run.** Reading is
   stateless over all committed archives; writing goes through a handle you
   opened at the depot. Inherent to any fs.
3. **`Ingest` has no read-side mirror.** It exists for admission: back-pressure,
   holding-vs-direct routing, drive leasing. Reads need no admission control —
   `ReadStore` is the whole read face. (It is also the one write-side name
   outside the prefix grid, deliberately: it is a factory, not a face.)
4. **`ArchiveWriter` is named; the read handle is anonymous.** Writing has a
   two-phase lifecycle — Commit is explicit and load-bearing (Close-as-commit
   would seal partial archives on error paths). Reading's whole protocol is
   Close, so there is deliberately no `ArchiveReader`.
5. **The librarian has no named read object.** Drive positioning state lives
   inside `Librarian`; the depot face (`ReadMedium`) is the object callers
   hold. `Allocator` exists on the write side only because multi-drive
   parallelism needs one bound object per drive.
6. **`Session` wears two hats by design.** The same object is held from above
   as `archivefs.WriteStore` (the open write handle: record, read back,
   reclaim) and called from below as `archiveio.Recorder` (the commit
   crossing). It is the point where the layers meet; the interface name at a
   call site says which hat is in play.

## What moved (and what deliberately didn't)

- `engine.depot` (private struct) → **`internal/depot`** package; the three
  faces moved from `librarian/faces.go` to the package that mints them. The
  write-claims table, lazy landing open + catalog bootstrap, limiters, and
  part-size policy all live there.
- `clerk` → **`archivefs`**; `Clerk` → `FS`. The fs contracts moved home from
  `archiveio`: `ReadStore`, `Store` → `WriteStore`, `Ingest`,
  `ErrMissingCopy`. `Deps` → `Depot` (the fs's slice of the depot).
  `archivefs.Medium` (Name+Volume) was kept as the Session's consumer-side
  slice of the opened `depot.WriteMedium` rather than dissolved — holding the
  full face would force every fs test fake to implement the whole write face
  (PrepareWrite, allocators, …) for the two methods a Session actually uses.
- `archiveio.Ref` → **`record.Ref`**: it is precisely the identity stamped in
  every part header, and `record` already owns the run-id vocabulary, so both
  the block layer (header assert) and the fs (the "filename") share it from
  below. *(Reversed 2026-07-03: `Ref` is never serialized — `Header` carries
  the fields flat — so it moved back to `archiveio` alongside `FilePos` and a
  position-only `ArchivePos`; `record` keeps only on-medium records, and the
  catalog persists its own `PlacedArchive` shape.)*
- `archiveio`: `Author` → `Writer` (the dual of `Reader`; "Author" was a name
  dodge around `ArchiveWriter`, resolved by scope instead: unqualified
  `Writer`/`Reader` are the run/medium ends, `ArchiveWriter` is one archive's
  handle). `Reader` is constructor-bound to its `PartOpener` + limiter
  (mirroring `NewWriter`); `VerifyParts` → `Verify`. `VolumeSink` →
  `PartAllocator`; `PlaceRecord` → `PlaceFile`; `WriteStore` dissolved into
  `PartAllocator` + `Recorder`.
- `librarian.WriteSink` → `librarian.Allocator`;
  `WriteMedium.WriteSink`/`LazyDriveSinks` → `Allocator`/`LazyDriveAllocators`.
- **Not moved**: `media`, `catalog`, `spool`, `xfer`, the operations — already
  on the right layer. `librarian` keeps its name and mechanism; only the faces
  left. The catalog-window semantics (write claims at open, faces as method
  sets) are untouched: this was a re-homing, not a redesign.
