# Archive shapes — capability-driven framing for ranged reads and encrypted atoms

Status: designed, not implemented. Generalizes and supersedes the encryption
section of [ranged-reads.md](ranged-reads.md); the frame/ConcatSafe machinery
designed there is the FRAMED-INVISIBLE shape here. Prerequisite work already on
main: per-part seals, the drill `sample` tier, and inline seal verification on
every read.

The goal: cheap partial reads (selective restore, drill sampling) should fall
out of what each component *is* — the archiver, the compressor, the encryption
scheme, the medium — with one decision point and no per-case branching. A user
adds a scheme or a medium; the system adjusts the archive's on-medium shape
automatically.

## Capabilities: declared, never negotiated

```
transform.Scheme   Concat: Full | PerFrame | None
                   Full:     concatenated frames decode as ONE stream with a
                             single stock Reverse invocation (gzip, zstd, none)
                   PerFrame: each frame decodes with one Reverse invocation
                             per frame (gpg, age, openssl — every real
                             encryption CLI; GnuPG ≥2.2.8 deliberately rejects
                             concatenated messages, see ranged-reads.md)
                   None:     whole-stream only (hypothetical)

archiver           BackupResult.Members []Member{Path, Off}; Off = -1 when the
                   archiver cannot report member offsets (gnutar can: tar -R,
                   Off = block × 512, byte-exact — PoC-verified). Offsets gate
                   only member-SELECTIVE restore, never the shape.
                   SpliceTrailer() []byte: declares the STRONGER promise selective
                   restore actually rides on — member extents are self-contained
                   and a stream assembled from them, terminated by these bytes
                   (tar: two 512-byte zero blocks), restores/lists correctly. An
                   offset-reporting but non-spliceable format (zip-style central
                   directory, solid compression) returns nil and reads whole
                   streams only.

media (exists)     PartSizePolicy{Default, Max} + Bounded() — placement
                   geometry and hard ceilings only. Media never influence shape.
```

One pure resolver, living beside the transform-placement logic in
`dumper/encode.go`, folds the configured stages:

```
resolveShape(stages, placement):
    any stage client-placed  or  any stage Concat=None   →  STREAM
    all stages Concat=Full                               →  FRAMED-INVISIBLE
    all stages ≥ PerFrame (≥1 not Full)                  →  FRAMED-ATOMIC
```

The shape is stamped into the archive's header/footer (`shape` field) —
recorded, not re-derived, so a reader never needs config to decode. A future
scheme declares one enum value at its `register()` call and every dumptype
using it lands in the right shape; nothing else branches on scheme names.

## Two cutting planes

Everything composes from keeping two cuts distinct:

- **Cut A — decode restarts** (transform domain): where a reader can start
  decoding. An *archive* property, set at dump, invariant across copies (the
  encoded byte stream is carried verbatim; `NewCopy` never re-encodes).
- **Cut B — placement split** (medium domain): where part files begin and end.
  A *placement* property, re-decided by every copy.

Encryption is the one capability that welds B to A: a gpg message cannot be
re-cut without the key, so its decode boundary must BE the placement boundary.

## The three shapes

```
STREAM             tar→zstd→gpg as one stream; parts are opaque slices.
                   Cut A: none. Cut B: cloud slices at the medium's part_size,
                   tape capacity-exact fill, disk one file. Reads: whole only.
                   Today's format, kept for client-placed transforms and
                   Concat=None schemes.

FRAMED-INVISIBLE   zstd restarts every frame_size (256 MiB raw): the
(all Concat=Full)  concatenation is a VALID single stream, byte-identical to
                   STREAM for every reader and for the stock one-liner. Cut B
                   unchanged from STREAM — part cuts fall mid-frame freely and
                   differ per placement. A frame table (rawOff→encOff) plus
                   member offsets in the index enable ranged reads (cloud
                   ranged GETs); the table is an optimization, never
                   decode-critical. PoC-validated end-to-end (ranged-reads.md):
                   0.3% egress for a single-file extract, +0.03% ratio cost.

FRAMED-ATOMIC      Inner cut: zstd frames every frame_size as above. Outer cut:
(FrameSafe         whole zstd frames are packed up to the ATOM size and each
 pipeline)         bundle is one complete gpg message = ONE PART, on every
                   medium. Parts are indivisible atoms: copies map them 1:1
                   and never re-split (re-cutting needs the key). Object count
                   equals today's (atoms are cut in the compressed domain at
                   part_size, the same place today's splitter cuts). Reads:
                   per-atom decrypt; member→atom via the inner frame table.
```

