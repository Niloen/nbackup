# NBackup

A slot-based backup system in Go. Its design comes from **[Amanda][amanda]** —
balanced multilevel scheduling, immutable daily artifacts, human-readable
contents, and cycle-based safety. What NBackup adds is **first-class disk and
cloud storage**: Amanda is tape-first, while NBackup treats local disk, virtual
tape, and object stores (S3, GCS, Azure Blob) as equal targets, and makes the
common modern shape — land fast on disk, then replicate offsite — a first-class
operation.

> A backup administrator should be able to reason about backups by looking at a
> sequence of immutable daily backup slots rather than a database of chunks.

Its artifacts are portable: every backup restores with standard tools, no
NBackup required. This is a first version — see [PRD.md](PRD.md) for the full
product vision. (The rest of this page assumes the Amanda lineage above and
calls it out again only where a specific mechanism is worth tracing back.)

## Core ideas

- **Slot** — the primary artifact. One run produces exactly one slot, an
  immutable directory you can copy, inspect, and understand without NBackup.
- **DLE** — a backup source (`host` + `path`).
- **Run** — one planner execution, typically daily.
- **Cycle** — the dump cycle: the target and hard-max time between fulls of each
  DLE, and the window retention protects.
- **Volume** — where slots live: local disk, a virtual tape, or a cloud object
  store (S3/GCS/Azure). Slots stream between volumes (`nb copy` for one, `nb sync`
  for many) — e.g. land fast on disk, then replicate offsite to tape or the cloud.

## Artifacts you can read

A **volume** is an ordered sequence of self-describing files, each carrying an
identity **header** (slot, DLE, level, scheme, …) and addressed by position.
A **slot** is a run of archives; each archive is its payload followed by a
**member index** (its file list) and a **commit footer** (its identity, sizes,
and checksums), the footer written last so its presence proves the archive landed
whole. A slot is complete once every archive it planned has committed. The same
shape maps to disk, an object store, or tape; each medium frames the header its
own way.

On **disk** the header is a separate `.hdr` sidecar so the payload stays a clean
archive, and the files are human-friendly — one archive is three numbered files
(payload, index, commit):

```text
slots/slot-2026-06-21/
  000000-app01-home-L0.tar.zst        # clean compressed tar (payload)
  000000-app01-home-L0.hdr            # JSON header sidecar
  000001-app01-home-L0-index.json.gz  # gzipped member list (browse without extracting)
  000001-app01-home-L0-index.hdr
  000002-app01-home-L0-commit.json    # per-archive footer: identity + sizes + checksums
  000002-app01-home-L0-commit.hdr
  000003-db01-pg-L1.tar.zst           # the next archive continues the numbering
  000003-db01-pg-L1.hdr
  000004-db01-pg-L1-index.json.gz
  000004-db01-pg-L1-index.hdr
  000005-db01-pg-L1-commit.json
  000005-db01-pg-L1-commit.hdr
```

The `NNNNNN` prefix is the file's position on the volume — a running counter, so
it keeps climbing across the slots that share a volume rather than resetting to
`000000` each slot. Each archive's **commit footer is its last file**; its
**payload is always the first** of the three, which is all the stock-tool restore
below needs (it globs the `…-L<n>.tar*` payloads and ignores the index/commit).

(On **tape** the header is instead a fixed 32 KB block inline ahead of each
payload, since a tape has no sidecars.)

Archives are produced by **GNU tar** in listed-incremental format, then piped
through an external compressor (`zstd` by default; `gzip` or `none` also built
in) and, optionally, an external **encryptor** (`gpg`). NBackup orchestrates
`tar`, the compressor, and gpg as child processes rather than reimplementing
them, so the tools that wrote an archive are the tools that read it —
**recovery never requires NBackup**. A full restores with one pipe:

```bash
zstd -dc 000000-app01-home-L0.tar.zst | tar -xf -
```

Restoring a full + its incrementals replays one archive per level in order,
exactly as `nb recover` does — and `nb drill --tier stock` rehearses that
bare-tools path for you and prints the commands. (Encrypted archives keep the
same name and just add a `gpg -d` at the front; spanned tape parts are listed by
`nb slot show`.) The full by-hand procedure is in
[docs/restore-by-hand.md](docs/restore-by-hand.md).

### Encryption

Set an `encrypt` block (config-wide, or per dumptype) to pipe each archive
through **gpg** after compression — public-key (`recipient`) or symmetric
(`passphrase_file`). Encryption is *source-tied*: the dump is encrypted once and
every copy holds the same ciphertext, so vaulting with `nb sync` never needs the
key. The archive records only the **scheme name** (`gpg`), never a key — gpg finds
the right key in the operator's keyring from the ciphertext itself, so a
**public-key** dump restores on any host with the private key, even with the config
gone. (A **symmetric** `passphrase_file` dump still needs the `encrypt` block at
restore to point gpg at the passphrase file — there is no key-id in the ciphertext.)

