# Archive shapes — implementation plan

Status: plan for [archive-shapes.md](archive-shapes.md). Each phase is a
self-contained agent-session deliverable: read this file + the docs below,
implement one phase, verify, land via the worktree-main flow.

**Read first, in order:** ARCHITECTURE.md (vocabulary + conventions),
[archive-shapes.md](archive-shapes.md) (the design being implemented),
[ranged-reads.md](ranged-reads.md) (frame mechanics, PoC evidence, rejected
roads — do not re-litigate these). CLAUDE.md rules apply throughout; note the
test env has no `zstd` (use scheme `none`/`gzip` in tests) and every phase ends
with `gofmt -l`, `go vet ./...`, `go test -race ./...` green **plus a real-CLI
road-test** of the phase's user-visible behavior (build `nb`, drive it against
a disk + cloud `file://` fixture — see the sample-tier test in
`internal/engine/drill_test.go` for the fixture shape).

**Phase 0 — DONE, on main** (do not redo): per-part seals
(`record.PartSeal`, footer `part_seals`, `PlacedArchive.Seals`, rebuild
restores them), drill `sample` tier with ledger rotation, inline seal
verification on every read (`archiveio.ErrSealMismatch` → ClassIntegrity).

---

## Phase 1 — Member offsets

Goal: the member index carries each member's byte offset in the raw archive
stream. Standalone value: structural verify compares offsets as well as names.
Everything later (selective restore, mount) builds on this.

- [x] `archiver/gnutar`: add `--block-number` to the backup index invocation
      (`createArgs`) and to `List` (`tar -tR`). Parse `block N: path` lines
      (new sibling of `scanMembers`; drop the `** Block of NULs **` trailer in
      list mode). `RawOff = N × 512` — the ×512 stays inside gnutar.
- [x] Widen `Members []string` → `[]record.Member{Path string; Off int64}`
      (Off −1 when unreported). Ripple is mechanical: `archiver.BackupResult`,
      `xfer.SourceStats`, `record.Archive`, `CountFiles`, recovery tree
      loaders, `RestoreStage` replay (paths only at point of use). Document on
      the type: **the list is in stream order; member i's extent is
      `[Off_i, Off_{i+1})`** — this invariant becomes load-bearing.
- [x] Index encoding (`record/index.go`): gzip JSON array of strings → gzip
      JSON document `{"members":[{"p":…,"o":…},…]}` with room for a `frames`
      key (phase 2). Update golden/format tests. Footer stays untouched
      (Members remain omitempty/stripped).
- [x] Structural verify (`engine/verify.go` membersDiff + drill structural
      tier): compare `{Path, Off}` pairs against the seal — a stronger check,
      free.
- [x] Zero-change incrementals record no index — unchanged; keep it that way.

Acceptance: existing suites green; new tests for offset parsing (create and
list mode, incl. a listed-incremental archive — dumpdir payloads sit between
members, PoC-verified byte-exact), index round-trip, offset-aware structural
verify catching a moved member. Road-test: dump → `nb verify --deep` green;
corrupt vs healthy classification unchanged.

## Phase 2 — FRAMED-INVISIBLE (unencrypted ranged reads)

Goal: ConcatSafe pipelines write invisible decode restarts; cloud selective
restore fetches only covering frames; drill gains structural frame sampling.
On-medium bytes stay byte-identical to today for every existing reader and the
stock one-liner.

- [ ] `transform.Scheme` gains `Concat` enum (`Full | PerFrame | None`),
      declared at each `register()`: zstd/gzip/none = Full, gpg = PerFrame.
- [ ] `resolveShape()` in `dumper/encode.go` beside transform placement:
      STREAM vs FRAMED-INVISIBLE only (ATOMIC is phase 3 — resolver returns
      STREAM for PerFrame pipelines until then). Conditions per the design:
      all stages Full AND server-placed. Stamp `shape` into
      ArchiveSpec → `record.Header`/footer.
- [ ] `xfer.ChunkSource(inner Source, filters Filters, frameSize)`: Source
      decorator absorbing the encode filters; one `RunPipe` per chunk (child
      respawn = the restart mechanism); counts raw/encoded offsets; merges
      `Frames []Frame` into `SourceStats` at finish. Wrap stage errors so the
      failing program is still named (encode faults now surface as RoleSource).
      `frame_size` advanced knob, default 256 MiB.
- [ ] Records: footer `shape`; index document gains
      `"frames": [[rawOff, encOff],…]` (absent = stream-as-today).
- [ ] `archiveio.Reader.OpenRange(ref, parts, off, n)`: encoded-offset →
      (part, offset) via cumulative part sizes — **per-part sizes already
      exist in `PlacedArchive.Seals`**. Below it, an optional ranged
      part-open capability on the medium: `fslike` implements (cloud
      `blob.NewRangeReader`, disk seek); tape doesn't and streams.
- [ ] Restorer: range planner in `ExtractSelection` — selected members → raw
      extents → covering frames → coalesced encoded ranges → OpenRange + one
      decode child per range → discard to member offset → tar. Feed exactly
      the wanted extent + 1 KiB NUL end-of-archive marker for clean tar exit
      (PoC-proven). Fall back to today's whole-stream path when any ingredient
      is missing (no table, no offsets, no ranged medium).
- [ ] Drill: structural frame sample — ranged-fetch one frame group, decode
      through the real pipeline, `tar -t` from the first indexed header in it,
      compare names+offsets against the index slice; ledger-rotated like the
      checksum sample.
