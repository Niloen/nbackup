# Design note: tapes and changers (what real hardware needs)

Status: **proposed.** Written after road-testing the current tape support against a
real SCSI media-changer emulator (`mtx -f /dev/sg0`, a 4-drive / 43-slot LTO-8
library with `/dev/nst0…3` drives). The emulated **directory** backend works and is
well-tested; the **real-hardware** path (`device: /dev/nstN`) is structurally
present but, as its own code comments admit, was never exercised on a drive — and on
contact with one it fails at every step. This note records what broke, compares our
model with Amanda's, and proposes the smallest model that makes real tape libraries
work.

## Part 1 — Validation: what works and what does not

The repo ships three tape shapes behind one `type: tape` medium
([`internal/media/tape`](../../internal/media/tape)):

| Shape | Config | Backend | Status |
|---|---|---|---|
| Robotic library (vtape) | `dir:` + `bays:` | `dirChanger` over `dirDevice` | **works, tested** |
| Manual single-drive station (vtape) | `dir:` + `mode: manual` | `shelfChanger` over `dirDevice` | **works, tested** |
| Real standalone drive | `device: /dev/nstN` | `driveChanger` over `mtDevice` | **broken on hardware** |
| **Real robotic library (SCSI/mtx)** | — | — | **does not exist** |

The directory backends are faithful and pass the suite. The findings below are all on
the real-drive path and the missing real-library path.

### Finding 0 — there is no real autochanger backend at all

The emulator's entire reason for being — a robotic library you drive with `mtx`/`sg`
— **cannot be used by NBackup.** `grep` for `mtx`, `/dev/sg`, `scsi`, barcodes, or
transfer elements across the codebase finds only doc comments. The only "robotic
library" we have is the directory emulator (`dirChanger`); the only real-hardware
path is a *single* drive an operator loads by hand (`driveChanger`). A real LTO
library is unusable.

### Findings 1–5 — the single-drive `device:` path fails end to end

Reproduced against `/dev/nst0` (an LTO-8 drive fed by the emulator). Each step of a
normal lifecycle failed:

1. **`mt eom` is not portable — `nb check` fails immediately.** The drive backend
   issues `mt eom` ([`mt.go:56,68`](../../internal/media/tape/mt.go)). That subcommand
   exists in GNU cpio's `mt`, but **not** in `mt-st` (the standard Linux SCSI-tape
   tool, what every Linux box actually has), which spells it `eod`. `nb check` dies
   with `mt: unknown command "eom"`.

2. **`reset()` does not erase, so relabel writes to the wrong place.** `WriteLabel`
   calls `reset()` = `mt rewind` ([`mt.go:117`](../../internal/media/tape/mt.go)) and
   then appends the label. But `appendWriter` first seeks to **end-of-data**
   ([`mt.go:68`](../../internal/media/tape/mt.go)), so on a tape that already holds
   data the label lands *after* it, not at file 0. The directory backend's `reset()`
   genuinely deletes every file (so EOD = file 0); `rewind` on real tape does not. A
   forced relabel of a non-blank tape therefore writes a second label past the old
   data and read-back of file 0 returns the *foreign* bytes — observed as
   `label write could not be confirmed (read back "", err=unexpected EOF)`.

3. **Double filemark per file inflates the file numbering.** `Commit` closes the
   device — "closing the device writes a filemark too on most drivers" — **and then
   also** runs `mt weof 1` ([`mt.go:101–107`](../../internal/media/tape/mt.go)). Two
   filemarks per file means the file count jumps by two per write (observed: a
   freshly-labeled tape reports **2 files**, one dump reports **8**). `ReadFile` then
   seeks with `mt asf N` to positions that no longer line up with what was written.

4. **No block-size discipline → checksum mismatch.** The drive came up in
   **variable-block mode** (`Tape block size 0 bytes`). The backend writes 32 KiB
   chunks through a `bufio.Writer` but never sets a block size (`grep` for `setblk`:
   nothing) and reads with `os.File.Read`. Written and read framing disagree; `nb
   verify` reports **`CHECKSUM MISMATCH`** — the data does not survive a round trip.