Stock recovery per shape — the promise each keeps:

```
STREAM / INVISIBLE   cat parts | gpg -d | zstd -d | tar x     (unchanged)
ATOMIC               for p in …-L0.p*.tar.zst.gpg; do gpg -d "$p"; done \
                       | zstd -d | tar x
                     — boundaries are FILE boundaries (objects on cloud,
                     filemark-delimited files on tape); no index needed. The
                     gpg loop yields concatenated zstd frames, which stock
                     zstd reads as one stream: the capabilities compose.
```

## What each shape buys

| pipeline | shape | stock restore | cheap reads |
|---|---|---|---|
| zstd only (offsets) | invisible | unchanged one-liner | ranged selective restore, structural frame sample |
| zstd only (no offsets) | invisible | unchanged one-liner | frame sample only |
| zstd + gpg (server) | atomic | file loop | selective = fetch covering atoms; **key-proving** drill sample (decrypt one atom) |
| anything client-placed | stream | unchanged | seal sampling only (shipped) |

Per-part seals + inline seal verification (shipped) are shape-agnostic and, on
atomic archives, become per-message authentication of order, count, and
content — the composition guarantee OpenPGP itself deliberately refuses to
provide for concatenated messages (see ranged-reads.md, encryption section).

## Sizing: two scopes of one knob, one rule

> **An atomic archive brings its own part size; a sliced archive takes the
> medium's.**

```yaml
part_size: 10GiB            # global default for both scopes

dumptypes:
  vm-images:
    encrypt: gpg
    part_size: 2GiB         # ATOM size (cut A) — stamped into the archive,
                            # rides with it to every medium. Amanda precedent:
                            # tape_splitsize was a dumptype option.

media:
  offsite:
    type: cloud
    part_size: 5GiB         # SLICE size (cut B) — this placement's cut of
                            # stream/invisible archives, re-cut on every copy
```

The two scopes are consulted by disjoint shapes, so no archive ever answers to
both. The dumptype knob is the selective-restore tuning lever: smaller atoms →
finer encrypted restore granularity and cheaper key-proving samples, more
objects. The medium knob keeps its exact current meaning for slices. Guardrails:

- **Inert-knob warning:** `part_size` on a dumptype with no per-frame stage
  does nothing — warn at validation.
- **Ceilings, not preferences:** dumptype atom sizes validate against media
  *ceilings* (`PartSizePolicy.Max`), never against media `part_size`
  preferences. A 10 GiB atom on a medium preferring 5 GiB slices is intended.
- **Per-archiver: rejected.** The archiver emits a raw stream and knows
  nothing of placement (archiver-neutrality); no use case connects the
  producing tool to the placement unit.
- `frame_size` (256 MiB default) is an advanced internal knob; frames never
  exist as files, so they are deliberately outside the "part" vocabulary.

## Tape posture

- The addressable tape unit is the **file** (one filemark per file); atoms map
  to files, so seek-to-atom is `MTFSF`/locate — the same access model as a
  cloud object through a different door. Within-file locate exists on modern
  drives but buys little: tape has no egress meter, and skipping 10 GiB at
  LTO speed is under a minute.
- **Atoms are fixed-size on tape too** — never "fill the reel": an unsized
  atom could be terabytes and would be unsyncable to any bounded medium
  without the key. Fixed parts are also the Amanda model (tape_splitsize).
- **No split buffer needed:** atoms are cut in the compressed domain, so the
  writer knows each atom's bound before placing it — EOT is handled
  proactively ("remaining < bound → roll first"), wasting ≤ one atom per reel
  tail (≤0.1% at 10 GiB on LTO-8). Amanda's split_diskbuffer/EOT-rewrite
  machinery is unnecessary.
- Slices on tape keep today's capacity-exact zero-waste fill.

## The atom rigidity trade — ceilings, validation, old runs

A sealed atom cannot shrink without the key: with per-part encryption you may
pick any two of {sealed atoms, key-free sync, target-chosen part sizes}; this
design picks the first two. Consequence: an archive with 10 GiB atoms can never
land on a medium whose ceiling is below 10 GiB. Handling:

- **Config/plan time: warning**, naming the dumptype × medium pairs that can
  never meet. Adding a low-ceiling medium must not brick the config.
- **Dump time: hard error** only when such a dumptype is actually routed to
  such a landing.
- **Sync time: per-archive refusal** with a precise message; everything that
  fits is carried. Slice-shaped archives are never affected.
