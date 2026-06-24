---
description: Review the NBackup codebase for understandability and maintainability — find hard-to-follow code, duplication, leaky abstractions, missing/over-abstractions, and naming drift, then report concrete simplifications.
---

You are doing a **code-quality review** of the NBackup codebase — NOT a bug hunt
and NOT a security review. The goal is to make the code **easier to understand
and more maintainable**. Look for: code that is hard to follow, duplicated logic,
leaky abstractions, missing abstractions (a repeated pattern begging for a
helper/type), speculative/over-abstraction (an interface with one impl,
indirection with no payoff), and naming that misleads or drifts from the
architecture vocabulary. Propose simplifications, abstraction add/remove, and
renames. Be specific, critical, and honest; note what is well-factored too so the
findings are calibrated and nobody "fixes" something that's already right.

If `$ARGUMENTS` is non-empty, focus the review on those packages/areas (e.g.
"engine", "media", "duplication"); otherwise review the whole tree.

## Ground yourself first

1. Read `ARCHITECTURE.md` end to end — especially the **Package map**, the
   **Load-bearing decisions** (each states what a package *should* and *should
   not* know), the **vocabulary** (DLE / Run / Slot / Archive / Cycle /
   Medium=Volume / Entry+Placement+Part / Label / Bay / reel / Drive / Changer /
   Shelf), and the **medium-neutral vocabulary** rule (the generic
   media/changer/config layer must not say "tape"). The architecture is the
   yardstick: a leaky abstraction is code that violates a stated decision; a
   naming-drift finding is a symbol that contradicts the vocabulary.
2. Get the lay of the land: `find internal cmd -name '*.go' -not -name '*_test.go' | xargs wc -l | sort -rn`
   to see where the mass is (the big files are the usual suspects).
3. Remember this is **greenfield / pre-release** (no back-compat, no migrations —
   deletions and renames are cheap) and **Amanda-faithful** (an Amanda-lineage
   name in prose is fine; the issue is when it leaks into a shape-agnostic API).

## Fan out parallel reviewers

Launch parallel subagents (general-purpose), one per package group, so the review
is fast and independent. Each agent reads the listed files **in full**, grounds in
the relevant ARCHITECTURE.md decisions, and returns a tight findings list. Suggested
grouping (adjust to the current tree / to `$ARGUMENTS`):

1. **engine** — `internal/engine/*.go` (the driver; the biggest file). Check it
   stays medium-shape-agnostic (dispatch belongs in `librarian`) and look for
   duplicated write-target/read-path setup between `Run`/`CopySlot`/`Verify`.
2. **cli** — `internal/cli/*.go`. Test the "thin command wiring" claim: business
   logic leaking into the CLI, duplicated arg/flag/output handling, long handlers.
3. **media core** — `internal/librarian/*.go`, `internal/media/{media,profile}.go`.
   Is the Drive/Changer/Shelf/Volume/Labeled split clean? Type-assertion / shape
   sniffing outside librarian? Is the capacity model (TotalBytes vs VolumeSize)
   legible? Does tape vocabulary leak into the neutral layer?
4. **media impls** — `internal/media/{disk,cloud}/*.go`, `internal/media/tape/*.go`.
   The architecture says cloud's layout is the disk medium's *verbatim* — check for
   actual disk↔cloud duplication. Is the dir/manual/mt split inside tape clear?
5. **persistence** — `internal/{slot,slotio,catalog}/*.go`. Is the
   Entry/Placement/Part/Archive/Slot model clear in code? Duplication between
   writer/reader and the scan rebuild path? Parallel position-types + converters?
6. **adapters + pure domain** — `internal/{method,method/gnutar,filter,crypt,xfer}`
   and the pure `internal/{planner,policy,restore,recovery}`. The architecture says
   `crypt` "mirrors `filter`" — confirm or refute whether they *share* the process
   plumbing or *duplicate* it. Verify the pure packages stay pure (no os/exec/media).
7. **config + support** — `internal/{config,progress,sizeutil,lock}/*.go`. Scattered
   defaulting/validation, smeared responsibilities, byte/duration-formatting that
   should be centralized in `sizeutil`.

Give each agent this rubric. For **every finding** require: a short title; a
`file:line` reference; severity (high/med/low by impact on understandability &
maintainability); 1–2 sentences on **why** it hurts; and a **concrete** suggested
change (extract X, merge Y, rename Z, delete dead code). Tell them to be selective
— real findings only, no gofmt-level nitpicks, no speculative layering, and to
flag well-factored code as `[GOOD]` so the report is calibrated.

## Verify before you report

Do not relay subagent claims blind. For each **high-severity** finding —
especially duplication, dead code, and abstraction-leak claims — confirm it
yourself with a quick `diff`/`grep` (e.g. `diff` the two allegedly-identical
function ranges; `grep -rn '<symbol>' internal` to prove a function has no
callers; `grep` for the shape-assertion the leak claim depends on). Drop or
downgrade anything you can't confirm.

## Report

Synthesize one consolidated, **prioritized** report (do not just concatenate the
agents). Structure it as:

- **Top simplifications by impact** — a short ranked list a maintainer could act on first.
- **Findings grouped by theme** — Duplication / Leaky abstractions / Long &
  overloaded functions / Naming & vocabulary / Missing or over-abstraction / Dead
  code. Each finding: title, `file:line`, severity, why, concrete fix. Merge
  cross-cutting findings the agents reported separately (e.g. the same duplication
  seen from two packages).
- **Well-factored (leave alone)** — a few things that are already right.

Be honest about scope: this is advice, not edits. **Do not modify any files**
unless the user explicitly asks you to apply a finding afterward.
