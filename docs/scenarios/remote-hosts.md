---
title: Remote hosts over SSH
layout: default
parent: Scenarios
nav_order: 7
description: "Back up several remote machines over SSH with no agent, daemon, or open port on the clients."
---

# Remote hosts over SSH
{: .no_toc }

One backup server pulls several remote machines over SSH — stock tools run on each client, archives stream home, nothing is installed on the clients.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

You have one backup server and a handful of machines to protect, and you don't
want to install anything on them. NBackup runs stock `tar` (and, optionally, the
compressor and `gpg`) on each client over an ordinary SSH connection and streams
the archive back to the server. There is **no NBackup software, no daemon, and no
open port on the client beyond SSH** — intermediate bytes never touch the client's
disk. The server holds the catalog, the media, and the schedule; the clients just
answer when SSH knocks.

## Config

Save this as `nbackup.yaml` on the backup server. Any source host that isn't
`localhost` is reached over SSH automatically.

```yaml
cycle: 7d

compress:
  scheme: zstd
  level: 3

# A single disk landing on the backup server. Slots from every host — local and
# remote — are written here.
media:
  disk:
    type: disk
    path: /var/lib/nbackup/disk
    capacity: 10TB
landing: disk

# SSH defaults applied to EVERY remote host (Amanda's global auth settings).
# identity_file is a PATH, not a key — NBackup stores no secret; the key itself
# lives in the operator's ssh agent/config.
ssh:
  user: backup
  identity_file: ~/.ssh/nbackup
  options: ["-o", "StrictHostKeyChecking=accept-new"]

# hosts: is OPTIONAL and only OVERRIDES the ssh: defaults for a named host — it is
# NOT what makes a host remote. Per-host fields merge over the global ssh: block.
hosts:
  app01:
    ssh:
      port: "2222"                  # this host listens on a non-standard port
    state_dir: /var/lib/nbackup     # where THIS host keeps its incremental (.snar) state
    archivers:
      gnutar:
        tar_path: /usr/local/bin/gtar   # this host's GNU tar lives off the default PATH

archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"

dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

# The disklist, grouped by dumptype then host. localhost is dumped locally; every
# other host name is a remote SSH client.
sources:
  default:
    app01: [/home, /etc]
    db01: [/var/lib/postgresql/backups]
    localhost: [/etc]
  no-logs:
    web01: [/srv/www, /opt/app]
```

**Any non-`localhost` host is remote by default.** `app01`, `db01`, and `web01`
above are all backed up over SSH simply by not being `localhost`; only `app01`
appears under `hosts:`, and only to override the SSH port, its state directory, and
its tar path. `db01` and `web01` use the global `ssh:` defaults unchanged.

**No secret is stored.** Credentials come from the operator's SSH agent and config
— `identity_file` is a path NBackup hands to `ssh`, never a key it reads or keeps,
the same no-secrets-in-config rule as cloud and gpg.

**Each host owns its own incremental state.** A client's `.snar` listed-incremental
library lives on the client under its `state_dir` (a host property, not an archiver
option — see [Concepts](../concepts)). Losing a client only forces *its* next dump
to a full; the others are unaffected.

## Commands

```bash
nb check                 # reach EVERY source host — confirm connectivity before a run
nb check --offline       # validate config only, skip the SSH reachability probe
nb plan                  # preview the next run across all hosts
nb dump                  # dump every host (remote hosts run tar over SSH)
nb status                # progress of the running (or most recent) dump
```

Run `nb check` before the first real run, and from cron's perspective whenever the
fleet changes — it connects to each host so a misconfigured key or a down machine
surfaces as a clear error instead of a failed dump.

## What happens

- For each remote DLE, NBackup opens an SSH connection, runs stock `tar` (plus the
  compressor / `gpg` if the dumptype runs them client-side) on the client, and
  streams the archive back to the server, where it lands as a slot on `disk`.
- `localhost` DLEs are dumped locally in the same run — one slot covers the whole
  fleet.
- Each host's incremental chain is tracked independently against its own `.snar`
  state, so levels and bumps are decided per host.

## What to watch

- **Combine with offsite replication.** This scenario lands everything on one disk;
  add a cloud medium and a `sync:` rule to push those slots offsite. See
  [Disk → S3](disk-to-s3).
- **Client-side encryption is available.** A dumptype can run compression and
  encryption on the *client* so only ciphertext crosses the wire and plaintext never
  leaves the source. See [Encryption](../features/encryption).
- **SSH paths aren't exercised in CI.** The remote transport is not covered by the
  automated test suite, so validate your hosts with `nb check` and a dry-run dump
  before trusting an unattended schedule.

See [Remote sources over SSH](../features/remote-sources) for the full feature
reference.