- **Self-healing:** atom size is per archive, so new dumps adopt a lowered
  part_size immediately and retention ages the oversized atoms out. The gap is
  transient by construction.
- `nb repack` (decrypt → re-encrypt with new atoms; a copy allowed to hold the
  key) is the named escape hatch, deliberately NOT built — rare-event tooling
  with real key-handling weight, usually mooted by retention.

In practice the collision needs an unusual medium: real ceilings are ~40 GiB
(cloud multipart) or absent (disk, tape), 4× above the default atom size.

## Filename grammar — the suffix position IS the shape

Today `.pNNN` after the extension deliberately marks "a slice of a whole, not
a valid file of that type" (fslike). Atoms move the part index BEFORE the
extensions, making the name honest and the shape operator-visible at a bare
bucket:

```
slices  (not decodable alone — suffix after extension, as today):
  000000-app01-home-L0.tar.zst.gpg.p000
atoms   (each file IS a valid .gpg — extension last, tools recognize it):
  000000-app01-home-L0.p000.tar.zst.gpg
```

`ls | sort` orders both; `.hdr` sidecars are untouched; single-part archives
keep their plain unsuffixed name.

## Seams — where each piece lands

```
DECLARE   transform/registry.go   Scheme gains `Concat` enum (+1 field per register())
          archiver/archiver.go    Members []string → []Member{Path, Off}
                                  (the widest mechanical ripple; stream-order
                                  invariant becomes load-bearing: extent =
                                  next member's Off)
          media                   nothing — PartSizePolicy/Bounded/placement
                                  verbs already say it all

DECIDE    dumper/encode.go        resolveShape() beside transform placement;
                                  shape + atom size stamped into ArchiveSpec →
                                  header/footer

WRITE     xfer                    ChunkSource (Source decorator absorbing the
                                  filters, one child per chunk — the restart
                                  mechanism; see ranged-reads.md) + a framed
                                  drive mode: Transfer cuts parts at FRAME
                                  boundaries for atomic archives instead of at
                                  the sink's cap. The only real xfer surgery.
          archiveio.Writer        atomic mode places each atom as a whole unit
                                  (the PlaceFile-style verb that already
                                  exists); streaming/invisible keep NextPart

RECORD    record                  footer: `shape`; PartSeals gain RawSize for
                                  atomic archives — cumulative raw sizes ARE
                                  their frame table (no separate table);
                                  invisible shape records `frames` in the index

READ      restorer.planDecode     footer shape → one Reverse child (stream/
                                  invisible) vs per-atom decode loop (atomic)
          archivefs               openRef unchanged. Atomic selective restore
                                  needs NO ranged seam — "fetch covering
                                  frames" = open those parts. OpenRange (ranged
                                  part-open on fslike media) is needed only for
                                  the invisible shape on cloud.
          engine drill            sample tier, shape-aware: atomic + key →
                                  decrypt-and-list one atom (structural proof
                                  at one atom's egress). NOTE: sample egress is
                                  part-granular, so the dumptype part_size knob
                                  also tunes drill cost.

COPY      archivefs/spool         NewCopy: atomic → part-per-part atom
                                  carriage; others re-split as today. Footer
                                  shape selects the mode. The holding disk
                                  stages framed archives in final shape (atoms
                                  as files) by construction — the drain is
                                  key-free and can never re-frame.
```

Untouched: Transfer's fault taxonomy, catalog windows, retention/prune, the
seals and verify machinery, tape label/changer.

## Hard spots, named

- **Member type widening** touches every consumer of `Members` — mechanical
  but broad.
- **ChunkSource + the framed drive mode** put new control flow in the write
  path, the one place bugs are unforgivable: a subtle framing bug writes
  archives that fail years later. Highest testing bar in the plan.
- **Two shapes forever** on the read side; mitigated by absence-means-today
  (stream is not a second code path, it is the unchanged one) and by the
  footer's explicit shape field.

## Sequencing

1. Member offsets (tar -R, Member widening) — smallest standalone step, also
   strengthens structural verify (names AND offsets against the seal).
2. FRAMED-INVISIBLE: ChunkSource + frame table + OpenRange + selective restore
   + structural frame sampling (unencrypted cloud gets ranged reads).
3. FRAMED-ATOMIC: framed drive mode, atom placement, copy atom-carriage,
   naming flip, key-proving drill sample (encrypted archives catch up).

Each step ships value alone; none is speculative once the previous is in use.