- [ ] Before freezing zstd's `Full` bit: re-run the multistream concat check
      on a machine WITH zstd (`cat f0.zst f1.zst | zstd -d` byte-identical);
      the PoC only proved gzip.

Acceptance: byte-identity test (framed vs whole-stream encode of the same
input decodes identically via the stock tool); selective restore of one file
from a multi-frame cloud archive reads a small fraction of the object (assert
via a counting mounter/opener); whole-stream reads and copies of framed
archives unchanged; ChunkSource fault-path tests (child dies mid-chunk → no
committed archive). Road-test: real `nb recover --path` of one file from a
`file://` cloud fixture; `nb drill --tier` structural sample.

## Phase 3 — FRAMED-ATOMIC (encrypted archives catch up)

Goal: FrameSafe pipelines produce sealed atoms — part == complete gpg message
— with honest filenames, atom-carrying copies, and the sizing knobs.

- [ ] Resolver: all stages ≥ PerFrame (≥1 not Full) → ATOMIC. Atom size =
      dumptype `part_size`, else global `part_size` (new top-level knob,
      default 10 GiB). Atoms cut in the **compressed domain**: inner Full
      stages frame at `frame_size`; whole inner frames pack up to the atom
      bound, then the PerFrame stage seals each bundle.
- [ ] `xfer` framed drive mode: for atomic archives Transfer cuts parts at
      frame boundaries, not at the sink's cap (per-frame readers from the
      framed source; one part per atom). The only real Transfer surgery —
      highest testing bar in the plan.
- [ ] `archiveio.Writer` atom placement: place each atom as a whole unit
      (PlaceFile-style verb with archive part headers); tape rolls proactively
      when `remaining < atom bound` (no split buffer). `record.PartSeal` gains
      `RawSize` — cumulative raw sizes are the atomic shape's frame table (no
      separate index table).
- [ ] Naming (`fslike`): atoms put `.pNNN` BEFORE the extensions
      (`…-L0.p000.tar.zst.gpg` — a valid file); slices keep `.pNNN` after
      (today's "not a real file" marker). Keyed off the recorded shape.
- [ ] Copies: `NewCopy` atom mode — parts carried 1:1, never re-split; footer
      shape selects the mode. Holding disk stages atomic archives as atoms
      (final shape), since the drain is key-free.
- [ ] Knobs + validation ladder: dumptype `part_size` (atom size; inert-knob
      warning when the dumptype has no PerFrame stage); media `part_size`
      keeps today's slice meaning; atom sizes validate against media
      *ceilings* (`PartSizePolicy.Max`) — config-time warning naming
      dumptype × medium pairs, dump-time hard error when routed there,
      sync-time per-archive refusal with remedy. `nb repack` is named in docs
      as deliberately not built.
- [ ] Read side: `restorer.planDecode` per footer shape — per-atom decode loop
      (one Reverse child per atom) vs single child. Selective restore on
      atomic = open the covering parts (no OpenRange needed). Stock drill tier
      learns the file-loop recipe for atomic archives; README/verification.md
      stock-recovery sections updated with both recipes.
- [ ] Drill: key-proving sample — decrypt-and-list ONE atom (structural proof
      at one atom's egress), the encrypted sibling of the checksum sample.

Acceptance: atomic e2e with gpg where available, `none`-as-PerFrame test
double otherwise (register a test scheme with Concat=PerFrame so CI needs no
gpg); stock loop restore of an atomic archive byte-identical; copy
cloud→disk→cloud preserves atoms bit-exact (same seals); tape roll-by-bound
(memVolume capacity test); ceiling-validation ladder tests; object count for
an encrypted archive ≈ today's at equal part_size. Road-test: real gpg
dumptype on `file://` cloud — dump, sync, single-file recover fetching ~one
atom, `for p in *.p*.gpg; do gpg -d; done | … | tar x` by hand.

## Phase 4 — Consumers: `nb serve`, then mount (design-first)

Not build-ready — needs its own short design pass first; listed so the target
stays visible.

- [ ] Member stat fields (size/mode/mtime via the `-Rv` long listing) for
      `stat` without media reads.
- [ ] `nb serve`: read-only WebDAV/NFS over `recovery.Tree` + the ranged read
      path, frame-granular local LRU cache — mountable by every OS, no FUSE
      platform pain. FUSE proper as follow-on.
- [ ] Latency target: cold file ≈ one frame fetch+decode (1–3 s), warm
      instant. Encrypted mounts document the atom-size trade.

---

## Guardrails for every phase

- Shape/frames are **overlays**: absence of a table/shape field means exactly
  today's behavior, and the stream path is the unchanged existing code, never
  a rewritten twin.
- Nothing anywhere branches on scheme or medium names — only on declared
  capabilities and the recorded shape.
- The write path is the highest-stakes code in the repo: a framing bug writes
  archives that fail years later. ChunkSource and the framed drive mode need
  fault-injection tests (child death, mid-chunk cancel, EOT during atom) in
  addition to happy paths.
- Amanda-faithful checkpoints: fixed atoms = tape_splitsize; dumptype-scoped
  part_size = Amanda precedent; no split buffer needed (roll-by-bound).
- Do not reintroduce roads rejected in ranged-reads.md (zstd seekable format,
  offsets-without-restarts, self-contained parts coupled to media, store-side
  checksums as verification).
