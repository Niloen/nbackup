# Ranged reads — frame-tabled archives for selective restore and cheap drills

Status: **implemented and shipped** (as the FRAMED-INVISIBLE shape) — this doc is
the PoC-validated design rationale and remains the reference for the frame
mechanics, PoC results, and rejected roads. Superseded in part:
[archive-shapes.md](archive-shapes.md) generalizes this design into a
capability-driven shape model (and replaces the encryption section here with the
FRAMED-ATOMIC shape).

Reading anything from a large archive today costs the whole archive: a
single-file `nb recover` streams every byte through decode and lets tar pick
the members, and a drill (checksum or structural tier) re-reads the entire
payload. On a cloud landing that is real money — egress is billed per GiB —
and real time. This doc designs the way out: record enough metadata at write
time that a read can fetch **only the bytes it needs** via ranged GETs, without
changing what the on-medium bytes look like to any existing reader or to the
stock-tools recovery one-liner.

## Why the format resists, and the one property that unlocks it

The payload is `tar → compress → encrypt` as one continuous stream, split into
parts only *after* the transforms. Two layers stand between a byte range and a
usable file:

- **tar** is the easy layer: a member's data is self-delimiting from its
  header, and tar happily consumes a stream that *starts* at any member
  header. An offset table fully solves it.
- **The transforms** are the real obstacle. Compression is stateful — decoding
  byte X needs the decoder window accumulated since the start of the frame —
  so an offset into the compressed stream is useless on its own. No amount of
  metadata fixes this; the stream needs **decode restart points**.

The unlock: for gzip and zstd, a concatenation of independently-compressed
streams is itself a valid stream (multistream/multi-frame). So if the raw tar
stream is cut into chunks and each chunk is compressed by a **fresh child
process**, the concatenated output is byte-for-byte a normal archive — every
existing reader, `nb copy`, deep verify, and `cat parts | gzip -dc | tar x`
work unchanged — yet every chunk boundary is a restart point a ranged read can
decode from.

That property is per-scheme, not per-category, so it becomes a capability bit
in the transform registry:

> **ConcatSafe**: reversing a concatenation of independently-encoded chunks
> with the scheme's stock Reverse command yields the concatenation of their
> plaintexts.

`gzip`, `zstd`, and `none` declare it; `gpg` does not (GnuPG ≥ 2.2.8
deliberately rejects concatenated messages — the multiple-plaintexts fix;
verified on 2.2.40: it decodes the first message then hard-fails). A future
scheme declares its own bit and nothing else changes — no "encryption is never
rangeable" assumption is baked in anywhere.

## When an archive gets a frame table — the exact conditions

An archive is written frame-tabled iff **all** of:

1. **Every stage in its pipeline is ConcatSafe.** The AND over the dumptype's
   configured stages (composition of concat-safe stages is concat-safe). In
   practice today: `compress: zstd|gzip|none` with `encrypt: none` qualifies;
   any gpg encryption disqualifies. A dumptype with no transforms at all
   (plain tar) qualifies trivially — the "frames" are just raw cut points.
2. **Every stage runs server-side.** The chunker is in-process server code; a
   client-placed transform (`at: client`) has already encoded the stream
   before it reaches the server, so there is nothing left to chunk. This is
   consistent with client placement's purpose (plaintext never leaves the
   client) and costs no machinery.
3. **The archiver reported member offsets.** gnutar does (below); an archiver
   that cannot simply reports none. Frames without member offsets still serve
   the drill (frame sampling needs only frame boundaries); member-selective
   restore needs both tables.

Everything else is orthogonal, by construction:

- **Medium type does not gate writing** the table — frames are cut in the raw
  domain before the part split, so tape spanning (exact compressed-size cuts)
  is untouched and one format serves all media. Medium type gates *using* it:
  cloud and disk implement the ranged part-open; tape does not and streams as
  today.
- **Copies keep the table for free.** The table maps raw offsets to offsets in
  the **encoded payload stream**, which is exactly what `NewCopy` carries
  verbatim (re-split, never re-encoded, same SHA). So it is recorded once in
  the archive's index and is valid on every copy — the strongest sign it lives
  at the right layer. (Contrast per-part SHAs, which are per-placement.)
- **Absence means today's behavior.** No table → stream the whole archive,
  exactly the current path. Rangeability is everywhere a pure overlay gated on
  the table's presence in the index; the plain path is not a second code path,
  it is the unchanged existing one. Zero-change incrementals record no index
  at all today and stay that way.

The user-visible consequence is worth stating in docs: **server-side
unencrypted (or concat-safe-encrypted, if such a scheme ever lands) dumptypes
get cheap selective restore and cheap drills; gpg-encrypted ones read whole,
as today.** That is a genuine trade a user makes when choosing encryption,
not an implementation accident.

