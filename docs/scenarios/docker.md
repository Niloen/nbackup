---
title: Running in Docker
layout: default
parent: Scenarios
nav_order: 10
description: "Run nb from the ghcr.io/niloen/nbackup container with the config, catalog, and sources mounted from the host — scheduling stays host cron."
---

# Running in Docker
{: .no_toc }

Run `nb` from the official container image with your config, catalog, and sources
mounted from the host — no daemon, scheduling stays host cron.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

You'd rather not install `nb` (and GNU tar, zstd, gnupg) on the host — a
container-first box, or a machine you keep clean. The image
`ghcr.io/niloen/nbackup` ships `nb` with GNU tar, zstd, gnupg, and
`openssh-client` (for remote sources) already inside. There is **no daemon**: the
entrypoint is `nb`, so each `docker run` is one command that exits, exactly like
running `nb` directly. Scheduling stays your host's cron.

## Config

Write `nbackup.yaml` on the host as usual (see [Single machine](single-machine) for
a minimal one). One container-specific rule: **`workdir` and `state_dir` must be
absolute paths** in the config, because the container has no notion of your host's
working directory. Point them (and the catalog `path`) at directories you mount
from the host, so runs and incremental state persist across container restarts.

## Run it

Mount the config, the catalog directory, the incremental `state_dir`, and whatever
you back up. This example keeps everything under `/var/lib/nbackup` on the host and
backs up `/home` read-only:

```bash
docker run --rm \
  -v /etc/nbackup/nbackup.yaml:/etc/nbackup/nbackup.yaml:ro \
  -v /var/lib/nbackup:/var/lib/nbackup \
  -v /home:/home:ro \
  ghcr.io/niloen/nbackup -c /etc/nbackup/nbackup.yaml dump
```

- The config is mounted read-only and named with `-c`, so `nb` finds it regardless
  of the container's working directory.
- `/var/lib/nbackup` holds the catalog, the workdir, and the `state_dir` — one
  mount keeps all persistent state on the host.
- The source is mounted read-only; add a `-v` for each path in your `sources:`.
- Reading system paths like `/etc` needs the container to run as **root** (the
  default), the same caveat as a host install.

Every other command runs the same way — swap `dump` for `plan`, `status`,
`recover`, `report`, and so on:

```bash
docker run --rm -v /etc/nbackup/nbackup.yaml:/etc/nbackup/nbackup.yaml:ro \
  -v /var/lib/nbackup:/var/lib/nbackup \
  ghcr.io/niloen/nbackup -c /etc/nbackup/nbackup.yaml plan
```

## Schedule with host cron

There is no scheduler in the image; a host cron line runs the same `docker run …
dump` nightly:

```cron
0 2 * * *  docker run --rm -v /etc/nbackup/nbackup.yaml:/etc/nbackup/nbackup.yaml:ro -v /var/lib/nbackup:/var/lib/nbackup -v /home:/home:ro ghcr.io/niloen/nbackup -c /etc/nbackup/nbackup.yaml dump
```

## Notes

- **Cloud / gdrive credentials** come from the environment, not the config — pass
  them with `-e AWS_ACCESS_KEY_ID=… -e AWS_SECRET_ACCESS_KEY=…` (or mount the
  credential file and point `GOOGLE_APPLICATION_CREDENTIALS` at the mounted path).
- **Remote SSH sources** need the key and `known_hosts` inside the container —
  mount `~/.ssh` read-only and reach the client from there.
- The image is a thin runtime layer; to build it yourself, `docker build -t
  nbackup .` from a source checkout uses the repo's `Dockerfile`.
