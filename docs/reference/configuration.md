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
`-c/--config`). The blocks below are listed in the order they appear in the
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
| `type` | all | `disk`, `tape`, or `cloud`. |
| `capacity` | disk, cloud | Space NBackup may use here; `nb prune` reclaims oldest to fit. |
| `minimum_age` | all | Retention floor before a run may be retired. Defaults to **one cycle**. |
| `throughput` | all | Bandwidth cap, e.g. `50MB/s` (bytes/sec, `/s` optional; default uncapped). Symmetric on reads; concurrent workers share it. |
| `holding` | disk, cloud | `true` marks a scratch buffer the dump flows through (see [Holding disk](../features/holding-disk)). |
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

### type: tape

A tape medium is a **changer**: drives (data-transfer elements) fed from slots
(storage elements that hold cartridges). A file-backed library uses `dir:` with
`slots:` (and optionally `drives:`); a hand-changed single drive uses `dir:` +
`manual: true` (a file-backed sim) or `device:` (a real no-rewind `st` drive).

```yaml
media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape   # a file-backed virtual library
    # device: /dev/nst0           # OR a real single no-rewind drive (`slots`/
    #                             # `drives`/`manual` do not apply)
    slots: 20                     # storage slots (dir-backed library)
    drives: 1                     # data-transfer drives a robot loads slots into
    volume_size: 6TB              # per-cartridge capacity; a write past it hits EOT
    # part_size: 6TB              # use instead of volume_size on a real drive
    # block_size: 64k             # (device: only) tape record size; default 64k, 32k–256k
    minimum_age: 180d
    appendable: true              # pack many runs per tape; false = one run per tape
  desk-drive:
    type: tape
    dir: /var/lib/nbackup/station
    manual: true                  # a human loads the drive; NBackup prompts and waits
    slots: 6                      # cartridges the operator can choose from (sim only)
    volume_size: 6TB
    appendable: false
```

**Tape capacity = `slots × volume_size`** (`0` = unbounded — e.g. a bare
`device:` drive whose shelf is unknowable). A real drive sets `part_size`
instead of `volume_size`, since end-of-tape is only hit, not read. `auto_label`
(global, below) lets a dump label a blank tape automatically. See
[Media](../features/media).

## landing

Which medium new runs are created on.

```yaml
landing: fast-disk
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

## auto_label

Let a dump auto-label a *blank* tape instead of requiring `nb label` first. Off
by default — explicit labeling is what makes the overwrite guard meaningful. It
never overwrites foreign or non-blank media.

```yaml
auto_label: false
```

## archivers

A map of named archiver definitions: a registered archiver `type` plus its
content-independent options (how tar runs, regardless of what you point it at).
An undeclared name is a bare type with defaults, so `archiver: gnutar` works
with no block here. Most setups need just one.

```yaml
archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"
    # tar_path: gtar     # GNU tar binary (use "gtar" on macOS/BSD)
```

Incremental state is **not** an archiver option — its location is the host-level
`state_dir`. The archiver owns only its format knobs like `tar_path`.

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
    exclude: ["*.log", "*.tmp"]
```

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
```

NBackup backs up filesystem trees; for a live database, snapshot or dump it to a
file first.

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
    slack:
      type: webhook            # generic JSON POST (Slack/Discord/PagerDuty)
      url_env: SLACK_WEBHOOK_URL
```

Any command alerts on failure; a successful `nb dump` notifies by default. See
[Monitoring](../features/monitoring).

## drill

Recovery-drill defaults (the cron block). Every key here has a `nb drill`
command-line peer.

```yaml
drill:
  window: 30d         # every DLE should be drilled at least this often
  sample: 1           # DLEs drilled per run (risk-biased rotation)
  from: cloud         # which copy to drill (default: the landing medium)
  tier: structural    # checksum | structural | chain | stock
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