## Write path: a Source decorator, one widened seam

The chunker is `xfer.ChunkSource(inner Source, filters Filters, chunkSize)` —
a Source decorator that absorbs the encode filter chain. It reads the raw
stream in `chunkSize` slices (64–256 MiB), runs one `RunPipe` per chunk
through the composed filters (the child respawn per chunk is what creates the
restart points — stock CLIs have no flush control), counts bytes in (raw
domain) and out (encoded domain), and appends one `(RawOff, EncOff)` pair per
chunk. `Transfer` then runs with empty Filters and is itself **unchanged** —
`drive`, part splitting, fault reaping all see a byte stream indistinguishable
from today's.

The table travels through the channel that already exists: `finish() →
SourceStats → sink.Commit(stats)`. `SourceStats` gains `Frames []Frame`;
`ChunkSource.finish` merges the inner source's stats with its own table, which
is complete before `Commit` runs because the stream has fully drained. No new
synchronization, no side-channel between dumper and sink. The dumper's only
change is the composition branch, beside where it already places transforms:
wrap in `ChunkSource` when conditions 1–2 hold, else exactly today's
composition.

One honest cost: with the filters inside the Source, an encode-side compressor
fault reports as `RoleSource` rather than `RoleFilters`. `ChunkSource` wraps
stage errors to keep naming the failing program; decode-side classification
(what the drill taxonomy keys on) is unaffected.

## Member offsets: one tar flag, a widened Member

Adding `--block-number` (`-R`) to the existing `--index-file` invocation turns
each index line into `block N: ./path`, where N is the member **header**'s
512-byte block — so `RawOff = N × 512` is exactly where tar must start
reading. Parsing and the ×512 stay inside `archiver/gnutar`; what crosses the
archiver seam is a byte offset into the raw stream, a format-neutral concept.
List mode (`tar -tR`) emits the same numbers (plus a `** Block of NULs **`
trailer the scanner drops), so the structural verify comparison gets stronger
for free: names *and* offsets against the seal.

`Members []string` widens to `[]record.Member{Path string; Off int64}` (Off
-1 when unreported), greenfield-style — no parallel-slice zip landmine. The
ripple is mechanical: paths-only consumers (`CountFiles`, recovery browse,
the `RestoreStage` replay) take `.Path`; the verify compare improves. One
invariant becomes load-bearing and is stated on the type: **the member list is
in stream order** (tar writes the index as it archives), so member *i*'s
extent is `[Members[i].Off, Members[i+1].Off)` with no size field.

## The index record: two tables, footer untouched

The per-archive index (KindIndex) goes from a gzip'd JSON array of paths to a
gzip'd JSON document:

```json
{ "members": [ {"p": "./sub/", "o": 512}, … ],
  "frames":  [ [0, 0], [67108864, 17825792], … ] }
```

`frames` is `[rawOff, encOff]` pairs, absent for a non-rangeable pipeline. The
commit footer is untouched — `Members` stays omitempty/stripped and `Frames`
is omitted the same way — so scan/rebuild still reads only small footers and
both tables are paid for only on browse/extract, preserving the existing
index/footer split. Size is a non-issue: a 1 TiB archive at 64 MiB chunks is
~16k pairs, a few hundred KB before gzip.

## Read path: OpenRange, a range planner, frame sampling

- `archiveio.Reader.OpenRange(ref, parts, off, n)` — the encoded payload's
  bytes in `[off, off+n)`, still transformed. Maps encoded offset →
  (part, offset-in-part) by cumulative part sizes (recorded per placement,
  falling out of the per-part-SHA work). Below it, one optional capability:
  a ranged part-open on the medium — `fslike` implements it (cloud via
  `blob.NewRangeReader`, disk via seek); tape opts out by not implementing.
- **Selective restore** (restorer): selected members → raw extents (member
  table) → covering frames coalesced into contiguous encoded ranges (frame
  table) → one `OpenRange` + one decode child per range → discard
  `memberOff − frameStart` raw bytes → tar extracts from the header it lands
  on. Feeding tar exactly the wanted extent plus a synthetic end-of-archive
  marker (1 KiB of NULs) gives a clean exit 0. Falls back to the whole-stream
  path when any ingredient is missing.
- **Drill frame sampling**: a new cheap exercise — ranged-fetch one frame
  group, decode through the real pipeline, `tar -t` from the first indexed
  header in it, compare names+offsets against the index slice. That is the
  structural tier's proof (pipeline + scheme + listable stream) at one frame's
  egress. The ledger records which frames have been sampled so successive
  drills rotate through the archive. Orthogonally, per-part SHA256s (recorded
  per placement at write time, where the bytes are already metered) give the
  checksum tier the same sampling shape on any medium, encrypted or not.

