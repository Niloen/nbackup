# Design note: tapes and changers (what real hardware needs)

Status: implemented — the single-drive `device:` path, the slots/drives/`Manual`
changer remodel, and the real `mtx`/`chg-robot` loader all shipped and are validated
against a SCSI media-changer emulator (`mtx -f /dev/sg0`, a 4-drive / 43-slot LTO-8
library with `/dev/nst0…3`). Multi-drive scheduling (the librarian assigning a drive
per worker) and real-hardware validation on a physical drive are the remaining work.
The current shape lives in ARCHITECTURE.md, "Tape = a changer"; this note keeps the
*why* — what real tape taught us and how the model maps onto Amanda's.

## Part 1 — What real tape requires (learned from a broken first cut)

The first `device: /dev/nstN` backend was written to the *directory emulator's*
semantics — exact file-number addressing, `reset()` = truncate, no block size, no real
erase — and a real drive shares almost none of them. On contact with a drive it failed
at every step of the lifecycle. Each failure is now fixed; recorded here as the
requirement it exposed:

1. **Positioning must be portable.** The backend shelled out to `mt eom`, which exists
   in GNU cpio's `mt` but not in `mt-st` (what every Linux box actually has, where it is
   `eod`) — `nb check` died with `unknown command "eom"`. The backend now drives the
   Linux st(4) ioctls directly instead of shelling out to `mt(1)` at all.

2. **Erase/overwrite means beginning-of-tape.** Relabel rewound and appended, but on a
   real drive `rewind` does not erase, so the new file-0 label landed *after* the old
   data and read-back returned foreign bytes. Relabel now writes the new label at BOT and
   lets the trailing filemark truncate the old data — never appends at EOD. (The dir
   emulator's `reset()` genuinely deletes every file, so EOD = file 0; real tape does
   not, which is why the emulator never modeled this.)

3. **Exactly one filemark per file.** `Commit` both closed the device (which writes a
   filemark on most drivers) *and* issued `weof`, so the file count jumped by two per
   write and later `asf N` seeks missed. The backend now writes one filemark per file.

4. **Block framing must be disciplined (correctness, not tuning).** The drive came up in
   variable-block mode (`block size 0`); writing 32 KiB chunks with no block-size
   discipline and reading with a plain `os.File.Read` made written and read framing
   disagree — `nb verify` reported `CHECKSUM MISMATCH`, i.e. the data did not survive a
   round trip.

5. **A read buffer must be ≥ the on-tape record.** A variable-block read whose buffer is
   smaller than the physical block returns `ENOMEM`; `nb recover` died and the backup was
   unrecoverable.

Root cause in one sentence: tape needs its own device discipline (block size, filemarks,
EOD, erase) that a directory emulator never has to model. Findings 4 and 5 are why block
size is a *correctness* requirement on real tape, addressed by the block-mode decision in
Part 3.

## Part 2 — Amanda's three-axis model

Amanda splits the problem into three orthogonal things, and that separation is exactly
what keeps its real-tape path correct where the first cut fused them into one and broke:

1. **Device** (the Device API: `tape:/dev/nst0`, `file:/vtape`, `s3://…`). A device
   plugin does byte I/O and positioning over one mounted volume — nothing else.
   Parameterised by properties, the load-bearing ones being `BLOCK_SIZE` /
   `READ_BLOCK_SIZE`; Amanda reads with a buffer sized to the block, so it never hits
   finding 4/5.

2. **Tapetype** (`define tapetype LTO8 { length …; blocksize …; filemark …; }`). A named
   profile of the *media* in the device: capacity, block size, filemark size (for
   capacity accounting), density.

