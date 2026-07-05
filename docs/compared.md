---
title: NBackup vs the alternatives
layout: default
nav_order: 9
description: "An honest comparison with restic/Borg/Kopia, Bacula/Bareos, Amanda, and pgBackRest/barman — including when the other tool is the better choice."
---

# NBackup vs the alternatives
{: .no_toc }

An honest map of the field. Backup tools embody real trade-offs, and the tool
that wins on one axis loses on another — so this page recommends the
competitor when the competitor is the better fit. If a row below rules
NBackup out for you, that's the page working.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The short version

NBackup trades storage efficiency for operational transparency: immutable
daily runs of ordinary tar archives that restore with stock tools, first-class
tape, and scheduled drills that *prove* restorability. If your deciding factor
is deduplication ratios, Windows clients, or point-in-time database recovery,
one of the tools below serves you better.

| | NBackup | restic / Borg / Kopia | Bacula / Bareos | pgBackRest |
|---|---|---|---|---|
| Restore without the tool | **yes** — stock `tar`/`zstd`/`gpg` | no — needs the tool + intact repository | no — `bextract`/`bls` are still Bacula tools | no — needs pgBackRest + its repository |
| Cross-backup dedup | no | **yes** — content-addressed chunks | no (core) | no (block-level incrementals, not dedup) |
| Tape | **first-class** — changers, spanning, label rotation | no — repositories need random access | **first-class** | no |
| Point-in-time recovery | no — run-granular | no — snapshot-granular | no — job-granular | **yes** — WAL replay to any moment |
| Scheduled restore drills | **yes** — `nb drill`, risk-biased, classified failures | checksum / repo checks | verify jobs | backup verification |
| Daemons required | none — one binary + cron | none | director + storage + file daemons + SQL database | none |
| Windows sources | no — Unix over SSH only | varies (restic, Kopia: yes) | **yes** | n/a (PostgreSQL only) |
| Catalog / index dependence | rebuildable cache — media are the source of truth | repository index is load-bearing | SQL catalog is load-bearing (`bscan` recovery is slow) | repository + WAL archive are load-bearing |

Cells are blunt by necessity; the nuance — including where each "no" is a
deliberate design decision rather than a gap — is in the sections below.

## restic, Borg, Kopia — the chunk-store generation

These are excellent tools with large, active communities, and for many
workloads they are the right choice. They slice data into content-addressed
chunks and deduplicate across every backup and often every machine, so **they
win decisively on storage efficiency**: fifty similar servers, or ninety daily
snapshots of slowly-changing data, may occupy a small fraction of what NBackup
would store — and NBackup's own `nb plan` cost forecast will honestly show you
the difference. Repository encryption is built into their formats, and dense
snapshot histories are cheap, so "keep everything for a year" is a realistic
policy rather than a capacity bill.

What you trade for that efficiency is exactly what NBackup refuses to trade:

- **Every restore needs the tool and an intact repository.** A chunk store is
  reassembled from a database of blocks; you cannot `ls` it and understand it,
  and a damaged index or subtle repository corruption puts *every* backup at
  risk at once. NBackup's runs are independent, self-describing files — damage
  stays local to one archive, and a full restores with one pipe
  (`zstd -dc payload.tar.zst | tar -x`) from any rescue shell, even if NBackup
  and its config are long gone. If "still restorable in fifteen years with
  whatever tools that decade ships" is a requirement, ordinary tar is the
  conservative bet.
- **No tape.** Chunk repositories require random-access storage — the restic
  project has said as much when tape support comes up — so a tape library,
  robot or hand-fed, is out of scope for this generation. NBackup treats tape
  as a first-class target: changers via `mtx`, spanning, label rotation,
  labels verified before every write.
- **Verification stops at checksums.** Repository checks prove the bytes are
  intact; they don't prove *you can restore* — a lost key, tool drift, or a
  broken incremental chain all pass a checksum. `nb drill` actually restores a
  risk-biased sample on a schedule, classifies each failure (integrity, key,
  chain, missing copy), audits your 3-2-1-1-0 posture, and exits non-zero so
  it can page you.
- **Planning and retention are per-medium and capacity-driven.** NBackup
  balances fulls and incrementals against a stated capacity and retains each
  copy on its own medium's terms — the offsite tape keeps a run after the
  local disk has pruned it. Chunk-store retention is snapshot-count/age policy
  against one repository.

**Pick restic, Borg, or Kopia** when you back up many similar machines, a
laptop fleet, or need dense snapshot histories on a tight storage budget —
that's their home turf, full stop. **Pick NBackup** when transparency, tape,
longevity, and proven restorability matter more than squeezing bytes today.

## Bacula / Bareos — the enterprise tape generation

Bacula and its fork Bareos share NBackup's lineage-era virtues: real tape
support (changers, pools, label management), scheduled fulls and
incrementals, and decades of production hardening. They also do things NBackup
doesn't: **Windows clients** with first-class file daemons, fine-grained
enterprise features (job hierarchies, copy/migration jobs, ACLs, plugins for
VMware and databases), and they scale to deployments — hundreds of clients,
multiple storage daemons — that a one-binary tool doesn't aim at.