5. **Reads with a buffer smaller than the physical block → restore impossible.** A
   variable-block read whose buffer is smaller than the on-tape block returns
   `ENOMEM`. `nb recover --all` dies with
   `read /dev/nst0: cannot allocate memory — the chain is broken`. **The backup is
   unrecoverable.**

**Root cause, one sentence:** the `mtDevice` backend was written to the *directory
emulator's* semantics — exact file-number addressing, `reset()` = truncate, no block
size, no real erase — and a real drive shares almost none of them. Tape needs its own
device discipline (block size, filemarks, EOD, erase) that the dir emulator never had
to model, plus there is no driver for the robot the hardware actually exposes.

## Part 2 — How Amanda models this (and why it doesn't break the same way)

Amanda splits the problem into **three orthogonal things**. This separation is exactly
what keeps its real-tape path correct where ours fused them into one and broke.

1. **Device** (the *Device API*: `tape:/dev/nst0`, `file:/vtape`, `s3://…`). A device
   plugin does **byte I/O and positioning over one mounted volume** — and nothing
   else. It is parameterised by device *properties*, the load-bearing ones being
   `BLOCK_SIZE` / `READ_BLOCK_SIZE`. Amanda reads with a buffer sized to the block, so
   it never hits our finding 4/5.

2. **Tapetype** (`define tapetype LTO8 { length 12000 gbytes; blocksize 256 kbytes;
   filemark 0 kbytes; }`). A named profile of the *media* in the device: capacity,
   block size, filemark size (for capacity accounting), density. The DLE/storage
   references a tapetype. **NBackup has no equivalent**, which is precisely why block
   size — a *correctness* requirement on real tape, not a tuning knob — has nowhere to
   live.

3. **Changer** (the *Changer API*, `tpchanger`). Moves volumes between **slots** and
   **drives**, and never touches bytes. The shipped drivers map one-to-one onto our
   shapes plus the one we lack:
   - `chg-disk` — vtape directory changer ⇄ our `dirChanger`.
   - `chg-single` — one fixed drive, no robot ⇄ our `driveChanger`.
   - `chg-manual` — prompt the operator ⇄ our `shelfChanger` / manual station.
   - **`chg-robot`** (modern; older `chg-zd-mtx`) — a real SCSI library via `mtx`:
     reads `mtx status` for drives, slots, import/export and **barcodes**, and
     loads/unloads **by element address**, across **multiple drives**. **This is the
     backend we are missing**, and it is exactly what the emulator at `/dev/sg0`
     speaks.

   The changer reports `inventory` (per slot: state, label, **barcode**) and supports
   `load slot→drive`, `unload`, `eject`, `reset`, and `search <label>`.

On top sit the rotation concepts we already mirror well: **labels** (`amlabel` writes
a header at file 0; `labelstr` constrains names; `autolabel`), the **`tapelist`**
(label ↔ datestamp ↔ reuse), **`tapecycle`** (the minimum-tapes-in-rotation safety
floor — our retention Floor + `minimum_age`), **`tapepool`** (our `pool`), and
**`runtapes`** (tapes usable per run). Modern Amanda groups device+changer+tapetype+
pool into a **`storage`** — which is essentially our `media:` entry.

**The mapping, and the gaps:**

| Amanda | NBackup today | Gap |
|---|---|---|
| Device API | `media.Volume` (the `device`/`tape` seam) | drive backend lacks block-size + filemark + erase discipline |
| Tapetype | — | **missing**; block size has no home (correctness bug) |
| Changer API | `media.Changer` / `media.Shelf` / `media.Drive` | good shape, but **no `mtx`/`chg-robot` driver**, and assumes **one drive** |
| `tapepool` / `labelstr` / `tapecycle` | `pool` / label guard / Floor + `minimum_age` | aligned |
| `storage` | `media:` entry | aligned |

