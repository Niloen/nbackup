---
description: Review one internal package for understandability — examine its abstractions (naming, file structure, sub-abstractions, what doesn't belong) and propose changes so the reader gets a just-right chunk of code and concept at a time.
---

You are doing an **understandability review** of a single NBackup package — NOT a
bug hunt and NOT a security review. The goal: a reader meeting this package cold
should get the concepts in **just-right chunks** — each file/type/function a
digestible unit, named for what it is, with nothing foreign mixed in.

Work through the package's **abstractions**:

- **Naming** — does each type/function/file name say what it is, and match the
  ARCHITECTURE.md vocabulary? Flag names that mislead or drift.
- **File structure** — is the split into files legible? Does each file hold one
  coherent concept, or are concepts smeared across files / crammed into one?
  Propose splits and merges.
- **Things that don't belong** — code that lives here but conceptually belongs in
  another package (or vice versa). Flag it.
- **Sub-abstractions** — an inner type/helper doing enough distinct work to
  deserve its own name (and maybe its own file); or, conversely, an abstraction
  that earns nothing and should be inlined away.
- **Chunking** — can a reader understand one piece without holding the whole
  package in their head? Where the concept boundaries are wrong, say so.

## Which package

If `$ARGUMENTS` names a package (e.g. `engine`, `internal/catalog`, `media/tape`),
review that one. Otherwise, list the candidates with
`find internal -maxdepth 1 -type d` plus a `wc -l` of each, and ask the user which
package to review before proceeding.

## Ground yourself first

1. Read `ARCHITECTURE.md` — the **Package map**, the **Load-bearing decisions**
   (each states what a package *should* and *should not* know), and the
   **vocabulary**. This is the yardstick: a name that contradicts the vocabulary
   is a finding; code that knows something a decision says it must not is a
   "doesn't belong" finding.
2. Read the **whole package** in full — every non-test `.go` file — plus the
   relevant `_test.go` files for how the abstractions are actually used.
3. Remember this is **greenfield / pre-release** (no back-compat, no migrations —
   renames, splits, and deletions are cheap) and **Amanda-faithful** (an
   Amanda-lineage name in prose is fine; the issue is when it leaks into a
   shape-agnostic API).

## Report

Synthesize a tight, prioritized report — advice, not edits:

- **Top changes by impact** — a short ranked list a maintainer could act on first.
- **Findings**, each with: a short title; a `file:line` reference; severity
  (high/med/low by impact on understandability); 1–2 sentences on **why** it hurts
  the reader; and a **concrete** change (rename X, split file Y into Y1/Y2, move Z
  to package W, extract sub-abstraction, inline pointless indirection).
- **Well-factored (leave alone)** — a few abstractions that are already
  just-right, so the report is calibrated and nobody "fixes" what's correct.

Be specific, critical, and honest. **Do not modify any files** unless the user
explicitly asks you to apply a finding afterward.