## PoC — every assumption validated with stock tools, no nb code

Manual run (GNU tar 1.34, gzip 1.12, GnuPG 2.2.40; 2.4 MiB source, 57
members, 256 KiB chunks):

- `block × 512` is byte-exact at the member header; tar consumes a stream
  starting at any header.
- Arbitrary-boundary chunked gzip, concatenated: `gzip -dc | cmp` against the
  whole-stream encoding is **byte-identical**; `| tar -t` lists all members.
- Ranged single-member extraction via the two tables alone: a 45-byte file
  cost **6,371 of 1,906,554 encoded bytes (0.3%)**; a member spanning 6
  frames and the last member both extracted byte-identical; clean tar exit 0
  with the synthetic EOF marker.
- One-frame structural sample: `tar -t` of the fetched frame matched the
  index's slice for its raw range exactly.
- gpg is not ConcatSafe: concatenated messages decode the first then hard-fail
  (exit 2). Per-frame loop decode (`for f; do gpg -d $f; done | gzip -dc`)
  is byte-identical — the stock shape if per-frame encryption is ever wanted.
- Framing overhead: +0.45% at 256 KiB chunks, **+0.03% at 1 MiB** — nil at
  the design's 64–256 MiB.
- Caveats found: stock-recovery recipes must skip with `tail -c +N` (`dd skip`
  under-reads on pipes); zstd multistream is documented but was absent in the
  PoC environment — re-run the concatenation check with zstd before freezing
  its ConcatSafe bit.

## Encryption and ranged reads

No stock encryption CLI is ConcatSafe, and deliberately so: gpg decodes the
first concatenated message then hard-fails (verified, 2.2.40 — GnuPG *removed*
`--allow-multiple-messages` as the multiple-plaintexts security fix), `openssl
enc` dies on the second frame's `Salted__` header mid-stream (verified,
3.0.18), age rejects trailing data by design, aespipe's positional sector IVs
garble a re-based frame. Accepting appended ciphertext as more plaintext in
one output is a splicing attack surface — concat-friendliness is harmless for
compression and dangerous for authenticated encryption. `ConcatSafe: false`
for every real encryption scheme is the permanent answer, not a gap. Two
routes still give encrypted archives cheap reads:

- **FrameSafe (a second, weaker capability): each frame independently
  decodable as a complete message.** Every encryption CLI has it trivially.
  The chunker, table, and ranged GET are identical; the read side invokes the
  decrypt child per frame instead of per frame group (loop decode verified
  byte-identical in the PoC). Costs, honestly: the stock one-liner becomes a
  documented recipe (split the payload at the index's encOffs — the index is
  gzip'd JSON on the medium, stock-readable — and decrypt each; no nb binary
  needed, but no longer one line); and cross-frame order/completeness is
  pinned by the frame table and archive SHA (the catalog layer), not the
  crypto — each frame is individually authenticated, full restores re-check
  the whole-archive SHA, ranged reads trust the catalog as they inherently
  must. Not scheduled; the capability slot is the design's hook for it.
- **Encrypt at the medium layer instead of the pipeline.** Server-side
  encryption (SSE-S3/SSE-KMS/SSE-C, GCS/Azure equivalents; LUKS for local
  media) decrypts transparently on ranged GETs, so the pipeline stays
  compress-only and fully frame-tabled — this design works unchanged, today.
  It is a bucket/medium property, not an archive property. The trade is the
  threat model: the provider holds (SSE-C: transiently sees) the key. Decision
  tree for users: provider-external threat → SSE + compress-only, ranged
  reads for free; provider-internal threat → gpg pipeline, whole-stream reads
  (or the future FrameSafe tier).

## Rejected roads

- **Offsets without restart points** (index the existing stream, ranged-GET
  into it): dead — compression is stateful; no metadata makes a mid-frame
  offset decodable. The gzip window-checkpoint hack (`zran.c`) needs
  in-process zlib state injection, abandoning the child-process stance, and
  has no zstd equivalent.
- **zstd seekable format**: the seek table rides in skippable frames (stock
  `zstd -d` ignores them), but *producing* it needs non-stock tooling or an
  in-process library — same stance violation — and it is one scheme's private
  answer where ConcatSafe is scheme-neutral.
- **Self-contained parts** (restart transforms at part boundaries, the first
  draft of this design): works for cloud but couples restart points to part
  layout — tape needs exact compressed-size cuts, so the format would fork
  per medium, and copies (which re-split) would need table rewrites. Frames
  cut in the raw domain above the split give one format, part-layout
  independence, finer granularity, and copy-invariance.
- **Trusting store-side checksums** (S3 GetObjectAttributes, zero egress):
  verifies metadata the store recorded at upload, not that our pipeline can
  read the bytes today. Not a drill.
