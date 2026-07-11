---
title: Configuration reference
layout: default
parent: Reference
nav_order: 2
description: "Every nbackup.yaml block — cycle, compress, media, archivers, dumptypes, sources, sync, notify, ssh, hosts — with examples."
---

# Configuration reference
{: .no_toc }

Every `nbackup.yaml` block, top to bottom, with a faithful example for each.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

NBackup reads a single YAML file (default `nbackup.yaml`, override with
`-c/--config` or the `NBACKUP_CONFIG` environment variable — handy for a
cron/systemd unit that sets the path once; an explicit `-c` still wins). The
blocks below are listed in the order they appear in the
shipped [`nbackup.example.yaml`](../../nbackup.example.yaml). Unknown keys are
rejected, so a typo fails loudly rather than being ignored.

## cycle

The dump cycle: the **target — and hard maximum — time between fulls** for every
DLE (default `7d`). It is the main scheduling knob. A full never ages past one
cycle; light runs are automatically filled by pulling a future full forward,
bounded by free capacity.

```yaml
cycle: 7d
```

`nb check` also enforces the cycle as a freshness bound: any configured DLE
whose newest backup at **any** level is older than one cycle is a check
failure (a never-backed-up DLE is a warning instead). See
[Monitoring](../features/monitoring).

`bump_percent` (default 5) is the companion knob: an incremental sits at level 1
and only climbs to a deeper level when doing so saves at least this percent of
the full-dump size. See [Planning](../features/planning).

```yaml
bump_percent: 5
```

## compress

Archives are piped through an external compressor child process. The scheme
binary must be on `PATH` and is **checked before a dump runs**.

```yaml
compress:
  scheme: zstd        # zstd | gzip | none
  level: 3            # compression level (0 = scheme default)
  threads: 0          # compressor threads (0 = scheme default)
  # program: /usr/bin/zstd   # optional binary override
  # at: server        # server (default) | client — where compression runs for a
                      # remote DLE (usually overridden per dumptype, see below)
```

`none` needs no binary; use it where `zstd` is not installed. The scheme is
recorded per archive, so restore reverses it from the artifact alone.

This block is the **config-wide default**. A `dumptype` may set its own `compress`
block to override it **wholesale** — the exact peer of [`encrypt`](#encrypt)
(a different scheme/level for some data, or a client-side `at`). There is **no
field merge**: a dumptype's `compress` block replaces this one entirely. The `at`
selector (`server`/`client`) chooses where compression runs for a
[remote DLE](../features/remote-sources); `encrypt.at: client` requires
`compress.at: client`.

## media

A map of named storage definitions. Each entry has a `type` and type-specific
parameters; `landing` (below) names the one runs are written to. Adding a
medium type is a registry registration — no config struct changes.

### Common keys

| Key | Applies to | Meaning |
|---|---|---|
| `type` | all | `disk`, `tape`, `cloud`, or `gdrive`. |
| `capacity` | disk, cloud | Space NBackup may use here; `nb prune` reclaims oldest to fit. |
| `minimum_age` | all | Retention floor before a run may be retired. Defaults to **one cycle**. |
| `throughput` | all | Bandwidth cap, e.g. `50MB/s` (bytes/sec, `/s` optional; default uncapped). Symmetric on reads; concurrent workers share it. |
| `holding` | disk, cloud | `true` marks a scratch buffer the dump flows through (see [Holding disk](../features/holding-disk)). |
| `writers` | all | Max archives written to this medium at once — direct dumps, drains from the holding disk, and staging onto a holding disk all count. Default: the medium's natural width (a serial library's `drives`, else `parallelism.workers`). Set `1` to keep a spinning disk's writes sequential. |
| `cost` | cloud | Optional per-medium rate overrides (below). |

**Capacity is per-medium.** A copy on another medium never makes an archive
prunable. `minimum_age` defaults to one cycle. See [Pruning](../features/pruning).

### type: disk

A filesystem directory (local or NFS).

```yaml
media:
  fast-disk:
    type: disk
    path: /var/lib/nbackup/catalog   # where runs are written
    capacity: 20TB
    # minimum_age: 30d
```

### type: cloud

An object store via the Go CDK. The backend is selected by the `url` **scheme**:
`s3://` (S3 and any S3-compatible store — MinIO, R2, B2, Wasabi), `gs://`
(Google Cloud Storage), `azblob://` (Azure Blob).

