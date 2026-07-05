# Archivers beyond tar — the plugin roadmap and the neutrality it demands

Status: phase 1 implemented (neutrality moves + the `pipe` archiver);
database archivers designed, not built.

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
Do not contort them; decide their capability gate when a DB archiver actually
needs it, not before.

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

1. **Phase 1 (this change):** the five neutrality moves + the `pipe`
   archiver. Each capability was added because pipe forced it, none
   speculatively.
2. **Phase 2:** postgres (`mode: wal` first), the `hosts:` connection
   overrides, the credentials ladder, per-DLE `CheckSource` connectivity
   probes.
3. **Phase 3:** mysql (`mode: dump`, then `mariabackup`), zfs send/recv.
4. **Not planned:** dump(8); a "service" config concept; mount/browse for
   non-tree archivers.
