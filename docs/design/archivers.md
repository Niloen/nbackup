# Archivers beyond tar — the plugin roadmap and the neutrality it demands

Status: phase 1 implemented (neutrality moves + the `pipe` archiver);
phase 2 implemented (the `postgres` archiver, `mode: incr` — see "The
postgres archiver as built", which records where the build deviated from
the original sketch below and why).

GNU tar is today the only `Archiver`. This doc records which archivers are
worth adding, what the generic layers must stop assuming so a non-tar archiver
fits, how "members" generalize when the backup isn't a file tree, how database
incrementals map onto the existing level machinery, and where connection
details and credentials live in the config.

## Which archivers, by value

1. **`pipe` — a generic command archiver** (Amanda's `amraw`/script-API
   analog). A user-configured producer command emits the stream; a consumer
   command restores it. Full-only, no member list, no offsets. Highest value
   per effort — it covers sqlite `.backup`, LVM snapshots, one-off application
   dumps — and, more importantly, it is the cheapest second archiver: it forces
   exactly the minimal API generalization with no speculative machinery.
   **Built in phase 1.**
2. **PostgreSQL** — the flagship database archiver. Amanda-faithful precedent
   is `ampgsql`: level 0 = base backup, level ≥1 = the WAL segments accrued
   since the previous dump (see "Database incrementals" below). `pg_basebackup
   -Ft` emits tar, so the full's member listing can even be real. PG17+ native
   incremental base backups are an alternative level model that also fits
   Level/BaseLevel; the choice lives entirely inside the archiver (`mode:`).
3. **MySQL/MariaDB** — `mysqldump` (logical, full-only: `HasBase` always
   false, so the planner schedules fulls forever — no new concept) and
   `mariabackup` (physical, LSN-based incrementals mapping directly onto
   Level/BaseLevel).
4. **ZFS send/recv** (Amanda: `amzfs-sendrecv`) — snapshot-pair incrementals.
   The retained snapshot *is* the incremental state: `Promote` = retag the
   snapshot, `HasBase` = "does the level-N snapshot exist".
5. **dump(8)/xfsdump — skipped despite the Amanda pedigree.** ext4 `dump` is
   effectively unmaintained and xfsdump niche; it fits every existing seam
   (it is tree-shaped) but earns almost nothing. Honesty over faithfulness.

## The neutrality moves (phase 1)

A full-tree survey found the levels/chain/promote/`HasBase` spine already
archiver-neutral; nearly every remaining coupling descends from one baked
assumption — *a `record.Member` is a filesystem path forming a tree* — plus a
handful of literal hardcodes. The interface's own precedent for fixing these
is `SpliceTrailer`: a capability the archiver declares, with a graceful
generic-layer fallback. Phase 1 extends that pattern:

1. **`List` is a declared capability** (`CanList()`). A pipe archiver (or
   `zfs send`) cannot enumerate members. Structural verify and the structural
   drill tier degrade to checksum verification when listing isn't offered —
   the same class of fallback ranged reads already take for `Off = -1`.
2. **The stock-recovery recipe moves into the archiver**
   (`StockExtract(dest)`). The drill stock tier hardcoded
   `tar --extract --listed-incremental=/dev/null …` in the *generic* engine;
   that is a per-format fact if anything is (postgres: `pg_restore`/WAL
   replay; pipe: the consumer command). The stock tier gates on it.
3. **`BackupRequest.SourcePath` → `Source`, opaque.** Directory for tar,
   database name for postgres, dataset for zfs, whatever the producer command
   wants for pipe. The two generic peeks inside it move behind the archiver:
   the `nb check` `test -r` readability probe becomes `Archiver.CheckSource`
   (postgres will probe connectivity instead), and `DLE.Name()` slugging stays
   as-is (db names and datasets are tamer than paths; a map-form source entry
   with an explicit `name:` is the deferred escape hatch if a DSN-shaped
   source ever appears).
4. **The restore destination is opaque and its policy archiver-owned.** The
   restorer refused a non-empty destination *because GNU tar incremental
   extraction deletes*; that is tar's deletion model enshrined as generic
   policy. The archiver knows whether its chain replay is destructive.