Two consequences: **lose the key and the data is unrecoverable** (NBackup holds
no copy by design), and each archive's **commit footer and member index stay
plaintext** — filenames and checksums remain readable so `nb recover` can browse
without the key. Integrity (`nb verify`) and copy/sync also stay keyless; only
extraction needs the key.

## Install

```bash
make build          # builds ./bin/nb
# or
go install ./cmd/nb
```

This produces a single `nb` binary. The convention: you **inspect** with a noun
(`nb slot`, `nb dle`, `nb medium`) and **act** with a flat verb (`nb dump`,
`nb recover`, `nb prune`, …).

| Command              | Purpose                                                  |
|----------------------|----------------------------------------------------------|
| `nb check`           | Verify the config and reach every source host           |
| `nb plan`            | Show what the next run would do                          |
| `nb dump`            | Execute a run and commit its archives                    |
| `nb status`          | Show progress of the current (or most recent) run        |
| `nb report`          | Summarize recent runs, or print one dump's per-DLE report |
| `nb slot`            | List slots (default)                                     |
| `nb slot show`       | Show a single slot's archives and copies                |
| `nb dle`             | List DLEs, or detail one's archive timeline across slots |
| `nb medium`          | List media, or detail one (incl. bays / drive + shelf)    |
| `nb verify`          | Verify slot integrity: checksums, or `--deep` structure  |
| `nb drill`           | Rehearse recovery: prove backups are restorable          |
| `nb recover`         | Recover as of a date: browse + pick files, or `--all` for a whole DLE |
| `nb copy`            | Copy one slot between media (`--from`/`--to`, e.g. disk → tape) |
| `nb sync`            | Mirror one medium's slots onto another (disk → tape/s3)  |
| `nb label`           | Label a volume (required for tape before its first dump) |
| `nb load`            | Load a volume into a medium's drive (bay or shelf reel)   |
| `nb prune <medium>`  | Delete a medium's slots past its cycle/capacity limits  |
| `nb rebuild`         | Rebuild the local slot-index cache from media            |

Run `nb help <command>` (or `nb <command> --help`) for per-command usage and
examples, and `nb completion <shell>` to generate shell completion.

## Quick start

```bash
cp nbackup.example.yaml nbackup.yaml   # edit sources + catalog path

nb plan                # preview today's plan, capacity usage, and $/month cost (cloud media)
nb plan --days 30      # forecast the next 30 daily runs + the $/month cost curve (cloud media)
nb dump                # run the backup, committing one slot's archives
nb dump --dry-run --date 2026-07-15    # plan that day's run; writes nothing
nb status              # progress of the running (or most recent) dump
nb slot                # list slots (with a COPIES column: where each lives)
nb slot show slot-2026-06-21   # archives + every copy's volume and file positions
nb medium              # media overview: type, slots, usage / capacity, volume
nb medium lto          # one medium's volume and the slots it holds
nb verify --all        # re-check every slot's archive checksums
nb recover --dle app01:/home --date 2026-06-21 --all --dest /tmp/out   # whole-DLE restore
```

These global flags work with every command and may appear anywhere on the command
line — before or after the subcommand and its arguments:

| Flag              | Purpose                                  |
|-------------------|------------------------------------------|
| `-c, --config`    | path to config file (default `nbackup.yaml`) |
| `--catalog`       | catalog directory (overrides config)     |
| `-q, --quiet`     | suppress progress output                 |

## How it works

### Planning

NBackup uses a **multilevel** scheme (levels 0–9) with a dynamic, estimate-driven
schedule and only two inputs — the **cycle** and the medium's **capacity**, no
balancing knobs. Levels are realized with GNU tar's listed-incremental **snapshot
library** under `<state_dir>/gnutar/<dle>/L<n>.snar`, turning tar's two-level
primitive into N-level backups.

**What each run decides.** In order:

1. **Estimate** every DLE's full size and its incremental at the current level and
   the next, by running the dump method against `/dev/null` — tar walks metadata
   without reading file bodies, so it is fast yet exactly honors excludes,
   one-file-system, and the incremental snapshot. Sizes are uncompressed: an upper
   bound on bytes stored.
2. **Pick a level** per DLE: never-fulled → mandatory L0; at or past the **cycle
   deadline** → forced L0; otherwise an incremental. The cycle is a *hard* ceiling
   — a full never ages past it, so a full is either due or it isn't. Incrementals
   follow a **bump** rule rather than climbing a level per run: a DLE sits at level
   1 — re-dumping everything since the full — and climbs higher only after holding
   the current level a couple of runs *and* when climbing would save at least
   `bump_percent` of the full size (default 5%). So L1 is the common case, deep
   levels are earned by real savings, restore chains stay short, and consecutive
   incrementals overlap for redundancy.
