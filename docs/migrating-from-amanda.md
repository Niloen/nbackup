---
title: Migrating from Amanda
layout: default
nav_order: 8
description: "For Amanda operators: a concept map (disklist, dumptype, tapecycle, amdump…), what carries over, what changed, what's missing, and how to run both during a transition."
---

# Migrating from Amanda
{: .no_toc }

NBackup descends from Amanda. If you run `amdump` today, you already know most
of NBackup — this page maps your vocabulary onto it, and is honest about what
changed and what isn't there.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The concept map

| Amanda | NBackup | Nuance |
|---|---|---|
| `disklist` / DLE | `sources:` block; **DLE** survives as the term | The disklist lives inside the one config file, grouped **dumptype → host → paths**. A DLE is still a `host` + `path` (`app01:/home`). |
| `dumptype` | `dumptypes:` | Same role — per-DLE policy. Excludes and encryption live here; **compression is a config-wide default** a dumptype may override wholesale (no per-field merge). |
| `dumpcycle` | `cycle:` | Same meaning: the target — and hard maximum — time between fulls of each DLE. It also defines the window retention protects. |
| `tapecycle` / `runtapes` | per-medium `capacity:` + automatic label rotation | The philosophy shift — see [Capacity, not counts](#capacity-not-counts) below. You never state a tape count to cycle through. |
| `holdingdisk` | a medium with `holding: true` | Same job: dumps land on it in parallel, a drainer feeds the tape at disk speed, `capacity` back-pressures the dumpers. See [Holding disk](features/holding-disk). |
| `amdump` | `nb dump` | One planner execution = one **run**, an immutable artifact named `run-YYYY-MM-DD.HHMMSS`. |
| `amcheck` | `nb check` | Verifies the config, the tool chain, and reaches every source host before the night's run. |
| `amreport` | `nb report` | `nb report` summarizes recent history plus a drill-coverage audit; `nb report --dump` prints the classic per-DLE dump report (level, orig/out size, compression %, rate). |
| `amrecover` | `nb recover` | The interactive shell is deliberately familiar: `setdisk`, `setdate`, `ls`, `cd`, `add`, `extract`. No index server — browsing reads the member index each archive already carries. |
| `amflush` | `nb flush` | Drains a holding disk's un-flushed archives to the landing. The next `nb dump` also auto-drains, so `nb flush` is the explicit form. |
| `amlabel` | `nb label` | Same contract: a tape must be labeled before its first write, and the label is verified before every write so a foreign reel is never clobbered. `nb label --relabel` is the manual early-recycle. |
| `amtape` | `nb medium` / `nb load` | `nb medium <name>` inventories a changer (drives + slots + barcodes); `nb load` loads a bay by id or `--label`. |
| `amcheckdump` / `amverify` | `nb verify` | Checksum verification, plus `--deep` for a structural decode. `nb drill` goes further than Amanda ever did — see below. |
| `amreindex` | `nb rebuild` | Same idea, wider reach: `amreindex` regenerates browse indexes from the volumes a run or a dump at a time; one `nb rebuild` scan reconstructs the whole catalog — indexes, dump history, and placements. |
| taper | the **drainer** and the **landing** | There is no taper process. Runs are written to the `landing:` medium; with a holding disk, one drainer copies finished archives to it. The planner is medium-neutral — it never knows tape from S3. |
| `amanda.conf` + `disklist` | one `nbackup.yaml` | Everything — media, dumptypes, sources, sync rules, notification — in a single file. Unknown keys are rejected, so typos fail loudly. See the [Configuration reference](reference/configuration). |
| `bumppercent` / `bumpdays` | `bump_percent` + the bump rule | Same intent, one knob (default 5%). A DLE sits at level 1 and climbs only after holding its level a couple of runs *and* when climbing saves ≥ `bump_percent` of the full — so chains stay short. See [Planning](features/planning). |
| tape spanning / `tape_splitsize`, tapetype `length` | automatic spanning + `volume_size` | A run that fills a tape mid-write spans onto the next automatically, splitting even a single archive. `volume_size` is the declared per-cartridge capacity (Amanda's tapetype `length`, real drives included): NBackup tracks each tape's fill in its catalog and sizes chunks to fit *before* writing — no `tape_splitsize`/part-cache tuning. Declare it a little below native capacity; `part_size` is an optional extra bound. |
| `ampgsql` | the `postgres` archiver | Same slot in the design, different mechanism: PostgreSQL 17+ native incremental base backups (`pg_basebackup --incremental`) instead of WAL shipping — no `archive_command` spool to operate. **Requires PostgreSQL 17+**; for older clusters see [PostgreSQL 16 and older](reference/configuration#postgresql-16-and-older). |
| client + `amandad` | remote sources over SSH — **no client software** | A headline improvement. Any non-`localhost` DLE host is backed up over SSH running stock `tar` (and optionally the compressor + `gpg`) on the client. No `amandad`, no `bsdtcp`/`bsdudp` auth negotiation, no open port beyond `sshd`. See [Remote sources](features/remote-sources). |
| `inparallel` | `parallelism: { workers: N }` | Same knob, same advice: keep `workers × compressor threads ≤ cores`. |
| `netusage` | per-medium `throughput:` | A token-bucket cap on the medium-facing stream (e.g. `throughput: 50MB/s` on the cloud medium), symmetric on reads. |

### Capacity, not counts

This is the one real philosophy shift. Amanda rotates through a fixed count of
tapes (`tapecycle`) and you size the count to your retention. NBackup asks for
one number per medium — its **capacity** (disk and cloud spell it directly;
a changer derives it as `slots × volume_size`) — and the planner chooses levels,
full frequency, and retention to fit it. Tape recycling still works exactly like
Amanda's tapecycle underneath: when a run needs a fresh volume and no blank is
loaded, NBackup reuses the **oldest tape whose every run is past protection**,
announcing which tape it wants. The difference is that the protection floor is
derived from the `cycle` and each medium's `minimum_age`, not from a count you
keep in your head — and if every tape still holds a protected run, the run
**fails loudly** rather than overwriting one. See
[Pruning & retention](features/pruning) and [Rationale](rationale).

## What you keep

The operational properties you trusted Amanda for are the point of NBackup:

- **Balanced multilevel scheduling.** Levels 0–9, estimate-driven, fulls spread
  across the cycle automatically — you still never hand-schedule a full, and
  there are no balancing knobs left to tune: promotion is automatic.
- **Immutable daily artifacts.** One run per execution, never overwritten, id
  never reused.
- **Cycle safety.** Yesterday's run can never overwrite a backup still inside
  the recovery window; recoverability outranks capacity, always.
- **Tape as a first-class target.** Changers (via `mtx`), single hand-fed
  drives, barcodes, labels verified before every write, spanning, label
  rotation. Not a legacy bolt-on.
- **GNU tar underneath.** Archives are GNU tar in listed-incremental format,
  piped through a stock compressor — the same dump program your Amanda
  `GNUTAR` dumptypes used, so your restore knowledge transfers. Where you
  reached for `amrestore | tar -x`, the NBackup equivalent is one pipe:
  `zstd -dc <payload> | tar -xf -` (see [Restore by hand](restore-by-hand)).
- **The holding disk**, doing exactly what Amanda's does.

## What's new or different

- **Object storage is a peer of tape.** S3 (and compatibles), GCS, and Azure
  Blob are deployment models, not adapters; the same run streams disk ↔ cloud
  ↔ tape unchanged, and "land fast, replicate offsite" is one command
  (`nb sync`). See [Replication](features/replication).
- **Capacity replaces tape counts** — see above.
- **Drills.** Amanda verifies tapes; it never proves a *restore*. `nb drill`
  actually restores a risk-biased sample (full + incrementals,
  deletion-faithful) into scratch on a schedule, classifies failures, audits
  your 3-2-1-1-0 posture, and pages you. See
  [Verification & drills](features/verification).
- **Cost forecasting.** `nb plan` prints the storage `$/month` of cloud media
  and the marginal cost of the next run, fully offline. See
  [Cost forecasting](features/cost).
- **One static binary + cron.** No `amandad`, no `xinetd`, no server/client
  package split. Clients need only `sshd` and `tar`; scheduling is your
  crontab: `nb dump && nb sync && nb prune && nb drill --unattended; nb report --notify`.
- **The catalog rebuilds itself.** Both systems keep the media self-describing
  — Amanda's labeled tapes and dump headers mean `amrestore` restores with the
  catalog gone, and NBackup inherits exactly that design. The difference is the
  road back: Amanda regenerates browse indexes from the tapes with
  `amreindex` (a run or a dump at a time), but `curinfo` (planning history)
  and `tapelist` (overwrite safety) have no rebuild — lose them and you
  reconstruct by hand. NBackup's one `nb rebuild` scan reconstructs the whole
  catalog: dump history, placements, and browse indexes together. See
  [Concepts](concepts#the-catalog-is-a-cache).

## What's not there

Honesty over faithfulness — deliberate omissions, so you can rule NBackup out
quickly if one is a dealbreaker:

- **No `dump(8)`/`xfsdump`.** GNU tar is the filesystem archiver. ext4 `dump`
  is effectively unmaintained and was skipped despite the Amanda pedigree.
- **No Windows clients.** Unix sources over SSH only.
- **No deduplication and no chunk store** — by design; see
  [Rationale](rationale#non-goals). If storage efficiency is your deciding
  factor, a chunk-store tool serves you better.
- **No storage-class lifecycle modeling** (Glacier / Deep Archive transitions).
  Which tier bytes sit in is configured operator-side.
- **Fewer application agents than Amanda's script ecosystem.** Today: `gnutar`,
  `postgres` (17+), and the generic `pipe` archiver (Amanda's `amraw`/script-API
  analog — your own producer/consumer commands). MySQL and ZFS send/recv are
  on the roadmap, not shipped.
- **No WAL-shipping mode for PostgreSQL.** The `postgres` archiver uses PG17+
  native incremental base backups, not `ampgsql`'s `archive_command` model —
  and therefore **requires PostgreSQL 17+**. Older clusters use the `pipe`
  archiver with `pg_dump` — full backups only; see
  [PostgreSQL 16 and older](reference/configuration#postgresql-16-and-older).
- **No import of Amanda's existing backups or history.** See next section.

## Running both during a transition

NBackup alongside Amanda on the same hosts is safe:

- **State is separate.** NBackup's GNU tar listed-incremental (`.snar`)
  snapshots live under its own `state_dir` (see the
  [configuration reference](reference/configuration#incremental-state));
  it never reads or writes Amanda's `gnutar-lists`. Reading the same source
  paths concurrently is fine — both are readers.
- **Start small.** Point NBackup at one host or one DLE, run it against its own
  medium for a cycle or two, drill it (`nb drill`), and widen from there. Day
  one fulls everything; promotion staggers the fulls apart over the next cycle.
- **Old Amanda backups stay where they are.** There is no import: NBackup
  starts a fresh backup history, and your existing Amanda tapes remain
  restorable with `amrestore`/`tar` exactly as before. Keep the Amanda install
  (or at least its tapes and a note on the restore procedure) until the last
  Amanda backup you'd ever restore has aged out of your retention.

---

Next: [Getting Started](getting-started) to install and run a first backup, or
pick the [Scenario](scenarios) closest to your Amanda setup — a
[tape library](scenarios/tape-library), a
[holding disk feeding a drive](scenarios/tape-holding-disk), or
[remote hosts over SSH](scenarios/remote-hosts).
