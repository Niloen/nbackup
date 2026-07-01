---
description: Road-test NBackup as a brand-new user — read the docs, exercise common use cases in parallel, and report bugs + usability findings.
---

You are road-testing **NBackup** the way a brand-new user would: read the
documentation, plan the common use cases, actually run them, and come back with a
prioritized list of improvements. Find **bugs AND usability problems** — not just
confirm things work. Be specific, critical, and honest; record what works well too.

If `$ARGUMENTS` is non-empty, focus the review on that area (e.g. "tape",
"encryption", "restore"); otherwise cover everything below.

## Setup (do this first)

1. Read `README.md`, `ARCHITECTURE.md`, and `nbackup.example.yaml` to learn the
   intended workflow as a user would.
2. Build the binary: `make build` → `./bin/nb` (use this absolute path; don't rebuild per-agent).
3. **Environment constraints (important):**
   - This machine has **no `zstd`** — use `codec: none` or `codec: gzip` in every config.
   - `tar`, `gzip`, `gpg`, `nice` are present; `gtar` and `mt` are not.
   - No cloud credentials and no real tape hardware: test cloud only if creds exist;
     test tape with the **file-backed** forms (`dir:` library with `slots:`/`drives:`, or `dir:` + `manual: true`).
   - Any command that might block on an interactive tape swap: run it under `timeout 30`
     so you never hang. Note anywhere it hangs instead of erroring cleanly.
   - Work entirely inside fresh `mktemp -d` dirs; never touch the repo working tree.
     For gpg, use an isolated `GNUPGHOME`.
   - Backdated `nb dump --date <past>` is rejected by design — build history forward
     from today (today + future dates) instead.

## Run the use cases in parallel

Launch parallel subagents (one per area, in their own temp dirs) so the review is
fast and independent. Each agent acts as a new user, follows the
README, and reports findings. Cover at least these areas, but explore other usecases as well:

1. **Core lifecycle (disk):** quick-start config, `plan` / `plan --days`, `dump`,
   inspect the run dir, `run` / `run <id>` / `medium` / `status`, `verify`
   (corrupt a byte and confirm it's caught), incrementals across dates (L0→L1→L2),
   same-day `.N` sequencing + immutability, stock-tool portability (`tar`/`gzip` with no `nb`).
2. **Recover / restore:** `recover --all` whole-DLE restore (deletion-accurate,
   point-in-time, non-empty `--dest` guard + `--force`); file-level `--path/--list`;
   the interactive shell (`disks`, `setdisk`, `setdate`, `cd`, `ls`, `add`, `extract`);
   verify restored trees against ground truth you control.
3. **Tape (file-backed):** label / relabel / overwrite guard / foreign-bay detection;
   `load`; `medium` inventory; dump landing on tape; auto_label on/off; **spanning** a
   run across volumes (tiny `volume_size`) + restore/verify of the spanned run;
   unattended runs must **error cleanly, never hang**; manual-station swap prompts.
4. **Replication & pruning:** `copy` (copies by default + `--dry-run`/`-n`), `sync`
   (`--to`/`--from`/`--last`/`--since`, config `sync:` rules, idempotency),
   `prune <medium>` (per-medium retention, safety floor, capacity reclamation,
   independence between media).
5. **Encryption (gpg):** symmetric (`passphrase_file`) and public-key (`recipient`);
   payload actually encrypted; seal/header carry only the scheme name, never a key;
   keyless `verify`/`run <id>`/`copy`/`sync`; restore needs the key and fails clearly
   without it; stock-tool `gpg -d | … | tar` portability; no secret leaks in args/logs.
6. **Planning / config / help / errors:** first-run with no config; the example config
   verbatim; a minimal config from the README; broken configs (bad YAML, unknown
   medium type, bad capacity/cycle, **typo'd keys**, missing dumptype); `plan` /
   forecast / `dump --dry-run` legibility; capacity warning; every command's `--help`;
   bogus command/flag; README-vs-CLI drift.

## Report

Have each agent return a numbered findings list; then synthesize one consolidated,
**prioritized** report. Tag each finding `[BUG]` / `[USABILITY]` / `[GOOD]` with a
severity (high/med/low), the exact repro command, and a one-line suggested fix.
Group by severity. Verify the highest-impact bugs yourself before reporting them.
Finish with a short "top fixes by impact" list.