3. **Promote** to balance — the *only* balancing lever, automatic (no knob). It
   builds a **deadline calendar** of upcoming fulls and pulls a full from the
   heaviest future day onto a lighter run. It promotes a DLE onto today only while
   (a) today is lighter than that future peak, (b) the move strictly lowers the
   peak — so a *lone* big DLE is never re-fulled early, since moving it would just
   relocate the peak — and (c) it fits the per-run room. With no free capacity it
   does nothing; with room to spare it keeps backups fresh.

This **de-clumps the cold start**: day one fulls everything (recoverability first),
and promotion staggers the resulting lock-step apart over the next cycle or two. The
planner consumes only bytes — it never knows whether the medium is tape or an object
store.

#### Two capacity limits

Capacity is the one number you give a medium, and it binds at two scopes:

| Scope | What must fit | How it's bounded |
|-------|---------------|------------------|
| **Per run** | A single run's peak. | **Promotion** is capped at the room left before pruning would evict a *protected* slot (`capacity − protected set`, tightened by the landing volume's free space). No room → no promotion; a run may still be lumpy when a big DLE hits its own deadline. |
| **Per cycle** | A **complete recovery set**: one full of every DLE — they coexist when `minimum_age ≥ cycle`, so `Σ full_est` must fit capacity. | Structural — no scheduling can change the cycle's fixed full demand. |

When `Σ full_est` exceeds capacity the plan carries a **warning** —
recoverability is at risk, backups still run — the signal to grow capacity or
lengthen the cycle, rather than silently pruning the oldest recovery points away.
The priority order is immovable: recoverability and cycle safety come first;
capacity bounds balance.

#### Forecasting

`nb plan --days N` projects the planner forward over N daily runs, advancing a
*copy* of the history after each simulated run — so the forecast shows when each
DLE's next full lands and how its incrementals bump in between, not just today's
decision repeated. Estimates and the capacity ceiling are sampled once and held
constant (a *level-schedule* forecast, not a capacity timeline); nothing is written.

`nb dump --dry-run [--date <day>]` is the single-run dry run: it plans the run for
`--date` against the current catalog — the exact decision a real `nb dump --date
<day>` would make — and prints it without writing anything.

#### Cost (what the bill will be)

