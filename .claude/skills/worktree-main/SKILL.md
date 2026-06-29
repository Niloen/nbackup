---
name: worktree-main
description: Merge the current git worktree's feature branch into main. Use whenever the user asks to "merge to main", "land this", "ship it to main", "integrate this branch", or similar AND the working directory is a linked git worktree (not the primary worktree where main is checked out). Encodes the safe conflict-in-isolation flow plus a feature merge commit so main is never the place conflicts get resolved and every landing names the feature it adds.
---

# worktree-main

Land the current worktree's feature branch onto `main` **safely**: rebase this
branch onto `main` inside the isolated worktree (resolving any conflicts here),
verify, then record a **feature merge commit** on `main` from its own worktree.
Conflicts must never be resolved on `main` itself.

The landing is a `--no-ff` merge with a message that summarizes *this feature* —
so `main`'s history reads as "Merge feature X into main", not the backwards
"Merge branch 'main' into worktree-X" that a fast-forward of a `merge main` would
leave behind.

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

2. **Commit this worktree's work.** The user asking to merge is the explicit
   request to commit (per the repo's "commit only when asked" rule). Write a clear
   message and end it with the `Co-Authored-By` trailer. Never push.

3. **Rebase this branch onto the newer `main` — resolve conflicts HERE.**
   ```
   git diff --name-only $(git merge-base HEAD main) main   # preview overlap with your files
   git rebase main
   ```
   If `main` is already an ancestor of `HEAD` (nothing new upstream), this is a
   no-op. Otherwise resolve any conflicts in this isolated worktree
   (`git rebase --continue` after each), so `main`'s own worktree stays untouched
   and clean. Rebasing keeps the feature's commits linear — no backwards
   "Merge branch 'main' into …" commit — and leaves `main` a strict ancestor of
   `HEAD`, which is what makes the step-5 merge conflict-free.

4. **Verify the rebased tree** before touching `main`. Per CLAUDE.md:
   ```
   gofmt -l .          # expect no output
   go build ./...
   go vet ./...
   go test -race ./... # at minimum the packages your change + the rebase touched
   ```
   Do not proceed if any fail — fix on this branch.

5. **Record the feature merge commit on `main`, from main's worktree.**
   ```
   git merge-base --is-ancestor main HEAD   # must succeed: main is an ancestor of HEAD
   git -C <ROOT> status --short             # main's worktree must be clean
   git -C <ROOT> merge --no-ff <BRANCH> -m "<feature summary>" -m "<details>" \
     -m "Co-Authored-By: ..."
   ```
   Write a **feature-level** message: a one-line summary of what the branch adds
   to `main` (not a restatement of the last commit), an optional body, and the
   `Co-Authored-By` trailer. Because step 3 made `main` a strict ancestor of
   `HEAD`, `--no-ff` can only fabricate a merge commit on top — never a real
   3-way merge, so no conflict can land on `main`. If the `--is-ancestor` check
   fails, something advanced `main` concurrently: re-run step 3 and retry; do not
   force.

6. **Report** the old→new `main` sha, the merge-commit message, and that nothing
   was pushed (no credentials).

## Why this shape

- Conflicts are resolved by rebasing in the throwaway worktree, so `main`'s
  working tree is never dirtied and a botched merge can't strand it.
- Landing with `--no-ff` gives every feature one commit on `main` that names it —
  the merge bubble is the feature's headline — while the rebased commits below it
  stay linear and readable.
- The `--is-ancestor` check is the safety interlock: it fails loudly if the
  branches diverged after step 3, so the `--no-ff` merge is always the trivial,
  conflict-free kind.
