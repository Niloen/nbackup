---
title: Remote sources over SSH
layout: default
parent: Features
nav_order: 11
description: "Back up remote hosts over SSH with no NBackup software, daemon, or open port on the client."
---

# Remote sources over SSH
{: .no_toc }

Back up remote hosts over SSH with no NBackup software, daemon, or open port on the client.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Any non-local host is remote

A DLE's `host` is meaningful. `localhost` (or an empty host) is dumped locally; **any
other host name is a remote client backed up over SSH**. NBackup runs stock tools
(`tar`, and the optional compressor and `gpg`) on the client and streams the archive
back over the connection. There is **no NBackup software, daemon, or open port on the
client**, and the intermediate bytes never touch the client's disk.

## Configuration

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

- The global `ssh:` block sets defaults for every remote host: the login `user`, the
  `identity_file`, and extra `options` passed to `ssh`.
- The `hosts:` block holds **optional** per-host overrides — a different `port`, the
  host's `state_dir`, or the path to its `tar` binary.
- `sources:` groups the DLEs. Any source whose host is not `localhost` is dumped over
  SSH.

## Credentials stay out of the config

Credentials follow the same no-secrets-in-config rule as cloud and gpg: `identity_file`
is a **path**, not a key, and the key itself comes from the operator's ssh agent or
config. NBackup stores no SSH secret.

Listing a host under `hosts:` is **only** to override the `ssh:` defaults — it is *not*
what makes a host remote. Any non-`localhost` source is remote by default, even with no
`hosts:` entry at all.

## Check connectivity first

`nb check` reaches every source host so you can confirm connectivity before a run:

```bash
nb check
```

Pass `--offline` to skip the reachability probes:

```bash
nb check --offline
```

## Incremental state lives with the host

A host's `state_dir` is where it keeps its incremental (`.snar`) state. It is a **host
property**, not an archiver option:

- a fleet-wide default `state_dir:` at the top level,
- a per-host override `hosts.<h>.state_dir`,
- and, unset, `nbackup-state`.

The state root is kept **beside** the catalog, never inside the disposable catalog
workdir — the workdir is a rebuild-from-media cache, while the incremental state is
precious and not reconstructable.

## Compressing and encrypting on the client

Both compression and encryption have an `at: server | client` selector (set per
dumptype, the peer of Amanda's client `compress`/`encrypt` directives). Moving
them to the **client** means only compressed ciphertext crosses the wire and
plaintext never leaves the source:

```yaml
dumptypes:
  remote-secure:
    archiver: default
    compress:
      at: client                     # server (default) | client
    encrypt:
      scheme: gpg
      recipient: backups@example.com
      at: client
```

`encrypt.at: client` **requires** `compress.at: client` (encryption is downstream
of compression). With a public-key recipient, only the *public* key needs to be on
the client — the private key decrypts wherever it lives. This is the basis of the
three key-trust postures; see [Encryption](encryption).

{: .note }
SSH paths are not exercised in CI.

---

See also: [Remote hosts scenario](../scenarios/remote-hosts), [Encryption](encryption),
[Concepts](../concepts).