You reason in dollars per month, not bytes: `nb plan` prints the current footprint's
**storage `$/month`** and the **marginal `$/month`** the next run adds; `nb plan
--days N` adds a **`$/MONTH` column** projecting the cost curve as fulls land and
pruning reclaims. A medium **prices itself** — with no config a cloud bucket infers
its provider from the URL scheme (`s3://` = AWS, `gs://` = GCS, `azblob://` = Azure);
local disk/tape has no recurring bill and shows no cost line.
An optional per-medium `cost:` block overrides a rate (a region's egress) or names a
different provider table. Egress on a restore is surfaced where it bites: `nb recover`
estimates **egress `$`** before pulling from a cloud store, warning — and,
interactively, confirming — when it is material; an offsite `nb drill`'s forecast
egress carries a `$`. Pricing is a flat estimate (storage + egress + request —
NBackup does not model Glacier/Deep-Archive lifecycle tiers) and **fully offline**:
a calculation over the catalog and a rate table, no billing API.

### Slot naming and multiple runs per day

The first run of a day is `slot-YYYY-MM-DD`. Run again the same day and you get
`slot-YYYY-MM-DD.2`, `.3`, … Each slot stays immutable; a committed date is never
overwritten. Restores and pruning order slots by date **then** sequence.

### Committing

A run writes each archive's payload, verifies its checksum against what landed on
the volume, then appends the archive's **member index** and, last, its **commit
footer** (identity, sizes, checksum, part count). Written last, the footer's
presence marks that archive complete and immutable. There is no slot-level seal:
"run complete?" is *derived* — did every planned DLE commit? — not a stored flag,
so a crashed run keeps every committed archive and a rerun fills in the rest.

### Monitoring a run

A long `nb dump` (run detached, e.g. from cron) reports progress to a status file
in the catalog workdir. From any other shell, `nb status` reads it and prints an
at-a-glance report:

```text
Run slot-2026-06-21  [running]
  started:  2026-06-21 02:00:03  (elapsed 4m12s)
  workers:  2 configured, 2 active
  dles:     1 done, 2 active, 1 pending

DLE            LEVEL  STATE    PROGRESS           DONE       EST        WRITTEN
app01:/etc     L1     done     [##########] 100%  120.00 kB  ~118.0 kB  41.00 kB
app01:/home    L0     dumping  [####......]  42%  8.40 GB    ~20.0 GB   3.10 GB
db01:/pg       L0     dumping  [##........]  18%  3.60 GB    ~20.0 GB   1.40 GB
app01:/var     L1     pending  -                  0 B        ~2.0 GB    0 B

Total:    12.12 GB of ~62.12 GB  (20%)
Rate:     48.10 MB/s
ETA:      17m18s
```

Each DLE's percentage is uncompressed bytes against the planner estimate; the run
streams source→compressor→volume in one pass, so there is a single `dumping` state
per DLE (no separate dumper/taper queues). `nb status --watch 2s` refreshes until the
run finishes; afterwards `nb status` shows the last run's final result.

### Reporting & alerting (unattended)

`nb status` shows one live run; a hands-off install also wants the *history* and a
way to be told when something breaks. Every mutating command (`dump`, `sync`,
`prune`, `verify`, `drill`) records a machine-readable summary to the catalog
workdir — appended to `run-log.jsonl` and mirrored as `run-summary.json` (scrape it
from a monitoring system) — and exits non-zero on failure.

`nb report` summarizes the recent history — what ran, what failed, bytes moved —
and a recovery-health audit that flags any DLE whose drills are failing, *degrading*
(passed before, failing now), stale, or never run:

```text
NBackup report — 3 run(s) from 2026-06-23 02:00 to 2026-06-23 02:25
1 run(s) FAILED, 10.74 GB moved

WHEN              COMMAND  OUTCOME  DETAIL
2026-06-23 02:25  drill    FAILED   1 failure(s), 1 overdue
2026-06-23 02:13  sync     OK       1 slot(s) copied, 5.37 GB
2026-06-23 02:00  dump     OK       slot-2026-06-23, 3 archive(s), 5.37 GB

FAILURES
  2026-06-23 02:25 drill [drill-failures]: 1 drill failure(s) — recovery is at risk

DRILL COVERAGE
  FAILING DLE  CLASS     LAST DRILL        REMEDY
  app01:/home  pipeline  2026-06-23 02:25  the archive would not decrypt/decompress/untar — …
  stale (overdue past 30d): app01:/etc (84d ago)
```

`nb report --last 30` widens the window; `nb report --json` emits the raw records.

For the classic per-DLE dump report, `nb report --dump`
prints the latest dump in detail — each DLE's level, original/output size,
compression %, files, dump time, and rate, with full/incremental totals:

```text
DUMP REPORT  slot-2026-06-24  (2026-06-24 02:00)
DLE          LVL  ORIG       OUT       COMP%  FILES  TIME    RATE
app01:/home  0    21.47 GB   5.37 GB   25%    1240   12m04s  29.66 MB/s
app01:/etc   1    122.88 kB  40.96 kB  33%    9      1s      …
--
FULL: 1 dle(s), 21.47 GB -> 5.37 GB (25%)
INCR: 1 dle(s), 122.88 kB -> 40.96 kB (33%)
```

`nb report --dump --slot slot-2026-06-21` reports a specific dump. (Sizes come from
each archive's commit footer; the per-DLE timing comes from the run history, so a slot
dumped before this was recorded shows sizes via `nb slot show` instead.)

To push failures to a human, add a `notify:` block (see `nbackup.example.yaml`).
Backends are pluggable — built-in **email (SMTP)** and a generic **webhook**
(Slack/Discord/PagerDuty-compatible):

```yaml
notify:
  on_failure: [email, slack]   # omit to alert every backend
  # on_success: [email]        # see below — dump already notifies on success
  digest: [email]              # for `nb report --notify`
  backends:
    email:
      type: smtp
      host: smtp.example.com
      from: nbackup@example.com
      to: [ops@example.com]
      password_env: SMTP_PASS        # env var name — never the secret itself
    slack:
      type: webhook
      url_env: SLACK_WEBHOOK_URL
```

**What notifies, when:** any command **alerts on failure** by default (every backend,
unless `on_failure` narrows it). A successful **`nb dump`** also notifies by default —
the nightly "backups happened" signal, so a silent inbox means cron didn't run, not
that all is well. Other commands' success is opt-in: list backends in `on_success`
for `sync`/`verify`/`drill`/`prune` (that list then applies to dump too). A dump
notification carries the **full per-DLE dump report** (the `nb report --dump` table),
so the nightly email *is* the full report — not just "it worked".

Secrets are referenced by environment-variable *name* and resolved at send time, so
nothing sensitive lives in the config (a literal `password:` is rejected). A
notification failure — unreachable mail server, missing secret, hung endpoint — is
only ever a stderr warning: it never fails or blocks the backup. So a complete
hands-off cron line is:

```sh
nb dump && nb sync && nb drill --unattended; nb report --notify
```

### Recover (restore a whole DLE, or pick files)

`nb recover` recovers from backups **as they stood on a date**, in two modes.

**Whole-DLE restore (`--all`).** `nb recover --dle X --date D --all --dest out`
rebuilds an entire DLE: it replays the most recent full at or before the date, then
every later incremental up to it, in run order, with GNU tar's incremental
extraction. Because the incrementals carry directory census data, **deletions are
applied** — a file removed between the full and the date is absent after restore —
and extraction **prunes the destination to match the backup**, so `--dest` must be
empty (or pass `--force` to restore into a populated one, replacing its contents).
Omit `--dle` to restore every DLE, each into its own subdirectory of `--dest`.

**File-level recovery (browse + pick).** Without `--all`, recover browses a DLE's
filesystem and pulls back individual files or directories. The browse view merges
the restore chain (the full plus every later incremental up to the date) so each
path shows its newest version on or before the date, recovered from the archive
that holds it. No separate index server is needed — browsing reads the member index
every archive already records, so it touches only the catalog and reaches media only
when you extract.

```bash
nb recover                                   # interactive shell (below)

# one-shot, scriptable:
nb recover --dle app01:/home --date 2026-06-20 --list --path /etc
nb recover --dle app01:/home --date 2026-06-20 \
    --path /etc/hosts --path /etc/nginx --dest /tmp/out
```

A DLE is identified by `host:path` (`app01:/home`); that is what the
tables show and what `--dle`/`setdisk` accept. The interactive shell tracks a
current DLE and date, then navigate and select:

```
recover> setdisk app01:/home
recover> setdate 2026-06-20
recover app01:/home:/> cd etc
recover app01:/home:/etc> ls
  hosts   nginx/   passwd
recover app01:/home:/etc> add hosts nginx
recover app01:/home:/etc> extract /tmp/out
recovered 12 entr(ies) from 2 archive(s) into /tmp/out
```

Paths are relative to the DLE's backed-up root. Selecting a directory pulls its
whole subtree (each file from the archive that last changed it). Unlike a whole-DLE
`--all` restore, selected-file recovery never deletes — it only writes the files you
asked for. One fidelity note: GNU tar records deletions in its snapshot, not the
member index, so a file deleted at a later incremental still shows in the browse
view; recover the *whole* DLE with `--all` when you need deletion-accurate state.

### Verifying and recovery drills

Two layers prove your backups are good, weakest to strongest:

- **`nb verify`** is the atomic integrity check. By default it re-hashes each
  archive's payload against its recorded checksum (corruption detection); it is
  stateless and keyless. `nb verify --deep` adds a **structural** check: it streams
  the archive through the real read pipeline — decrypt → decompress → `tar -t` (list,
  not extract) — and asserts the pipeline completes and the members match the index,
  proving the bytes are a valid *restorable stream* and exercising the key + scheme,
  while still writing nothing.

- **`nb drill`** is the recoverability rehearsal layered on `nb verify`. Checksums
  can't catch a lost key, scheme/tar drift, a broken incremental chain, or an
  unreadable offsite copy — a drill **actually restores** a risk-biased sample of
  DLEs (full + incrementals, deletion-faithful) into a scratch dir and discards it.
  It is NBackup's contribution of the **"0 errors"** digit of [3-2-1-1-0][321].

```bash
nb drill                       # drill the riskiest sample on the landing copy
nb drill --dry-run             # preview: what would be drilled + a posture audit
nb drill --from cloud --tier structural   # routine offsite check
nb drill --tier stock          # restore via the documented gpg/zstd/tar one-liner
nb dump && nb sync && nb drill --unattended; nb report --notify   # hands-off cron line
```

A drill **selects** risk-first: it rotates DLEs so each is drilled within a window,
prioritizes the longest incremental chains and the oldest fulls still relied upon,
and drills a **point-in-time** (`--as-of`), not just the latest slot. Each target is
exercised at a **tier** — `checksum`, `structural`, a real `chain` restore, or
`stock` (the documented one-liner) — and the outcome is appended to an inspectable
**ledger** (`drill-ledger.json`) in the workdir: per DLE its last drill, tier,
source medium, and pass/fail. A failure is **classified** — integrity (corruption),
pipeline (key/scheme), chain (incremental composition), or missing-copy — because
each implies a different fix, and the command **exits non-zero** so it can page you.

Two run modes: **attended** (interactive) may prompt to load a tape; **unattended**
(`--unattended`, the cron mode, auto-detected when stdin is not a terminal) never
prompts and **skips** any target needing a tape swap — a skip is a coverage warning,
not a failure, so a nightly drill stays green while it rotates through the fleet.

Every run also prints a **3-2-1-1-0 recoverability posture audit** — copies, media,
offsite presence, immutability, and 0 errors, plus key-reachable, incremental-state,
and capacity checks. The **immutability** line comes from a WORM probe: NBackup keeps
one fixed probe object on the `--from` medium and checks that deleting it is *refused*
(S3 Object Lock, LTO WORM). NBackup only **detects** immutability — you configure it
operator-side on the storage; least privilege keeps NBackup unable to turn it off.

> Honest limits: an encrypted+compressed archive is all-or-nothing to read (you must
> decrypt+decompress the whole stream to reach late members), so a drill costs the
> full bytes — make routine **offsite** drills the no-write `structural` tier and
> watch the forecast egress the dry-run prints. Drills restore only to scratch and
> never touch real data or the tar snapshot library.

### Pruning (cycle safety)

`nb prune <medium>` deletes by default; pass `--dry-run` (`-n`) to preview.
**Retention is per-medium**, so the medium is named explicitly (`nb prune disk`,
`nb prune offsite`): each store is pruned against its own archives, capacity, and
`minimum_age` — a copy on another medium never makes an archive prunable, because
double storage exists for redundancy. The unit pruning reasons about is the
**archive** (one DLE's image within a slot), not the whole slot, so an old slot can
shed one DLE's image while keeping a slot-mate the chain still needs. Pruning has
two layers:

1. **Safety floor**: an archive is *protected* if it is younger than the medium's
   `minimum_age` (defaults to one cycle), or belongs to its DLE's **live recovery
   chain** — that DLE's last full and *every later incremental* (a whole-DLE restore
   replays them in order, so dropping the tip loses the latest state and dropping a
   middle incremental breaks a climbing-level chain). A recent dump likewise pins the
   older base its restore needs. Only a chain **superseded by a newer full** becomes
   reclaimable; protected archives are never reclaimed. The rule is medium-neutral;
   the archive set it judges is the medium's own.
2. **Capacity reclamation**: among non-protected archives, the medium's retention
   strategy reclaims to fit capacity. Object stores (disk, S3) reclaim **per-archive**,
   deleting the **oldest dead archives until total ≤ capacity**. Tape reclaims **whole
   volumes**: a reel is reused by relabeling it once all its runs are unprotected
   (the oldest-reusable-tape pick, applied when a run needs a volume), so `nb prune`
   never deletes individual archives from a tape.

### Replication / tiered storage

The common operational shape is **land fast, replicate offsite**: dump to local
disk (cheap, fast, online), then mirror committed slots to tape or S3 for the
offsite copy. `nb sync` is the batch form of `nb copy`: it copies every slot the
target medium is missing, **oldest first** (so an interrupted sync makes
contiguous, replayable progress and a full lands before its incrementals).

```bash
nb sync --to lto --dry-run  # preview: what disk has that tape doesn't
nb sync --to lto            # copy the backlog
nb sync --to glacier --last 4   # only the 4 most recent slots
nb sync                     # run every rule in the config's `sync:` block
nb sync --from lto --to disk    # un-vault: restage tape back to disk
```

The source defaults to the landing medium; **`--from` overrides it**, so the same
command both pushes offsite (disk → tape/S3) and pulls back (tape → disk) —
reading a tape source mounts the volume holding each slot, just like a restore.

It **copies by default** (pass `--dry-run`/`-n` to preview) and is **idempotent**:
each slot copies atomically and records a second placement, so re-running resumes
where an interrupted sync left off and a fully-mirrored target reports "up to date".
On a hard error (target full or offline) it stops and reports progress. Declare
recurring targets in the config so a cron line is just `nb dump && nb sync`:

```yaml
sync:
  - to: glacier        # mirror everything to the object store
  - to: lto
    last: 4            # copy only the 4 most recent slots (does not remove older
                       # ones already on tape — `nb prune` trims)
  - from: lto          # second tier: tape -> deep-archive (source need not be landing)
    to: deep-archive
```

Replication and pruning are independent: each medium prunes against its own
retention, so a slot leaves disk when **disk's** capacity and cycle say so — never
merely because a copy reached S3 or tape. Both copies are kept, each retained on its
own terms. To use a cheap offsite tier as bulk retention while disk stays lean, give
disk a tighter `capacity` (or shorter `minimum_age`) than the tier; `nb sync` mirrors
slots offsite and `nb prune` independently trims disk back to its budget.

## Configuration

See [`nbackup.example.yaml`](nbackup.example.yaml). Minimal example:

```yaml
cycle: 7d                            # target & hard-max time between fulls per DLE

# Compression. The default scheme is zstd, which must be on PATH; set `none` or
# `gzip` if zstd is not installed (the scheme binary is checked before a dump).
compress:
  scheme: zstd                        # zstd | gzip | none

# Named storage definitions; `landing` selects which one slots are created on.
# Capacity is per-medium; minimum_age is optional (defaults to one cycle).
media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog   # where slots are written
    capacity: 20TB                   # the space NBackup may use here
landing: disk

# The catalog's own local cache is separate from any
# medium and defaults to ./nbackup-catalog in the working directory. Set `workdir`
# to place it deliberately (e.g. alongside the disk medium above).
# workdir: /var/lib/nbackup/catalog

# Named archiver definitions: an archiver type + its
# content-independent options. An undeclared name is a bare type, so `archiver:
# gnutar` needs no block; most setups need just one.
archivers:
  default:
    type: gnutar
    one-file-system: "true"
    # tar_path: gtar     # GNU tar binary (use "gtar" on macOS/BSD)

# Named dumptypes: an archiver reference + per-DLE policy —
# what to skip (exclude) and encryption. Excludes are a content decision, so they
# live here, not on the archiver; a DLE selects one dumptype.
dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

# The disklist: grouped by dumptype, then host, then paths. The host `localhost`
# is backed up locally; any OTHER host name is a remote SSH client (see the
# `ssh:`/`hosts:` config), so keep it `localhost` on a single machine.
sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /opt/app]
```

- **Media** is a map of named definitions, each with a `type` and type-specific
  parameters; `landing` names the one slots are written to. Adding a medium type is
  a registry registration — no config struct changes.
- **Archivers** are named definitions of the dump program plus its content-
  independent options — the tar binary, `one-file-system`. Most setups need just one;
  an undeclared name is a bare type, so `archiver: gnutar` needs no block. (The
  incremental-state root is a host property — `state_dir` — not an archiver option.)
- **Dumptypes** name an archiver and carry per-DLE policy — what to skip (`exclude`)
  and encryption. Excludes live here, not on the archiver, because skipping logs is a
  decision about the data, not how tar runs. Compression is config-wide.
- **Sources** (the disklist) are grouped by dumptype → host → paths, so each DLE is
  just a path under the dumptype that governs it — all per-DLE tuning lives in the
  dumptype, never on the entry.

### Capacity and retention are per-medium

Each medium declares its **capacity** — the space NBackup may use there. Disk and
cloud spell it directly (`capacity: 20TB`); a tape library derives it as
`bays × volume_size` (`0` = unbounded). Capacity is the headline knob: the planner
uses it — promotion fills free space, pruning reclaims to stay within it.
`minimum_age` is an optional per-medium safety floor (defaults to one cycle) — long
enough that yesterday's run never overwrites a slot still inside the recovery window.

Balancing dumps over time is **not** a medium property — it's a global, temporal
planning concern (an S3 bucket has no meaningful per-run size), so the planner
spreads fulls across the cycle on its own (see [Planning](#planning)). Pruning
consumes only capacity; the reclamation difference (delete a slot vs reclaim a whole
tape) lives in the medium's retention strategy.

### Bandwidth politeness is per-medium

A medium may declare a **throughput** cap — `throughput: 50MB/s` (bytes/sec, the
`/s` is optional; default uncapped). The network analogue of the `nice` CPU
politeness NBackup already applies, it keeps an `nb dump`/`nb sync` from saturating
the office uplink, and a restore/drill download from the same medium honors the same
budget (the cap is symmetric on reads). It is enforced as a token bucket on the
medium-facing stream, back-pressuring the one-pass pipeline without a holding-disk
buffer. Workers writing one medium concurrently **share** the single budget. Set it
on the medium whose link you must protect — typically the cloud or a remote tier.

## Requirements

- **Go 1.25+** to build.
- **GNU tar** at runtime (`tar` on Linux, `gtar` elsewhere; set a `tar_path` option
  on the archiver to override). NBackup checks the binary is GNU tar before running.
- The configured **compressor** on `PATH`: `zstd` (default) or `gzip`; `none`
  needs nothing. NBackup checks it before running. Optional `nice` is used for
  CPU politeness when configured.

## Status & limitations (first version)

Implemented: disk, tape, and cloud (S3/GCS/Azure) Volumes, **copying slots between
media** (`nb copy`, e.g. disk → tape or disk → cloud) with the copy **recorded as a
second placement** so a restore reads from any available copy (and `nb verify` audits
*every* copy, reporting that an intact copy remains when one is damaged), balanced
**multilevel (L0–L9)** planning with a GNU tar snapshot library, immutable
commit-footed archives with **sequence-suffixed** same-day runs, **deletion-aware** incremental
restore, checksum verification, point-in-time restore, per-medium capacity reporting,
cycle-safe pruning, **unattended reporting and alerting** (`nb report`, pluggable
email/webhook notifications), and **remote sources over SSH** (any non-`localhost`
DLE host runs stock tar on the client — no NBackup software or open port there).

### Tape

The `tape` medium comes in shapes that differ in *who changes the tape*:

- A **robotic library** (`dir:` file-backed) has `bays: N` physical positions,
  each a finite `volume_size` tape, and a command moves which bay is mounted.
- A **single drive you change by hand** — the disk-emulated station (`mode:
  manual`), or a real drive (`device:` via `mt`) — shows only the reel currently
  in the drive; the emulated station also lists the other reels on a shelf you
  can load.

When a backup or restore needs a different tape, NBackup **prompts you to swap it
in and waits** (an unattended run errors instead of hanging). Either way you label
a blank tape (`nb label`), inventory a medium with `nb medium <name>` (its bays, or
the drive and shelf), and load a tape with `nb load`. Tapes carry a self-describing
label that NBackup **verifies before every write**, so a foreign or wrong-pool reel
is never clobbered. Relabeling a tape that still holds **protected** slots (within
`minimum_age`, or a DLE's last recovery path — so a slot spanned across tapes
protects every tape it touches) is refused unless you pass `--force`.

### Cloud (object stores)

The `cloud` medium stores slots in an object store via the Go CDK
([gocloud.dev/blob][gocloud]). One type covers many backends — the `url` scheme
selects which:

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1   # or gs://bucket, azblob://container
    # prefix: nbackup/      # optional: confine keys under a folder in the bucket
    capacity: 50TB
```