```yaml
media:
  cloud:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    # prefix: nbackup/      # optional: confine keys under a folder in the bucket
    capacity: 50TB
    throughput: 50MB/s
```

For an S3-compatible store at a custom URL, add `endpoint=` to the URL query;
`region` and `endpoint` pass through to the AWS SDK v2:

```yaml
    url: s3://my-bucket?region=eu-005&endpoint=https://s3.example.com
```

**Credentials are never in the config** — they come from each SDK's standard
environment (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`).

An optional `cost:` block overrides the rate a cloud medium otherwise infers
from its URL scheme (`s3://` = AWS, `gs://` = GCS, `azblob://` = Azure):

```yaml
    cost:
      provider: aws-s3              # base rate table (default: inferred from url)
      storage_per_gb_month: 0.021
      egress_per_gb: 0.05
      get_per_1000: 0.0004
```

See [Media](../features/media) and [Cost](../features/cost).

### type: gdrive

A Google Drive folder (a file API, not an object store, so it is its own type).
Address-identified like disk/cloud; the on-Drive layout is disk's verbatim. A
large archive splits into `≤ part_size` part-files (default 10 GiB).

```yaml
media:
  gdrive:
    type: gdrive
    folder: 0A--FOLDER-OR-SHARED-DRIVE-ID   # from the folder's URL
    # prefix: nbackup/       # optional: a subfolder path under `folder`
    capacity: 2TB
    # throughput: 20MB/s
```

**Credentials are never in the config.** There are two paths, auto-detected:

- **Service account** (unattended; a Workspace **Shared Drive**): set
  `GOOGLE_APPLICATION_CREDENTIALS` to the key file. When set it always wins.
- **OAuth authorized-user token** (a personal Google Drive): run
  `nb login gdrive` once, which writes a reusable token to a default path under
  the [`secrets_dir`](#secrets_dir) (`<secrets_dir>/gdrive.json`) that the medium
  reads back automatically — no environment variable to set.

A bare service account has no usable My-Drive quota, so personal accounts must
use the OAuth path. The scope is `drive.file`, so NBackup sees only files it
created. An optional `cost:` block overrides the storage rate (Drive bills no
egress/GET).

See [Backing up to Google Drive](../scenarios/gdrive), [Media](../features/media),
and `nb login`.

### type: tape

A tape medium is a **changer**: drives (data-transfer elements) fed from slots
(storage elements that hold cartridges). It comes in three shapes: an emulated
library (`dir:` with `slots:`, and optionally `drives:`); a **real SCSI library**
(`changer:` the robot's sg control node + `device:` the drive nodes, driven via
`mtx(1)`); or a single drive — hand-loaded emulated (`dir:` + `manual: true`) or a
real no-rewind `st` drive (`device:`). `dir:` is a local directory or a bucket URL
(`s3://`, `gs://`, `azblob://`), so the same virtual library can live in an object
store.

```yaml
media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape   # an emulated virtual library
    # dir: s3://backups?prefix=vtape/  # OR the same library in an object store
    # device: /dev/nst0           # OR a real single no-rewind drive (`slots`/
    #                             # `drives`/`manual` do not apply)
    slots: 20                     # storage slots (dir-backed library)
    drives: 1                     # data-transfer drives a robot loads slots into
    volume_size: 6TB              # per-cartridge capacity; on a real drive declare
    #                             #   it a bit below native (fill is catalog-tracked)
    # block_size: 64k             # (device:/changer: only) tape record size; default 64k, 32k–256k
    minimum_age: 180d
    appendable: true              # pack many runs per tape; false = one run per tape
  robot:                          # a REAL SCSI library driven via mtx(1)
    type: tape
    changer: /dev/sg0             # the library's robot control (sg) node
    device: /dev/nst0,/dev/nst1   # the drive nodes IN THE LIBRARY'S DRIVE ORDER
                                  #   (entry i = drive i; NOT the numeric nstN order —
                                  #   a library's drive 0 can be /dev/nst7). `nb medium`
                                  #   prints each drive's node so you can confirm it.
                                  #   `slots`/`drives`/`manual` do not apply.
    volume_size: 6TB              # declared per-cartridge capacity (a bit below native)
    minimum_age: 180d
  desk-drive:
    type: tape
    dir: /var/lib/nbackup/station
    manual: true                  # a human loads the drive; NBackup prompts and waits
    slots: 6                      # cartridges the operator can choose from (sim only)
    volume_size: 6TB
    appendable: false
```

**Tape capacity = `slots × volume_size`** (`0` = unbounded — e.g. a bare
`device:` drive whose shelf is unknowable). `volume_size` is a *declared*
capacity on real hardware (a tape reports end-of-tape only by hitting it):
NBackup derives each cartridge's fill from its catalog and spans proactively
against the declared size, so set it a little below the native capacity and
turn drive compression off (NBackup compresses in software). `part_size` is an
optional extra bound on part size. `auto_label` (global, below) lets a dump
label a blank tape automatically. See [Media](../features/media).

## landing

Which medium new runs are created on. A plain name is one landing; a **list**
fans each archive out to every entry, **primary first** — the first medium is
"the landing" that accounting, read preference, and `nb sync`'s default source
treat as authoritative, and the rest are additional copies written in the same
run.

```yaml
landing: fast-disk           # single landing
# landing: [fast-disk, s3]   # OR fan out: primary first, the rest are copies
```

A `dumptype` may set its own `landing` (a name or a list) to route that
dumptype's DLEs to different media within one run — cheap cloud for bulk data,
fast disk or tape for the rest. A dumptype's `landing` overrides this top-level
one for its DLEs; unset, they use the config-wide `landing`.

```yaml
dumptypes:
  archive-only:
    archiver: default
    landing: [s3, gdrive]    # this dumptype's DLEs land on s3 (primary) + gdrive
```

## workdir

Where the catalog's own local cache lives — its own directory, independent of
any storage medium. Defaults to `nbackup-catalog` in the working directory.

```yaml
workdir: /var/lib/nbackup/catalog
```

{: #incremental-state }
## state_dir — incremental state

Where archivers keep their incremental state (for gnutar, the `.snar`
listed-incremental library). This is a **host property**, not an archiver
option, and is a dedicated location **beside** the workdir, never beneath it —
the workdir is the disposable rebuild-from-media cache, while this state is
precious (losing a DLE's base forces its next run to a full). Defaults to
`nbackup-state`.

```yaml
state_dir: /var/lib/nbackup/state
```

Both `workdir` and `state_dir` default **relative to the working directory**, so
a cron job that runs `nb` from elsewhere must set absolute paths or it will
silently re-full. `nb check` **warns when either resolves to a relative path**,
for exactly this reason. A per-host override lives under `hosts:` (below). The
engine namespaces each archiver under a private subtree by type
(`<state_dir>/gnutar`).

{: #secrets_dir }
## secrets_dir

The pool-side root for credentials a medium's `nb login` mints — today the
Google Drive OAuth token (`<secrets_dir>/gdrive.json`). Like `state_dir` it is a
dedicated location **beside** the workdir, never beneath it: the workdir is a
disposable rebuild-from-media cache, while a login token is precious,
non-rebuildable state (only a fresh consent brings it back). Defaults to
`nbackup-secrets` relative to the config file's directory.

```yaml
secrets_dir: /var/lib/nbackup/secrets
```

## part_size

The config-wide default **atom size** for encrypted archives: each part of such
an archive is one complete encrypted message of at most this many compressed
bytes, cut at dump time and carried unchanged by every copy (default 10 GiB). A
`dumptype` may override it (`dumptypes.<name>.part_size`) — the selective-restore
tuning lever: smaller atoms give finer encrypted-restore granularity and cheaper
key-proving drills, at the cost of more objects. It is inert (and warned about at
`nb check`) on a dumptype with no encryption stage.

```yaml
part_size: 10GiB
```

## frame_size

An advanced internal knob: the raw-stream interval at which a framed archive's
encode pipeline restarts, giving a decode-restart point every this-many bytes for
ranged reads (default 256 MiB). Frames never exist as files — they are outside the
"part" vocabulary — and the default is right for almost everyone; smaller frames
only trade a sliver of compression ratio for finer ranged reads. See
[Recovery](../features/recovery).

```yaml
frame_size: 256MiB
```

## auto_label

Let a dump auto-label a *blank* tape instead of requiring `nb label` first. Off
by default — explicit labeling is what makes the overwrite guard meaningful. It
never overwrites foreign or non-blank media.

```yaml
auto_label: false
```

## archivers

A map of named archiver definitions: a registered archiver `type` plus its
content-independent options (how the tool runs, regardless of what you point it
at). An undeclared name is a bare type with defaults, so `archiver: gnutar`
works with no block here. Most setups need just one. Three types are built in —
`gnutar` (the default), `postgres`, and `pipe`; see
[Archivers](../features/archivers) for what each does and when to use it.

```yaml
archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"
    # tar_path: gtar     # GNU tar binary (use "gtar" on macOS/BSD)

  # Live PostgreSQL 17+ clusters via native incremental base backups. The DLE
  # source string is a libpq connection reference ("app_prod", "service=prod",
  # "host=/run/postgresql dbname=app"); credentials are libpq's own config
  # (peer auth, ~/.pgpass, ~/.pg_service.conf) — never this file. One DLE per
  # cluster. Requires `summarize_wal = on` (`nb check` prints the line).
  pg:
    type: postgres
    # mode: incr                           # the default (and only) mode
    # bin_dir: /usr/lib/postgresql/17/bin  # if the v17 tools are off PATH

  # Bring-your-own command: {source}/{dest} substitute the DLE source string
  # and restore destination. Full-only; the stock recovery recipe is your own
  # restore_command.
  sqlite:
    type: pipe
    backup_command: "sqlite3 {source} '.backup /dev/stdout'"
    restore_command: "sqlite3 {dest}"
    # estimate_command: "stat -c%s {source}"  # optional: prints a byte count
```

Incremental state is **not** an archiver option — its location is the host-level
`state_dir` (gnutar's `.snar` snapshots and postgres's backup manifests both
live there). The archiver owns only its format knobs like `tar_path`; a
per-host value goes in `hosts.<h>.archivers.<type>` (below).

### PostgreSQL 16 and older

Many production clusters run 13–16, where the `postgres` archiver cannot work
(the server has no incremental base backups). The honest fallback is the
`pipe` archiver around `pg_dump` — **full backups only**, but with all of
NBackup's scheduling, retention, cycle safety, copies/sync, checksum
verification, and drills applied to those fulls:

```yaml
archivers:
  pgdump:
    type: pipe
    backup_command: "pg_dump --format=custom {source}"
    # pg_restore turns the stream back into ready-to-import SQL under the
    # restore destination ({dest} must be an existing directory; a drill's
    # scratch directory is). Importing it stays your own, explicit act:
    #   psql -d <db> -f restored.sql
    restore_command: "pg_restore --file={dest}/restored.sql"
    extension: .dump
    # estimate_command: "psql -Atc 'select pg_database_size(current_database())' {source}"

dumptypes:
  databases:
    archiver: pgdump
    compress:
      scheme: none      # pg_dump's custom format is already compressed

sources:
  databases:
    db01: [app_prod]    # per-DATABASE (pg_dump), unlike the cluster-level postgres type
```

What you lose against the `postgres` type: **no incrementals** (the pipe
archiver keeps no incremental state, so it never reports a base and the
planner schedules a full every run — levels never rise above 0), **no
per-file browse** or `nb mount`, and **no table inventory/export** (the
stream is opaque; structural verify degrades to a clean decode). For a
whole-cluster dump swap in `pg_dumpall` and drop the per-database source
strings.

After upgrading the cluster to 17+, point the dumptype's `archiver` at a
`postgres` definition: the next run starts a fresh chain with a full (and the
DLE shape changes from one-per-database to one-per-cluster). The old pipe
fulls remain restorable as recorded, with `pg_restore` alone.

## dumptypes

A map of named dumptypes: an archiver reference plus per-DLE policy — what to
skip (`exclude`), and overrides for compression and encryption. Excludes live
here, not on the archiver, because skipping logs is a decision about the data,
not how tar runs. A DLE selects one dumptype.

```yaml
dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp", "./var/cache"]
```

Excludes are **relative to each source's root** (the Amanda convention): a bare
pattern (`*.log`, `cache`) matches at any depth, while a `./`-prefixed pattern
(`./var/cache`) anchors — it excludes exactly that subtree under the source
root and nothing deeper. An absolute path (`/var/cache`) is rejected at config
load, since it would silently never match. Under a partitioned source, anchored
excludes anchor at the **base you wrote**, applied to whichever derived DLE owns
that subtree — partitioning never changes which bytes are excluded. Adding an
anchored exclude re-baselines the owning DLE with one fresh full (its old chain
still holds the now-excluded subtree); editing bare globs never forces a full.

A dumptype may set its own `compress` and/or `encrypt` block to override the
config-wide default (below) **wholesale** — there is **no field merge**, so
restate the whole block. For example, spend more CPU on sensitive data and
encrypt it to a stricter key:

```yaml
  finance:
    archiver: default
    compress:
      scheme: zstd
      level: 19                      # spend more CPU on this data
    encrypt:
      scheme: gpg
      recipient: finance-key@example.com
```

A remote DLE can run compression and/or encryption on the **client** via the
`at` selector, so only compressed ciphertext crosses the wire and plaintext
never leaves the source. `encrypt.at: client` requires `compress.at: client`
(encryption is downstream of compression):

```yaml
  remote-secure:
    archiver: default
    compress:
      at: client                     # server (default) | client
    encrypt:
      scheme: gpg
      recipient: backups@example.com
      at: client
```

See [Encryption](../features/encryption) and
[Remote sources](../features/remote-sources).

## encrypt

The config-wide encryption default. After compression, archives can be piped
through `gpg`. The scheme name is recorded per-archive so restore reverses it
from the artifact alone; the **key is never stored**.

```yaml
encrypt:
  scheme: gpg                      # gpg | none (default none)
  recipient: backups@example.com   # public-key recipient (asymmetric)
  # passphrase_file: /etc/nbackup/secret   # symmetric, instead of a recipient
  # program: /usr/bin/gpg
```

Encryption is config-wide here or whole-block per-dumptype — **no field merge**.
See [Encryption](../features/encryption).

## parallelism

Run several dumps at once (Amanda's `inparallel`). The concurrency unit is a
**worker**. Keep `workers × compressor threads ≤ cores`.

```yaml
parallelism:
  workers: 1         # concurrent DLE dumps per run
```

`nice: 10` (top-level) optionally runs child processes under `nice -n` for CPU
politeness.

## sources

The disklist: grouped by **dumptype → host → paths**. The host `localhost` (or
an empty host) is backed up locally; **any other host name is a remote SSH
client** (see `ssh:`/`hosts:` below).

```yaml
sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /opt/app]
  db:
    localhost: [app_prod]     # a postgres dumptype's "path" is a libpq
                              # connection reference, not a filesystem path
```

The "path" is a **source string the dumptype's archiver interprets**: a
filesystem path for `gnutar`, a libpq connection reference for `postgres`
(`app_prod`, `service=legacy`, `"host=10.0.0.12 dbname=app"`), an opaque token
your own commands consume for `pipe`. It is also the DLE's identity — changing
the string mints a new DLE (fresh level-0; the old history stays recoverable
under the old ID until retention retires it), so prefer a stable form such as a
`service=` name whose details live in `~/.pg_service.conf`. For any other live
database, snapshot or dump it to a file first (or wrap the dump in a `pipe`
archiver).

### Pattern sources: one source, many DLEs

A `gnutar` source can resolve into many DLEs, **re-discovered at every plan** —
new directories are picked up with no config edit. Two forms, one rule: **the
rest exists exactly when you name a base.**

```yaml
sources:
  default:
    fileserver:
      - path: /data              # PARTITION: one DLE per child directory,
        partition: "*"           #   plus "the rest of /data" — full coverage
      - /srv/web-*               # SELECTION: one DLE per match, nothing else
```

The mapping form names a base, so all of it is covered: each matching child
directory becomes its own DLE and a remainder DLE ("the rest") holds loose
files and everything unmatched. A wildcard directly in the path is a dynamic
*list* — exactly the matches, no remainder, like a hand-written list. `nb plan`
renders the split with a `✓ covers 100%` line for a partition (a selection
shows none — the visible cue that only the matches are covered).

Rules: sources are **directories only** (a matching file falls to the rest, or
is not a DLE); `*` matches **one** path segment and **does** match
dot-directories (unlike a shell — over-matching is the safe direction for a
backup tool); no `**`; the partition base must be a literal, non-root path; the
partition glob is relative to it. A new child is covered by the rest the run it
appears, then graduates to its own DLE (mandatory first full) while the rest
re-baselines once. Resolution is live over the source host and **fails the
command** rather than guessing; two sources resolving to the same DLE identity
are refused with both origins named. See
[Partitioned sources](../features/partitioned-sources) for the full story.

## sync

Replication rules — each mirrors one medium's sealed runs onto another, the
batch idempotent form of `nb copy`. `nb sync` with no `--to` runs every rule.

```yaml
sync:
  - to: cloud           # mirror landing -> object store
  - to: lto
    last: 4             # copy only the 4 most recent runs
  - from: lto           # second tier: source need not be the landing
    to: deep-archive
```

| Key | Meaning |
|---|---|
| `to` | Target medium (required). |
| `from` | Source medium (defaults to the landing medium). |
| `last` | Copy only the N most recent runs (does not trim older copies — `nb prune` does that). |

See [Replication](../features/replication).

## notify

Unattended alerting. A failed run reaches the configured channels;
`nb report --notify` mails the nightly digest. A literal `password:`/`token:` key is
**rejected** (neither is a config field), so an SMTP password is referenced by the
**name** of an environment variable (`password_env`) and resolved at send time. A
webhook URL may be a literal `url:` or, when the URL is itself secret (Slack/Discord),
the name of an environment variable (`url_env`, preferred).

```yaml
notify:
  on_failure: [email, slack]   # channels to alert on failure (omit = every backend)
  # on_success: [email]        # dump notifies every backend by default; this opts
  #                            # other commands' success in (and applies to dump too)
  digest: [email]              # channels for `nb report --notify`
  backends:
    email:
      type: smtp
      host: smtp.example.com
      port: 587                # default 587
      from: nbackup@example.com
      to: [ops@example.com]
      username: nbackup        # optional (defaults to from)
      password_env: SMTP_PASS  # env var NAME — never the secret
    local-mail:
      type: sendmail           # hand off to the local MTA
      from: nbackup@example.com
      to: [ops@example.com]
      # sendmail_path: /usr/sbin/sendmail   # local sendmail binary (this is the default)
    slack:
      type: webhook            # generic JSON POST (Slack/Discord/PagerDuty)
      url_env: SLACK_WEBHOOK_URL
      # template: text         # payload field the message goes in (default "text")
      # headers:               # optional extra HTTP headers
      #   Authorization: Bearer ...
    hc:
      type: healthcheck        # dead-man's switch (healthchecks.io-style pings)
      url_env: HEALTHCHECKS_URL
    hook:
      type: command            # exec an operator script on each notification
      command: /etc/nbackup/on-notify.sh
      # args: [--nightly]      # optional fixed arguments
```

Any command alerts on failure; a successful `nb dump` notifies by default. A
`healthcheck` backend is a liveness beacon, not a report channel: it pings
`<url>/start` when a run begins and `<url>` (success) or `<url>/fail` when it
ends, on **every** run regardless of `on_failure`/`on_success` — a *missing*
ping is the alarm, so routing never filters it. A `command` backend execs the
given program directly (no shell) with `NB_COMMAND`, `NB_STATUS` (`OK`/`FAILED`),
and `NB_SUBJECT` in the environment and the rendered report on stdin. See
[Monitoring](../features/monitoring).

## drill

Recovery-drill defaults (the cron block). Every key here has a `nb drill`
command-line peer.

```yaml
drill:
  window: 30d         # every DLE should be drilled at least this often
  sample: 1           # DLEs drilled per run (risk-biased rotation)
  from: cloud         # which copy to drill (default: the landing medium)
  tier: structural    # sample | checksum | structural | chain | stock
  worm: true          # probe the medium for WORM/immutability
  unattended: false   # cron mode: never prompt; skip targets needing a tape swap
```

See [Verification](../features/verification).

## ssh and hosts

`ssh:` sets global defaults applied to every remote host (any source host that
is not `localhost`). NBackup stores **no SSH secret** — `identity_file` is a
path, the key comes from your ssh agent/config.

```yaml
ssh:
  user: backup
  # port: "22"
  identity_file: ~/.ssh/nbackup   # a PATH, not a key
  options: ["-o", "StrictHostKeyChecking=accept-new"]
```

`hosts:` is **optional** and only **overrides** the defaults for a specific
host — it is not what makes a host remote. Per-host fields are merged over the
global `ssh:` block; available overrides are `ssh.port` (and other `ssh:`
fields), `state_dir`, and `archivers.<type>` format knobs.

```yaml
hosts:
  app01:
    ssh:
      port: "2222"
    state_dir: /var/lib/nbackup     # this host's incremental-state root
    archivers:
      gnutar:
        tar_path: /usr/local/bin/gtar
```

## parallelism workers (summary)

The concurrency unit is the **worker** — `parallelism.workers` concurrent DLE
dumps per run. A spanning-capable tape landing clamps to one worker unless a
[holding disk](../features/holding-disk) absorbs the parallel dumps.

---

For the full, annotated surface — including every comment explaining the
semantics above — see the shipped
[`nbackup.example.yaml`](../../nbackup.example.yaml). For the commands that read
this config, see the [CLI reference](cli).
