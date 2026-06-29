---
name: worktree-main
description: Merge the current git worktree's feature branch into main. Use whenever the user asks to "merge to main", "land this", "ship it to main", "integrate this branch", or similar AND the working directory is a linked git worktree (not the primary worktree where main is checked out). Encodes the safe conflict-in-isolation + fast-forward flow so main is never the place conflicts get resolved.
---

# worktree-main

Land the current worktree's feature branch onto `main` **safely**: resolve any
conflicts inside this isolated worktree, verify, then fast-forward `main` from its
own worktree. Conflicts must never be resolved on `main` itself.

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

3. **Pull the newer `main` into this branch — resolve conflicts HERE.**
   ```
   git diff --name-only $(git merge-base HEAD main) main   # preview overlap with your files
   git merge --no-edit main
   ```
   If `main` is already an ancestor of `HEAD` (nothing new upstream), this is a
   no-op and you can skip to step 5. Otherwise resolve any conflicts in this
   isolated worktree — `main`'s own worktree stays untouched and clean.

4. **Verify the merged tree** before touching `main`. Per CLAUDE.md:
   ```
   gofmt -l .          # expect no output
   go build ./...
   go vet ./...
   go test -race ./... # at minimum the packages your change + the merge touched
   ```
   Do not proceed if any fail — fix on this branch.

5. **Fast-forward `main` to this tip, from main's worktree.**
   ```
   git merge-base --is-ancestor main HEAD   # must succeed: ff is possible
   git -C <ROOT> status --short             # main's worktree must be clean
   git -C <ROOT> merge --ff-only <BRANCH>
   ```
   `--ff-only` guarantees no merge commit and no possibility of a conflict on
   `main` — because step 3 already folded `main` into `BRANCH`, `main` is a strict
   ancestor of `HEAD`, so the only outcome is a clean pointer advance. If
   `--ff-only` is rejected, something changed `main` concurrently: re-run step 3
   and retry; do not force.

6. **Report** the old→new `main` sha and that nothing was pushed (no credentials).

## Why this shape

- Conflicts are resolved in the throwaway worktree, so `main` is only ever advanced
  by a fast-forward — `main`'s working tree is never dirtied and a botched merge
  can't strand it.
- `--ff-only` is the safety interlock: it fails loudly rather than silently
  creating a merge commit if the branches diverged after step 3.