`s3://` reaches S3 and any S3-compatible store (MinIO, Cloudflare R2, Backblaze
B2, Wasabi); `gs://` is Google Cloud Storage; `azblob://` is Azure Blob.
**Credentials are not in the config** — they come from each SDK's standard
environment (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`). An object store
is **address-identified** like disk: no labels, no swap prompts, nothing to
inventory — it just lands and reclaims slots within its `capacity`. Each archive is
stored as a clean `.tar.<scheme>` object (a plain GET restores it with stock tools)
plus a small header sidecar, so a slot streams disk↔cloud unchanged. (Google Drive
and other file-API stores are out of scope — `gocloud.dev/blob` is an object-store
abstraction.)

`appendable: true` (default) packs many runs per tape; `appendable: false` uses
one run per tape. Restore mounts (robot) or prompts for (manual) whichever tape
holds the copy it needs. A run that **fills a tape mid-write spans onto the next
automatically** — for both `nb dump` and `nb copy`/`nb sync`, splitting even a
single large archive: a robotic library mounts the next writable bay (auto-labeling
a blank), a manual drive prompts for a swap. Spanning is **proactive** — set
`volume_size` so NBackup sizes each chunk to fit *before* writing it (a real drive
with no readable capacity can instead set `part_size`); if a chunk still overflows,
the run fails with a clear message rather than guessing. A restore reassembles a
spanned archive by mounting its tapes in order. (Internals:
[ARCHITECTURE.md](ARCHITECTURE.md).)

### Remote sources over SSH

A DLE's `host` is meaningful: `localhost` (or an empty host) is dumped locally;
**any other host name is a remote client backed up over SSH**. NBackup runs stock
tools (`tar`, and the optional compressor + `gpg`) on the client and streams the
archive back over the connection — there is **no NBackup software, daemon, or open
port on the client**, and intermediate bytes never touch the client's disk.

```yaml
ssh:                              # defaults applied to every remote host
  user: backup
  identity_file: ~/.ssh/nbackup   # a path, not a key — NBackup stores no secret
  options: ["-o", "StrictHostKeyChecking=accept-new"]

hosts:                            # optional: override the defaults per host
  app01:
    ssh:
      port: "2222"
    state_dir: /var/lib/nbackup   # where this host keeps incremental (.snar) state
    archivers:
      gnutar:
        tar_path: /usr/local/bin/gtar

sources:
  default:
    app01: [/home, /etc]          # backed up over SSH; localhost stays local
```

Credentials follow the same no-secrets-in-config rule as cloud and gpg: the key
comes from the operator's ssh agent/config (`identity_file` is a path). Listing a
host under `hosts:` is **only** to override the `ssh:` defaults — it is *not* what
makes a host remote; any non-`localhost` source is remote by default. `nb check`
reaches every source host so you can confirm connectivity before a run.

### Not yet implemented

- **Tape whole-volume reclamation** — capacity-driven pruning already fits
  object stores and disk to their `capacity` (reclaiming the oldest
  non-protected slots first); reclaiming whole *tapes* to fit a library's
  capacity is not yet automatic — tape reuse is identified by `nb plan` and
  gated behind a deliberate `nb label --relabel`.

## Architecture

NBackup's internals are built on a pluggable-API structure: mechanism lives behind
interfaces with named, registered implementations, and one orchestrator (`engine`)
composes them. The **media are the source of truth** (every file self-describing,
every archive committed, every labeled volume identified); the **catalog is a local cache**
with its own directory, so planning, listing, restore-location, and pruning never
touch a slow or offline volume, and a single scan rebuilds it (`nb rebuild`).

Contributors and agents: see **[ARCHITECTURE.md](ARCHITECTURE.md)** for the
package map, the catalog `Entry`/`Placement` model, the design decisions and
their rationale, and the project conventions.

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
```

## License

Copyright © 2026 Niloen AB.

NBackup is free software, licensed under the **GNU General Public License v3.0**.
See [LICENSE](LICENSE) for the full text.

[amanda]: https://www.amanda.org/
[gocloud]: https://gocloud.dev/howto/blob/
[321]: https://www.veeam.com/blog/321-backup-rule.html