The structural difference is machinery. A Bacula/Bareos installation is a
**daemon constellation**: a director, one or more storage daemons, a file
daemon on every client, and a SQL catalog (PostgreSQL, MySQL, or SQLite) that
all of them depend on. That catalog is **load-bearing**: it holds the file
indexes and volume records restores need, it must itself be backed up, and
rebuilding it from volumes with `bscan` is a documented but slow disaster
procedure. NBackup inverts this: **one static binary driven by cron**, clients
need only `sshd` and stock `tar` (no agent, no open port), and the catalog is
a **disposable cache** — the media are the source of truth, every file
self-describing, and one scan (`nb rebuild`) reconstructs the catalog from
them. Volume formats differ the same way: Bacula volumes interleave jobs in
its own block format read by its own tools; an NBackup volume is a sequence of
plain tar archives.

**Pick Bacula or Bareos** if you have Windows sources, need their enterprise
feature depth, or run at a scale where dedicated storage daemons and a real
DBA earn their keep. **Pick NBackup** if you want the same tape-era operational
discipline as one binary and a crontab, with nothing precious to operate but
the backups themselves.

## Amanda

Amanda is NBackup's direct ancestor, and the comparison is the friendliest on
this page: same philosophy — balanced multilevel scheduling, immutable daily
artifacts, cycle safety, GNU tar underneath — modernized. Object storage is a
peer of tape rather than a bolt-on, clients are reached over plain SSH instead
of `amandad`, the whole system is one static binary with no server/client
package split, and `nb drill` proves restores in a way Amanda never attempted.
If you run `amdump` today, you already know most of NBackup;
[Migrating from Amanda](migrating-from-amanda) maps your vocabulary
(disklist, dumptype, tapecycle, the `am*` commands) onto it and is honest
about what changed and what isn't there.

## pgBackRest / barman — PostgreSQL specialists

For a dedicated PostgreSQL estate, these are the right tools, and this section
won't pretend otherwise. Both are built around **WAL archiving**, which buys
the one thing NBackup structurally does not have: **point-in-time recovery**.
pgBackRest replays WAL to any moment — a timestamp, an LSN, a transaction id —
so "restore to 14:32, just before the bad `DELETE`" is a supported operation.
It adds delta restore (re-copying only files that changed), backup from a
standby to unload the primary, parallel everything, and deep
PostgreSQL-specific validation; barman covers similar ground with its own
operational style. **NBackup's recovery points are run-granular**: you get the
cluster as of a nightly (or however often you dump) run, and nothing in
between. Say it plainly: if losing up to a day of database changes is
unacceptable, use pgBackRest or barman — or run one of them *alongside*
NBackup for the databases that need PITR.

What NBackup's [`postgres` archiver](features/archivers#postgres--live-postgresql-clusters)
offers instead is **one system**: the databases ride in the *same* nightly run
as the filesystems — the same balanced planning, the same media and offsite
`nb sync`, the same retention, and the same `nb drill` rehearsals that prove
the backups restore. Level 0 is a streamed `pg_basebackup`, level N uses
PostgreSQL 17's native incremental base backups (block deltas — small nightly
increments without operating a WAL spool), and a restore merges the chain with
`pg_combinebackup` — the database's own tools stay authoritative. Backups
know their tables: `nb recover --inventory` lists them with sizes as of any
date, and pointing `--path` at a table exports it as ready-to-import `pg_dump`
SQL via a scratch restore, no live server touched. Note the floor: the
archiver **requires PostgreSQL 17+**; older clusters use the `pipe` archiver
with `pg_dump` — full backups only — per the
[PostgreSQL 16 and older recipe](reference/configuration#postgresql-16-and-older).

**Pick pgBackRest or barman** when PostgreSQL is the estate and PITR is a
requirement. **Pick NBackup's postgres archiver** when the database is one
source among your filesystems and run-granular recovery points are
acceptable — one config, one cron line, one drill ledger covering everything.

## rsync / rclone + snapshots — "I just script it"

A hand-rolled `rsync --link-dest` or `rclone sync` loop is a fine start, and
plenty of small setups never need more. What the script doesn't give you is
the management layer, which is most of what NBackup is: estimate-driven
**levels** with fulls balanced across the cycle instead of a full every time
(or never); a **retention safety floor** that refuses to delete the last
recovery path, rather than a `find -mtime -delete` that will happily do so;
**verification and drills** that detect silent corruption and prove restores
before you need one; a **catalog** that answers "what did we have on June
20th, and which medium holds it?"; and tape, spanning, encryption, and
per-medium capacity handled rather than hand-waved. The artifacts stay as
transparent as the script's — ordinary tar files you can read — so you give up
none of the "I can see my backups" property that made you script it in the
first place.

---

Next: [Rationale](rationale) for the design philosophy behind these
trade-offs, or [Getting Started](getting-started) to try it.
