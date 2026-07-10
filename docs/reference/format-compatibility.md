---
title: Format compatibility
layout: default
parent: Reference
nav_order: 4
description: "The on-medium format promise: payloads stay stock-tool-readable forever, metadata only ever grows, and every new release reads every artifact an older release wrote."
---

# Format compatibility

NBackup's pitch is that its artifacts stay readable for decades. That is a
promise about formats, so here is the promise, stated as policy. It has three
parts, ordered by how much they matter when everything else is gone.

## 1. The payload format is frozen

An archive's payload is a **GNU tar stream**, passed through a **stock
compressor** (`zstd` or `gzip`, or neither) and optionally **stock `gpg`**.
Nothing NBackup-specific is ever inserted into the payload bytes:

- A *stream* or *framed* archive's parts concatenate back into exactly that
  pipe's output. (Framing only constrains where the compressor restarted; the
  bytes remain a valid ordinary stream.)
- An *atomic* (encrypted, part-split) archive's parts are each one complete
  `gpg` message over such a stream.

This will not change. The [restore-by-hand](../restore-by-hand) procedure —
one pipe of stock tools, no NBackup binary, config, or catalog — is guaranteed
to work for every archive any NBackup release has written or will write. If a
future archiver or medium ever needed payload bytes stock tools cannot decode,
it would be a new, separately documented artifact kind — never a reinterpretation
of existing ones.

## 2. Metadata only ever grows

The NBackup-specific metadata on a medium — each archive's **commit footer**
and **per-archive index** — is plain, human-readable JSON. It evolves under an
additive-only rule:

- **New fields are optional** and carry a defined fallback, so every artifact
  written before the field existed keeps working unchanged (for example,
  archives that predate recorded member offsets simply don't offer ranged
  reads).
- **Existing keys are never renamed, repurposed, or removed.** A field's wire
  key outlives even a concept rename in the code.
- **Readers ignore unknown fields**, so a newer artifact never makes an older
  reader error out on parse.

The practical guarantee: **every NBackup release reads every artifact written
by any release since 1.0.** Upgrading is a binary swap; there are no format
migrations, and there is nothing to convert.

The one direction *not* promised is downgrade: an older binary parses a newer
artifact's footer, but a feature the artifact actually uses (a new shape, a new
archiver type) may need the release that introduced it — or, always, the
by-hand pipe above.

## 3. Everything else is rebuildable working state

Nothing outside the medium needs a compatibility story, because nothing outside
the medium is needed to restore:

- **The catalog is a cache.** `nb rebuild` reconstructs it by scanning the
  media; deleting it costs run-log history and usage statistics, never
  restorability.
- **An archiver's incremental state** (e.g. GNU tar's `.snar` snapshots) is
  working state for planning the *next* dump. Losing it forces one full backup
  (`nb reset`), and costs nothing about restoring existing archives.
- **Configuration** may gain keys between releases; existing configs keep
  working. Restore needs no config at all beyond your `gpg` key.