Our `media.Drive`/`Changer`/`Shelf` interfaces are already the right *shape* and are
medium-neutral. The model is not wrong — it is **incomplete** (no robot driver, no
multi-drive, no tapetype) and the one real backend it does have is **undisciplined**.

## Part 3 — Proposed model

Keep the interfaces; complete them along Amanda's three axes; fix the drive backend.
Four changes, smallest first.

### 3.1 A media profile (tapetype), because block size is correctness

Introduce a small, named **tapetype** — the one new concept, and only because real
tape *requires* it. At minimum it carries `block_size`; optionally `volume_size`
(capacity, today an inline param) and `filemark` (capacity accounting).

```yaml
tapetypes:
  lto8: { block_size: 256k, volume_size: 12TB }
```

A `type: tape` medium references one (`tapetype: lto8`). The directory backend may
ignore `block_size` (a file has no blocks); the real drive **must** honour it. This
single value is what removes findings 4 and 5. It stays medium-neutral: non-tape
media never see a tapetype.

**Block mode: variable, not fixed (a deliberate divergence from Amanda — verified).**
Amanda writes **fixed-size blocks and zero-pads the final one**, because its on-tape
format records each block's exact data length, so it reads back a known size and
ignores the padding. NBackup has no such length: verify re-hashes the payload it
reads back *to the trailing filemark*, so the stored bytes must be exactly the
written bytes — padding would change the hash. The implemented backend therefore
uses **variable-block mode** (`MTSETBLK 0`), where each `write()` is one record of
its exact length. This was researched specifically because fixed is the tape-world
default and the divergence felt risky; the conclusion is that Amanda's choice is for
simplicity/portability/throughput, not because variable is unsafe (Amanda's own docs:
*"variable block mode is fine as is fixed at 32KB"*). Variable mode has two real,
bounded hazards, and the backend guards both:

- **A record larger than the st(4) driver's buffer can fail to allocate.** The driver
  guarantees a single contiguous buffer up to 256 KiB (64-bit) / 128 KiB (32-bit);
  larger blocks are stitched from chunks and *may* fail under memory fragmentation. So
  `block_size` is **capped at 256 KiB** (floor 32 KiB = the header block, default 64
  KiB), and writes are chunked to ≤ `block_size` — no record can exceed it.
- **A read buffer smaller than the on-tape record returns `ENOMEM`** (this was
  finding 5). Reads buffer to `block_size`, but to avoid coupling read correctness to
  an unchanged config, the reader also **grows the buffer and retries on ENOMEM** (up
  to the cap) — Amanda's `RESULT_SMALL_BUFFER` pattern. A tape written with a larger
  `block_size` than the reader is configured for still restores (verified: write @
  256k, read @ 64k, byte-identical).

**Fixed-block fallback (not implemented; the escape hatch for an arcane drive).**
Variable mode is the recommended default for modern LTO (Bacula defaults to it and
writes ~64 KiB records with a short final one — our exact shape; Amanda confirms it is
fine). But a quirky or ancient drive that only supports *fixed* blocks would need
fixed mode, and the load-bearing requirement is that adding it **must not touch
anything above the media seam** — `verify`/`restore`/the catalog must keep seeing a
byte-exact "read the payload until EOF" stream. That is achievable entirely inside the
tape driver by making each file **self-describe its exact length on the tape**:

```
fixed mode, one tape file:
[ header block ][ payload blocks … last zero-padded ][ length trailer ] <filemark>
   32k, fixed          fixed blocks                    one fixed block
```

The **length trailer** is one fixed block the driver appends recording the exact
payload byte count — the device's own framing, as invisible to callers as filemarks
and padding already are. On read, `dev.readFile` returns exactly `[header][payload]`
with no padding or trailer by trimming internally: it streams forward (never
backspaces — tape positioning is where the bugs live), holds a one-block read-ahead,
and recognizes the trailer as the block immediately before the end-of-file filemark
(when a read returns 0, the buffered block was the last *data* block) — it then trims
that block to the trailer's recorded length and emits it. The result is byte-exact and
ends at EOF, identical to variable mode and to the disk/cloud backends.