3. **Changer** (the Changer API, `tpchanger`). Moves volumes between **slots** and
   **drives** and never touches bytes. Its drivers map one-to-one onto NBackup's shapes:
   - `chg-disk` — vtape directory changer ⇄ the file-backed `dirLoader`.
   - `chg-single` / `chg-manual` — one fixed or hand-loaded drive ⇄ the real
     single-drive `device:` backend (`Manual()`).
   - **`chg-robot`** (older `chg-zd-mtx`) — a real SCSI library via `mtx`: `mtx status`
     for drives/slots/import-export/**barcodes**, load/unload **by element address**,
     across **multiple drives** ⇄ the `mtxLoader`.

   The changer reports an inventory (per slot: state, label, **barcode**) and supports
   load/unload/eject/reset/search.

On top sit rotation concepts NBackup already mirrors: labels (`amlabel` at file 0,
`labelstr`, `autolabel`), `tapelist` (label ↔ datestamp ↔ reuse), `tapecycle` (the
minimum-tapes-in-rotation floor — NBackup's retention Floor + `minimum_age`), `tapepool`
(`pool`), `runtapes`. Modern Amanda groups device+changer+tapetype+pool into a
`storage`, essentially a NBackup `media:` entry.

The mapping to NBackup:

| Amanda | NBackup |
|---|---|
| Device API | `media.Volume` (the per-cartridge I/O seam) |
| Tapetype | inline `block_size` on the medium (see Part 3 — a full named type is deferred) |
| Changer API | `media.Changer` (`Slots`/`Drives`/`Drive(i)`/`Load`/`Unload`/`Manual`) |
| `tapepool` / `labelstr` / `tapecycle` | `pool` / label guard / Floor + `minimum_age` |
| `storage` | `media:` entry |

## Part 3 — Block mode: variable, not fixed (a deliberate divergence, verified)

The one place NBackup deliberately diverges from Amanda on the device, and the one Part
worth keeping in full because it justifies a non-obvious choice.

Amanda writes **fixed-size blocks and zero-pads the final one**, because its on-tape
format records each block's exact data length, so it reads back a known size and ignores
the padding. NBackup has no such length: verify re-hashes the payload it reads back *to
the trailing filemark*, so the stored bytes must be exactly the written bytes — padding
would change the hash. The backend therefore uses **variable-block mode** (`MTSETBLK 0`),
where each `write()` is one record of its exact length. This was researched precisely
because fixed is the tape-world default and the divergence felt risky; the conclusion is
that Amanda's choice is for simplicity/portability/throughput, not because variable is
unsafe (Amanda's own docs: variable is fine, as is fixed at 32 KB; Bacula defaults to
variable). Variable mode has two bounded hazards, and the backend guards both:

- **A record larger than the st(4) driver's buffer can fail to allocate.** The driver
  guarantees a single contiguous buffer up to 256 KiB (64-bit) / 128 KiB (32-bit). So
  `block_size` is **capped at 256 KiB** (floor 32 KiB = the header block, default 64
  KiB), and writes are chunked to ≤ `block_size` — no record can exceed it.
- **A read buffer smaller than the on-tape record returns `ENOMEM`** (finding 5). Reads
  buffer to `block_size`, and to avoid coupling read correctness to an unchanged config
  the reader also **grows the buffer and retries on ENOMEM** (up to the cap) — Amanda's
  `RESULT_SMALL_BUFFER` pattern. A tape written with a larger `block_size` than the
  reader is configured for still restores (verified: write @ 256k, read @ 64k,
  byte-identical).

Block size is an inline `block_size` on the medium (default 64k), not a named tapetype:
it is the only strictly-required field — capacity already lives on the medium — so the
full Amanda tapetype concept is deferred until a second field (filemark/density) earns
it.

### Future: a fixed-block fallback (not built)

Variable mode is the right default for modern LTO, but a quirky or ancient drive that
only supports *fixed* blocks would need fixed mode. The load-bearing requirement for
adding it: it **must not touch anything above the media seam** — `verify`/`restore`/the
catalog must keep seeing a byte-exact "read the payload until EOF" stream. That is
achievable entirely inside the tape driver by making each tape file self-describe its
exact length:

```
fixed mode, one tape file:
[ header block ][ payload blocks … last zero-padded ][ length trailer ] <filemark>
   32k, fixed          fixed blocks                    one fixed block
```

The **length trailer** is one fixed block the driver appends recording the exact payload
byte count — the device's own framing, as invisible to callers as filemarks and padding
already are. On read the driver streams forward (never backspaces — tape positioning is
where the bugs live), holds a one-block read-ahead, recognizes the trailer as the block
before the end-of-file filemark, and trims the last data block to the recorded length.
The result is byte-exact and ends at EOF, identical to variable mode and to the
disk/cloud backends. (Header/footer/index files need no special care: they are parsed
structurally, so trailing zero padding is ignored on its own; only the re-hashed archive
payload is padding-sensitive.)

Cost: a `block_mode` option (default `variable`) on the `device:` medium, `MTSETBLK
<blocksize>` instead of `0`, the writer buffering to full blocks + pad + trailer, the
reader doing the lookahead trim — the `device` interface and every caller unchanged, the
pos model unchanged (one logical file = one tape file), one wasted block per file,
~30 lines of driver logic. Selection can be explicit or a Bacula-style auto-fallback (try
`MTSETBLK 0`; if the drive refuses variable mode, use fixed) detected by the `nb check`
tape self-test. The point of recording it: variable-now is **not a lock-in** — the
fallback is additive and contained.