5. **The payload filename extension is archiver-supplied.** The fslike media
   layer baked `.tar`/`.tar.gz` into payload names for every archiver. Naming
   only (the header sidecar is authoritative), but the name asserted "this is
   a tar". The extension is recorded per-archive like the scheme, so restore
   needs no config.

Deliberately *not* changed: `BackupRequest{DLE, Level, BaseLevel}`, `HasBase`,
`BackupSource`'s Finish/Promote/Cleanup contract, planner/chain arithmetic,
`Estimate`'s role. The `.new`-then-`Promote` state model generalizes untouched:
gnutar promotes a `.snar`, postgres will promote a WAL-position marker, zfs a
snapshot.

### Restore-side archiver resolution goes through the DLE

A sixth move the pipe build forced: the archive records its producing archiver's
*type* ("how to reverse the stream"), but a type's options live in a config
definition — and for pipe the `restore_command` option is load-bearing to
extraction, where gnutar's flags are not. The recorded type alone cannot name
the definition (a config may hold several pipe definitions). So every
restore-side resolution (verify's structural list, the chain restore, ranged
selection, the drill's stock tail) carries the DLE, and the toolchain resolves
through the DLE's dumptype → archiver definition when the DLE is still
configured and still maps to that type — the same config-is-source-of-truth
rule decrypt keys already follow (`decryptOptsFor`). A DLE dropped from the
config (or remapped to another type) falls back to the bare type with default
options: fine for gnutar, and an archiver whose options are load-bearing then
errors naming the missing option. Rejected: recording the definition name or
the commands in the artifact — executing config-shaped commands recorded years
ago is exactly the drift the config-resolves-at-restore rule avoids.

## Members when the backup isn't files

Reframed doc, unchanged type: a member is a **named restorable unit in stream
order**; "slash-separated paths forming a tree, directories with a trailing
slash" is the tar/dump specialization. `Member{Path, Off}` already carries the
escape hatches (`Off = -1`, nothing structurally requires slashes).

- Physical DB backup (basebackup, mariabackup): members are files again.
- Logical dump in a structured format: `pg_dump -Fc` has a real TOC
  (`pg_restore -l`) and `-L` does selective restore by entry — so members =
  tables/schema objects, and `RestoreStage(dest, members)` already has the
  right shape for "restore just these tables". Member *selection*
  generalizes; only *tree reconstruction* doesn't. A flat namespace browses
  fine; the trailing-slash convention simply never fires.
- Log increments: members = WAL/binlog segment files. Flat, ordered.
- pipe/zfs: no members. `FileCount` degrades to zero, harmlessly.

`nb mount` and the recovery browse tree stay tree-capability consumers:
present for tar/basebackup, absent (or one opaque file) for logical DB dumps.
The capability gate this paragraph deferred is now decided — see neutrality
moves 7–8 under "The postgres archiver as built": the assembler makes
incremental chains browse correctly, and the unit inventory makes them legible.

## Database incrementals — the transaction log, and it is Amanda-faithful

The `ampgsql` model maps onto the existing machinery directly:

- **Level 0** = base backup. **Level N≥1** = the WAL/binlog segments accrued
  since the level-(N−1) dump.
- **The archiver's incremental state** = the log-position marker, keyed by
  (DLE, level) exactly like a `.snar`: written to a side file during the dump,
  `Promote`d only after the archive commits. A killed dump never advances the
  position; a retry re-bundles the same segments.
- **`HasBase(dle, level)`** = "do I hold the marker from a committed level-N
  dump". A missing/corrupt marker forces a full, as an empty `.snar` does.
- **Chain restore** = restore the base, then replay each level's segments
  ascending — precisely what the existing chain loop does: one `RestoreStage`
  per step in level order. Where gnutar's step applies deletions, postgres's
  feeds segments to WAL replay. The generic loop does not change.

Design notes for the postgres build:

- **WAL spooling.** Postgres pushes segments continuously via
  `archive_command`; the archiver bundles at dump time whatever accrued since
  the last promote. The spool is payload-sized but conceptually the same
  "non-derivable data the next incremental builds on", so it lives under the
  host `state_dir`, engine-namespaced as usual. `nb check` verifies
  `archive_mode`/`archive_command` and prints the exact line to configure.
- **Point-in-time recovery** falls out (replay stops at a timestamp) and is
  the DB analog of selected-file recovery — a later extension of the
  `members` parameter, not designed now.
- Streams like `pg_dump -Fc` and `zfs send -c` are already compressed; config
  validation should nudge such dumptypes toward `compress: {scheme: none}`.

## Config model

The `archivers:` / `dumptypes:` / `sources:` split (Amanda's application /
dumptype / disklist) already fits; only the *interpretation* of the source
string changes (archiver-owned). The reusability question — where do
connection details live so one archiver definition serves many hosts — is
answered by an existing concept: the `hosts:` block already carries per-host
archiver-*type* option overrides (`hosts: app01: archivers: gnutar:
{tar_path: …}`), merged over the named archiver's options exactly as per-host
`ssh:` fields merge over the global `ssh:` block. Connection details are the
postgres analog of `tar_path`. The split rule:

- **Archiver options** = strategy and tool behavior: `mode`, format flags,
  binary-path defaults.
- **Host archiver overrides** = locality: port, socket, user, binary path on
  that box.
- **Source string** = which object: path, database name, dataset.

```yaml
archivers:
  pg: { type: postgres, mode: wal }
dumptypes:
  databases: { archiver: pg }
hosts:
  db01: { archivers: { postgres: { port: "5432", user: backup } } }
  db02: { archivers: { postgres: { socket: /var/run/postgresql } } }
sources:
  databases:
    db01: [app_prod, analytics]
    db02: [legacy]
```

Two clusters on one host is the awkward case; a second named archiver
(`pg-legacy: {type: postgres, port: "5433"}`) covers it and is no longer a
smell — it *is* a different service. Rejected: a separate "service"/"endpoint"
concept — it would duplicate what `hosts:` + named archivers express and give
every DLE a third coordinate.

## Credentials

Secrets never live in the config file (existing doctrine: notify rejects a
literal `password:`; cloud creds come from SDK env; gpg from the keyring; SSH
from the agent). The DB ladder, in order of preference:

1. **No credential at all — OS-level auth on the client.** Remote DLEs run
   their tools over SSH on the source host, so the SSH key is already the
   authenticated identity; from there, peer auth over the local socket
   (postgres `peer`, MySQL `auth_socket`) costs nothing. This is the
   recommended setup and the one `nb check`'s hint prints, including the
   one-time role grant (`CREATE ROLE backup LOGIN REPLICATION` for WAL mode).
2. **Tool-native credential files, referenced by path** (Amanda's
   `PG-PASSFILE`): `passfile:` / `service:` (postgres), `login_path:` /
   `defaults_file:` (mysql). Config stores the path; the file lives on the
   client at 0600 and the tool enforces permissions.
3. **Env-var indirection, `password_env`** (same key and semantics as the
   SMTP backend): resolved at spawn time on the *server*, injected into the
   child's environment across the SSH session — encrypted in transit, never
   on a command line (`ps` leaks) or the client's disk.
4. **Rejected: any literal secret key.** `password:`/`token:` in an archiver
   or host block fail validation as unknown fields, pointing at
   `passfile`/`password_env`.

The restore host needs credentials too (the target database may be
elsewhere); restore auth follows the same ladder on whatever host runs the
restore, mirroring the gpg "key must be in the keyring on the restoring host"
story. The drill stock tier assumes mechanism 1 or 2 — a stock recovery
one-liner must not depend on an NBackup-resolved env var. `nb check` proves
auth by actually connecting (`SELECT 1`) as the configured identity. The pipe
archiver stays out of all of this: its commands are user-authored.

## The postgres archiver as built (phase 2, `mode: incr`)

The build deviated from the sketch above in three deliberate ways, each a
user decision recorded here so the sketch's `mode: wal` plan is read as
history, not intent.

### PG17 incremental base backups, not the WAL spool

`mode: incr` shipped first: level 0 = `pg_basebackup --format=tar -D -`
(streamed to stdout), level N = the same with `--incremental=<manifest>`,
restore = `pg_combinebackup`. The `ampgsql`-faithful `mode: wal` remains a
possible later mode, but it was passed over because its incremental state is
an external, payload-sized WAL spool that `archive_command` must feed
continuously — machinery outside the dump window. The PG17 model's state is
just the **backup manifest**: a small file teed out of the dump's own tar
stream in flight (`backup_manifest` rides inside it) and promoted
`.new`-then-rename exactly like a `.snar`. Nothing is staged on backup; the
manifests are the only files the archiver keeps on the host. `nb check`
verifies connectivity (`SELECT 1`) and `summarize_wal = on` (printing the
`ALTER SYSTEM` line), instead of an `archive_command` recipe.

One format fact the integration test pinned down (cross-validating against
`pg_combinebackup` byte-for-byte): an `INCREMENTAL.<name>` file pads its
header (magic, block count, truncation length, block-number array) with
zeros to the next BLCKSZ boundary before the block data — but only when it
carries blocks; a zero-block stub is the bare 12-byte header.

### Connection details and credentials: libpq's own config, not ours

The `hosts:` connection-override model and credential rungs 2–3 sketched
above were **not built**. The DLE source string is a libpq connection
reference (`app_prod`, `service=legacy`, `host=/run/postgresql
dbname=app`), and authentication is entirely the client's own libpq
configuration: peer auth as the executor identity, `~/.pgpass`,
`~/.pg_service.conf`. That is the ssh-agent/gpg-keyring doctrine applied to
the database — libpq already is the credential resolver, so NBackup adds
none. The archiver's options are just `mode` and `bin_dir`; there is no
`password_env`, no `Cmd.Env`, and locality lives in the client's service
file rather than a `hosts:` block. `nb check` proves the whole chain by
connecting as the configured identity.

### Two more neutrality moves (7 and 8)

The combine-shaped restore and the delta-shaped members forced two further
declared capabilities, both following the SpliceTrailer pattern (declared by
the archiver, graceful generic default):

7. **Gather-then-combine restore** (`RestoreIsCombine` + `CombineStage`).
   A postgres chain cannot be replayed additively: `pg_combinebackup` is an
   N-input merge needing every level on disk simultaneously. The restorer
   stages each level's `RestoreStage` into `dest/.nb-combine/L<n>` — inside
   the destination, so one filesystem (the combine's `--copy-file-range`
   can reflink) and covered by the existing empty-dest guard and rollback —
   then runs `CombineStage` once. Default (gnutar, pipe): the additive
   replay, unchanged.
8. **Archiver-owned member assembly** (`Assembler`: `Logical` + `Assemble`),
   plus the **unit inventory** (`record.Unit`). The browse tree
   (`recovery.Tree`, and with it `nb recover`'s selection and `nb mount`)
   was a most-recent-wins union — wrong for a chain whose newest version of
   a changed relation file is an `INCREMENTAL.<name>` block delta (a mounted
   chain would show a stale full beside a garbage delta). With an assembler
   the tree keys nodes on the LOGICAL path (`Logical` folds the delta name),
   keeps each node's chain versions, takes the newest level as the census
   (such archivers enumerate every live file per level, so deletions fall
   out — the union caveat does not apply), and browse-time reads fetch each
   version and run `Assemble` (~100 lines of block splicing, cross-validated
   byte-for-byte against `pg_combinebackup`). Whole-DLE restore still runs
   `pg_combinebackup` itself — the database's own tool stays authoritative;
   the assembler is the mount/browse/selection read path.

   The *legibility* half went through a deliberate revision. The first build
   recorded a per-member alias path and grafted `tables/…` symlinks into the
   tree/mount — rejected after review: a downloaded heap file is an
   attractive nuisance (raw 8k pages, no catalogs, no rows — a dead end at
   the worst moment), and file→thing is the wrong cardinality anyway (one
   thing spans many files; one file can serve many things; a thing needn't
   be file-shaped at all — a logical dump's TOC entry, a pipe stream).
   Replaced by the archive-level **unit inventory**: `record.Unit{Path,
   Size, Members}` in the per-archive index — Path a stable name-based
   identity in the archiver's vocabulary ("table.postgres.public.users" — flat kind-first dotted names, not paths),
   Size the unit's TOTAL size as of the dump (postgres: `pg_table_size`,
   never delta-bytes extent math), Members the raw members in that archive
   carrying it (heap + forks + segments + TOAST + toast index), normalized
   to logical browse paths when served. Rendered by `nb recover
   --inventory` (a sibling MODE of `--list`/`--all`, chain-tip units) and
   the shell's `inventory` verb; the shell's `add` falls back to unit
   matching (exact, then unique substring) so `add public.users` selects
   the table's files — assembly included. The CLI grows no per-archiver
   nouns: the vocabulary lives in the recorded unit paths. The mount stays
   a faithful physical view. This resolves the "mount/browse stay
   tree-capability consumers" question above: the capability gate is the
   assembler + the inventory, and incremental chains browse correctly.

9. **Unit export** (`Exporter`: `Ext` + `Stage`). The answer to "what does a
   user DO with a table in a backup": pointing `--path`/`add` at a unit name
   materializes the unit in its directly-useful form instead of handing over
   physical files. One pointing rule everywhere — an exact tree path is that
   file; anything else resolves against unit identities (exact, then unique
   substring), unambiguous because the archiver names both namespaces and
   keeps them disjoint. Unit identities are flat kind-first dotted names
   ("table.<db>.<schema>.<table>") and export as exactly `<identity><ext>`,
   so the selection says verbatim what will land. For postgres the exporter
   restores the DLE to scratch (the honest physical-backup cost, priced and
   confirmed like `--all`), boots a THROWAWAY postmaster with every
   prod-reaching knob overridden (sockets only, no archive_command, no
   preload libraries, no absolute pid/log paths), `pg_dump -t`'s each unit,
   and tears everything down — the output is stock pg_dump SQL the operator
   imports themselves. Non-destructive by construction: no export path
   touches a live service, and pg_dump's CREATE TABLE errors loudly if the
   table already exists. A future logical mode exports without any scratch
   boot — same flag, same UX, cheaper cost note — which is the sign the
   capability sits at the right seam.

Semantics note, documented loudly: `pg_basebackup` is **cluster**-level.
The source's database only names the connection — configure one DLE per
cluster. (Per-database logical backup is the future `mode: dump`.)

## The pipe archiver (phase 1)

```yaml
archivers:
  sqlite:
    type: pipe
    backup_command: "sqlite3 {source} '.backup /dev/stdout'"
    restore_command: "sqlite3 {dest}"        # consumes the stream on stdin
    # estimate_command: "stat -c%s {source}" # optional: prints a byte count
```

- `{source}`/`{dest}` substitute the DLE source string / restore destination,
  shell-quoted; the commands run via the host executor (`sh -c`), so a remote
  DLE's producer runs on the client like tar does.
- Full-only: `HasBase` always false (the planner then schedules fulls — no
  new scheduling concept), a level > 0 request is rejected.
- No members, no `List`, nil `SpliceTrailer`, no incremental state, nothing
  to `Promote`. Estimate: `estimate_command` if given (stdout = byte count),
  else 0 — the planner already treats a zero estimate as "no estimator". The
  raw stream size is metered by the dumper off the producer stage's own output
  (the archiver has no totals side channel), so the committed record still
  carries a real uncompressed size for a locally-run producer.
- Stock recipe = the configured `restore_command` — recovery needs only the
  user's own tool.

## Phasing

1. **Phase 1 (done):** the five neutrality moves + the `pipe` archiver.
   Each capability was added because pipe forced it, none speculatively.
2. **Phase 2 (done):** postgres — built as `mode: incr` with libpq
   client-side credentials, per-DLE `CheckSource` connectivity probes, and
   neutrality moves 7–8 (see "The postgres archiver as built"); the
   `hosts:` connection overrides and the env-var credential rung were
   dropped, `mode: wal` deferred.
3. **Phase 3:** mysql (`mode: dump`, then `mariabackup`), zfs send/recv;
   postgres `mode: dump` (logical, members = pg_restore TOC entries) and/or
   `mode: wal` if wanted.
4. **Not planned:** dump(8); a "service" config concept.