Note the **header/footer/index files need no special care either way**: they are
parsed structurally (the header is a fixed 32k JSON block read up to its newline; the
footer is JSON; the index is gzip), so trailing zero padding is ignored on its own.
Only the raw archive *payload*, which is re-hashed verbatim, is sensitive to padding —
and the trailer-trim makes the driver return it byte-exact regardless.

What this costs, and where it lives: a `block_mode` option (default `variable`) on the
`device:` medium; in fixed mode `MTSETBLK <blocksize>` instead of `0`, the writer
buffers to full blocks + pads + writes the trailer, the reader does the lookahead
trim. **The `device` interface, `tape.go`, `ReadFile`, and every caller stay
unchanged**; the pos model is unchanged (one logical file is still one tape file). One
wasted block per file; ~30 lines of driver logic. Selection can be explicit or a
Bacula-style auto-fallback (try `MTSETBLK 0`; if the drive refuses variable mode, use
fixed) detected by the `nb check` tape self-test. The point of recording it here: it
proves variable-now is **not a lock-in** — the fallback is additive and contained.

### 3.2 An `mtx` changer driver (the missing `chg-robot`)

Add `mtxChanger` implementing `media.Changer` over `mtx -f <sg-device>`:

- `Bays()` parses `mtx status` Storage Elements → `VolumeStatus{ID, Label}` where the
  **VolumeTag is a true barcode**, surfaced *without mounting* (today's dir backend
  fakes a barcode by reading the label; a real library hands it to us for free, which
  is how a library picks a tape cheaply and only label-verifies after the move).
- `Mount(bay)` issues `mtx load <element> <drive>`; the drive's bytes are then the
  `mtDevice` at the configured `/dev/nstN`.
- changer state comes from `mtx status`, **not** the `.loaded` marker file the dir
  backend persists (real state lives in the robot).

This is a thin process wrapper, exactly like `mt.go` — and exactly how Amanda's
`chg-robot` shells out to `mtx`. Import/export slots and `mtx unload` (return to a
home slot) round it out.

### 3.3 Multiple drives — a library has N

