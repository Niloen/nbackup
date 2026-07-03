---
name: worktree-main
description: Merge the current git worktree's feature branch into main. Use whenever the user asks to "merge to main", "land this", "ship it to main", "integrate this branch", or similar AND the working directory is a linked git worktree (not the primary worktree where main is checked out). Encodes the safe conflict-in-isolation flow, a fast-forward for trivial landings and a feature merge commit only for multi-commit features, and landing-time verification scoped to what the rebase actually changed.
---

# worktree-main

Land the current worktree's feature branch onto `main` **safely**: rebase this
branch onto `main` inside the isolated worktree (resolving any conflicts here),
verify what the rebase changed, then advance `main` from its own worktree —
**fast-forward for a trivial landing, a feature merge commit only when the
feature is more than one commit**. Conflicts must never be resolved on `main`.

Two things this flow is careful about:

- **No diamond for trivial landings.** After the rebase, `main` is already a
  strict ancestor of `HEAD`, so a `--no-ff` merge of a *single* commit fabricates
  a two-parent bubble sitting on top of exactly one commit — a diamond that says
  nothing the commit doesn't. So a one-commit branch fast-forwards (linear, the
  commit *is* the headline); only a genuinely multi-commit feature gets a merge
  bubble that names it.
- **No re-running the whole suite for a small change.** The branch was already
  `gofmt`/`vet`/`go test -race ./...`-clean during development (that is where
  CLAUDE.md's verify rule is satisfied). Landing only needs to re-verify what the
  *rebase* changed — often nothing.

## When this applies

- You are in a **linked worktree** on a feature branch (e.g.
  `worktree-<topic>`), and `main` is checked out in a **different** worktree
  (usually the repo root). Confirm with `git worktree list`.
- The user asked to merge/land/ship/integrate this branch to `main`.

If `main` is checked out *here* (no separate worktree), this skill does not apply —
just commit and you are already on `main`.

## Procedure

Run everything from the current worktree dir. Reach the other worktree with
`git -C <path>`, never `cd`.

1. **Locate the worktrees and branch.**
   ```
   git rev-parse --abbrev-ref HEAD        # this feature branch
   git worktree list                      # find which path has main checked out
   git status --short                     # uncommitted work here
   ```
   Let `ROOT` = the path whose branch is `[main]`, `BRANCH` = this feature branch.

2. **Commit this worktree's work with a feature-level message.** The user asking
   to merge is the explicit request to commit (per the repo's "commit only when
   asked" rule). Because a single-commit branch fast-forwards (step 5), this
   commit's subject may *become* main's headline for the feature — so write it at
   feature altitude (what this branch adds to main), not as a narrow "fix typo"
   note. End with the `Co-Authored-By` trailer. Never push.

3. **Rebase this branch onto the newer `main` — resolve conflicts HERE.**
   ```
   git rev-list --count HEAD..main                          # commits main is ahead; 0 = no-op rebase
   git diff --name-only $(git merge-base HEAD main) main    # preview overlap with your files
   git rebase main
   ```
   Note whether the rebase was a **no-op** (main was already an ancestor — the
   count above is `0`) or **replayed** your commits onto a newer `main`. Step 4
   uses this. Resolve any conflicts in this isolated worktree
   (`git rebase --continue` after each) so `main`'s own worktree stays clean.
   Rebasing keeps the feature's commits linear and leaves `main` a strict ancestor
   of `HEAD`, which is what makes step 5 conflict-free.

4. **Verify — scoped to what the rebase changed.** `gofmt -l .` and, if any `.go`
   files changed, `go build ./...` + `go vet ./...` are cheap and always run in
   full (expect no output / no errors). The slow part is `-race`; scope it:

   - **No-op rebase** (main was already an ancestor): nothing replayed, so the
     branch is exactly as it was tested during development. A `go build ./...`
     sanity check is enough — skip the race suite.
   - **Docs/config-only landing** (`git diff --name-only main...HEAD` matches no
     `*.go`): skip build/vet/test entirely; there is no runtime surface to verify.
   - **Replayed rebase with Go changes:** race-test the packages the landing
     touches plus their direct importers, not all 40:
     ```
     # packages changed by this landing
     git diff --name-only main...HEAD | grep '\.go$' | xargs -r -n1 dirname | sort -u
     # their direct importers (add to the set)
     go list -f '{{.ImportPath}} {{join .Imports " "}}' ./... | grep -E '<changed-import-paths>'
     go test -race ./internal/<pkgA>/... ./internal/<pkgB>/...   # the union
     ```
   - **Escape hatch — go full.** If the change touches a widely-imported core
     package (`engine`, `spool`, `media`, `catalog`, `conductor`) or the import
     graph makes the affected set most of the tree, just run
     `go test -race ./...`. Cheaper to run it all than to reason about the blast
     radius.

   Do not proceed if anything fails — fix on this branch.

5. **Advance `main` from main's worktree — fast-forward or feature merge.**
   ```
   git merge-base --is-ancestor main HEAD    # must succeed: main is an ancestor of HEAD
   git -C <ROOT> status --short              # main's worktree must be clean
   n=$(git rev-list --count main..HEAD)      # commits this branch adds
   ```
   - **`n == 1` (trivial landing):** fast-forward — no bubble, linear history, the
     commit is the headline (that is why step 2's message matters).
     ```
     git -C <ROOT> merge --ff-only <BRANCH>
     ```
   - **`n >= 2` (multi-commit feature):** record a feature merge commit whose
     message names what the branch adds to `main` (not a restatement of the last
     commit), with an optional body and the `Co-Authored-By` trailer:
     ```
     git -C <ROOT> merge --no-ff <BRANCH> -m "<feature summary>" -m "<details>" \
       -m "Co-Authored-By: ..."
     ```
   Because step 3 made `main` a strict ancestor of `HEAD`, both forms are trivial
   and conflict-free. If the `--is-ancestor` check fails, something advanced `main`
   concurrently: re-run step 3 and retry; do not force.

6. **Report** the old→new `main` sha, whether it was a fast-forward or a feature
   merge (and its message), the verification actually run (full / scoped / skipped
   and why), and that nothing was pushed (no credentials).

## Why this shape

- Conflicts are resolved by rebasing in the throwaway worktree, so `main`'s
  working tree is never dirtied and a botched merge can't strand it.
- A merge bubble is reserved for features that are *actually* several commits, so
  it means something in the graph. Single-commit landings fast-forward, so `main`
  reads as a clean line of feature-named commits instead of a chain of noise
  diamonds each capping one commit.
- Landing-time verification re-checks the *rebase delta*, not the whole feature:
  the branch already passed `-race ./...` during development, and a no-op rebase
  changed nothing, so re-running 40 packages proves nothing new. When the rebase
  did replay onto a newer `main`, the affected packages + their importers are the
  only place a semantic conflict could hide.
- The `--is-ancestor` check is the safety interlock: it fails loudly if the
  branches diverged after step 3, so the step-5 advance is always trivial.