The emulator has **4** drives; `media.Changer` assumes one (`Loaded()` → "the volume
in *the* drive"). A real library's value is parallelism: N drives = N concurrent tape
writers (Amanda's `runtapes`/multi-drive). Generalise the changer to a set of drives:
`Drives() []DriveStatus`, `Mount(bay, drive)`, and let the librarian place one writer
per free drive. This is the larger change and can land *after* 3.1/3.2 (a one-drive
`mtxChanger` is already useful); the interface should anticipate it so we don't wedge
in a single-drive assumption again.

### 3.4 Drive-backend correctness (fix the five findings)

Independently of the robot, `mtDevice` must speak real tape:

- **Portability:** use `eod`, not `eom` (mt-st is the common case; GNU `mt` accepts
  `eod` too). *(finding 1)*
- **One filemark per file:** stop relying on "close writes a filemark" *and* issuing
  `weof` — pick one, so file numbers match `asf N`. *(finding 3)*
- **Block size:** set a fixed block (`MTSETBLK`) from the tapetype, or read variable
  blocks with a buffer ≥ block size. *(findings 4, 5)*
- **Erase/overwrite means BOT:** relabel must write the new file-0 label at
  beginning-of-tape and let the trailing filemarks truncate the old data — never
  append at EOD. Position the label write to BOT explicitly rather than assuming
  `reset()` emptied the volume. *(finding 2)*
- **Test seam:** none of this is covered by tests because it needs a drive. The
  emulator (`/dev/sg0` + `/dev/nstN`) makes a hardware integration test *possible*;
  add one behind a build tag / env guard so the real path stops being unverified.

### 3.5 Config shape

The current `type: tape` with `dir`/`device`/`mode`/`bays`/`reels` fuses device,
changer, and profile into ad-hoc keys — the fusion that produced the bugs. Make the
three axes explicit while keeping the common cases short:

```yaml
tapetypes:
  lto8: { block_size: 256k, volume_size: 12TB }

media:
  # Real SCSI library (the emulator): robot + one-or-more drives + media profile
  lto:
    type: tape
    changer: { driver: mtx, control: /dev/sg0 }
    drives:  [/dev/nst0, /dev/nst1, /dev/nst2, /dev/nst3]
    tapetype: lto8
    pool: offsite
    minimum_age: 180d

  # File-backed library (today's default; unchanged in spirit)
  vtl:
    type: tape
    changer: { driver: dir, dir: /var/lib/nbackup/vtape, slots: 20 }
    tapetype: lto8

  # Single drive loaded by hand
  desk:
    type: tape
    changer: { driver: manual }     # operator prompts; one drive
    drives:  [/dev/nst0]
    tapetype: lto8
```

`driver:` names the changer backend (`mtx` / `dir` / `manual`), mirroring Amanda's
`tpchanger`. This keeps the medium-neutral rule intact — `bays`/`drives`/`control`
live under the tape medium's `changer:`, never in the generic layer.

## Scope and sequencing

1. **tapetype + drive-backend fixes (3.1, 3.4). — DONE (2026-06-30).** The single-drive
   `device:` path now works end to end on real hardware (validated against the mtx/sg
   LTO-8 emulator: label → dump → verify → recover → relabel, byte-exact). The backend
   (`internal/media/tape/mt_linux.go`) drives the Linux st(4) ioctls directly instead of
   shelling out to `mt(1)`, uses variable-block mode for byte-exact storage, writes one
   filemark per file, and truncates from BOT on relabel. Block size is an inline
   `block_size` on the medium (default 64k) — the full named-tapetype concept (3.1) is
   deferred until a second field (filemark/density) earns it. Non-Linux is a clean stub.
2. **Changer remodel — slots + drives + the `Manual` capability. — DONE (2026-06-30).**
   `media.Changer` is now `Slots`/`Drives`/`Drive(i)`/`Load(slot,drive)`/`Unload(drive)`/
   `Manual()` over real SCSI vocabulary; `media.Shelf`/`Bays`/`Mount` retired. The tape
   package collapses to a `loader` behind one `tapeChanger`: a file-backed `dirLoader`
   (`slots: N` cartridges, `drives: K`, optional `manual: true`, simulated per-slot
   barcodes) and the real `realDriveLoader` (one drive, `ErrManualLoad`). The librarian's
   internals run over slots/drives with its public API unchanged; disk/cloud are wrapped
   in a librarian-internal `directChanger` so it has one shape. `nb medium` is
   barcode-first (drives + slots); `nb load <slot>` / `--label`. The interface **admits N
   drives** but the librarian still schedules drive 0 only.
3. **Real `mtx` changer loader (3.2).** A `mtxLoader` driving `mtx -f /dev/sg0` (the
   `chg-robot` equivalent), surfacing real barcodes; slots into the slots/drives model
   already in place. Not started.
4. **Multi-drive scheduling (3.3).** Parallel tape writers — the librarian assigns a
   drive per worker. The interface is ready; the engine/librarian scheduling is not.

Greenfield, no migration shims (per project convention): the `bays`/`reels`/`mode` keys
were replaced by `slots`/`drives`/`manual` outright.

## Open questions

- **One tapetype concept, or fold `block_size` into the medium?** A named tapetype is
  Amanda-faithful and shares across media; a single inline `block_size:` is smaller.
  Block size is the only strictly-required field — capacity already lives on the
  medium. Leaning inline unless a second field (filemark/density) earns the type.
- **Multi-drive in the `media.Changer` interface now, or after?** A single-drive
  `mtxChanger` is useful immediately, but the interface should not re-bake the
  one-drive assumption. Propose: add `drive` params to `Mount`/`Loaded` up front,
  implement N-drive scheduling later.
- **Barcode vs label as the selection key.** Real `mtx` gives barcodes pre-mount;
  should the librarian select by barcode (cheap, no mount) and only label-verify after
  the move (Amanda's model), tightening today's "read label after mount" loop?
