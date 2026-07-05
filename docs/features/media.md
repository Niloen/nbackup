---
title: Storage media
layout: default
parent: Features
nav_order: 4
description: "Disk, tape, and cloud object stores — three medium types behind one self-describing artifact, plus capacity and bandwidth controls."
---

# Storage media
{: .no_toc }

Disk, tape, and cloud object stores — three medium types behind one self-describing artifact.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The `media:` map

Storage is a map of **named definitions**. Each entry has a `type` and a set of
type-specific parameters; `landing:` names the medium that new runs are written
to:

```yaml
media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog
    capacity: 20TB
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
landing: disk
```

There are four types — **disk**, **tape**, **cloud**, and **gdrive** (Google
Drive) — but they all produce the same self-describing artifact: an archive
restores with stock tools whether it landed on a filesystem, a tape, an object
store, or a Drive folder. Adding a new medium type is a code-level registration;
it needs no change to the config structure.

Any medium can also be a **replication target**: dump to one, then mirror sealed
runs onto another with [Replication](replication). Capacity and retention are set
per medium, not globally.

## Disk

```yaml
media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog   # where runs are written (local dir or NFS)
    capacity: 20TB                   # space NBackup may use here
```

A disk medium is **address-identified**: a file's path names it, so there are no
labels, swaps, or inventory. Each archive lands as a clean payload object plus a
small `.hdr` sidecar, in a human-friendly directory layout you can browse with
`ls` — one run is a directory, one archive is three numbered files (payload,
member index, commit footer). A plain copy of the payload restores with `tar`.

## Cloud

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    # prefix: nbackup/   # optional: confine all keys under a folder in the bucket
    capacity: 50TB
```

A cloud medium stores runs in an **object store**. The `url` scheme selects the
backend:

| Scheme | Backend |
|---|---|
| `s3://` | Amazon S3 — and any S3-compatible store (MinIO, Cloudflare R2, Backblaze B2, Wasabi, Synology C2) |
| `gs://` | Google Cloud Storage |
| `azblob://` | Azure Blob Storage |

Like disk, a cloud medium is **address-identified** — no labels, no swap prompts,
nothing to inventory. It just lands and reclaims runs within its `capacity`. The
on-store layout is **disk's, verbatim**: each archive is a clean `.tar.<scheme>`
object plus a header sidecar, so a run streams disk↔cloud unchanged and a plain
GET yields an archive any stock tool can restore.

### S3-compatible custom endpoints

For stores that speak the S3 protocol at their own URL, add an `endpoint=` query
parameter:

```yaml
media:
  offsite:
    type: cloud
    url: s3://my-bucket?region=eu-005&endpoint=https://s3.example.com
    capacity: 500GB
```

The `endpoint` and `region` parameters are passed straight to the AWS SDK v2
(`V2ConfigFromURLParams`), so any of its URL options work.

### Credentials come from the environment

NBackup never reads cloud credentials from the config file. They come from each
SDK's standard environment:

- S3: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`, or an IAM role
- GCS: `GOOGLE_APPLICATION_CREDENTIALS`
- Azure: the `AZURE_*` variables

This `cloud` type is an **object-store** abstraction (S3/GCS/Azure). Google
Drive is a file API, not an object store, so it is its own medium type —
**gdrive**, below.

## Google Drive

```yaml
media:
  gdrive:
    type: gdrive
    folder: 0A--REPLACE-WITH-FOLDER-OR-SHARED-DRIVE-ID   # where backups are stored
    # prefix: nbackup/   # optional: a subfolder path under `folder`
    capacity: 2TB
```

A **gdrive** medium stores runs in a Google Drive folder. Like disk and cloud it
is **address-identified** (no labels, no swap prompts, nothing to inventory), and
its on-Drive layout is **disk's, verbatim** — `runs/<run>/` folders holding clean
payload files plus `.hdr` sidecars — so a run streams disk↔cloud↔gdrive unchanged
and a plain download yields a stock-tool-restorable archive. A large archive is
split into `≤ part_size` ordered part-files (default 10 GiB) for resumability.
Selective restore uses Drive's ranged download, so it pays for the covering
frames' bytes, not the whole archive.

`folder` is a Drive **folder ID** (the last path segment of the folder's URL) or a
**Shared Drive ID**. NBackup only ever touches files it created (the `drive.file`
OAuth scope).

### Two ways to authenticate

Credentials come from `GOOGLE_APPLICATION_CREDENTIALS` — **never the config file** —
and the file is one of two kinds, auto-detected:

| Credential | Best for | Setup |
|---|---|---|
| **Service-account key** | Unattended, **Workspace + a Shared Drive** | Share a Shared Drive with the service account; point `GOOGLE_APPLICATION_CREDENTIALS` at its JSON key. No login step. |
| **OAuth user token** | A **personal `@gmail`** Drive (or a Workspace user's own Drive) | Run `nb login gdrive` once (below); it writes the token file. |

A bare service account has **no usable My-Drive storage quota**, so on a personal
account (which has no Shared Drives) the OAuth token is the only workable path; on
Workspace, a Shared Drive is the clean unattended choice. The full account-type ×
mechanism matrix is in [Backing up to Google Drive](../scenarios/gdrive).

### `nb login` for the OAuth path

`nb login gdrive` runs a **headless** consent flow — no browser is launched and no
callback port is bound, so it works over SSH on a server. It prints a URL you open
on any device and reads back the authorization code you paste. It needs a
**Desktop-app OAuth client** you create once in the Google Cloud Console (NBackup
ships none, so there is no shared app or quota):

```bash
export GOOGLE_APPLICATION_CREDENTIALS=~/.config/nbackup/gdrive-token.json
nb login gdrive --client ~/Downloads/client_secret.json
# open the printed URL, authorize, paste the code back
```

Because the scope is `drive.file` (non-sensitive), you can publish your consent
screen to **Production** without Google's verification review, so the token does
not expire. See [Backing up to Google Drive](../scenarios/gdrive) for the
step-by-step console walkthrough.

## Tape

```yaml
media:
  lto:
    type: tape
```

A tape medium is a **changer**: a set of **drives** (data-transfer elements) fed
from a set of **slots** (storage elements that hold cartridges). It comes in
shapes that differ in **who loads the tape**:

- A **changer with a robot** — either a **virtual library** (`dir:` in a local
  directory or, via a bucket URL like `s3://…`, an object store) with `slots: N`
  cartridges and a finite `volume_size`, or a **real SCSI library** (`changer:`
  the robot's control node + `device:` the drive nodes) driven via `mtx(1)`. Either
  way `drives: K` (default 1) run in parallel and a command loads a slot into a
  drive; the robot does it unattended.
- A **single drive loaded by hand** — either an emulated sim (`manual: true`, a
  drive a human loads), or a real drive (`device:`, a one-drive changer with
  `block_size:` for the tape record size). It shows only the cartridge currently
  loaded.

For a real SCSI library, `changer:` is the control (sg) node and `device:` lists
the drive nodes **in the library's drive order** — entry *i* is drive *i*, which is
not the numeric `/dev/nstN` order (a library's drive 0 can be `/dev/nst7`). `nb
medium <name>` prints each drive's node so you can confirm the mapping; see
[Robotic tape library](../scenarios/tape-library#real-hardware-a-scsi-changer).

When a backup or restore needs a different tape, a robot loads it; a `manual:
true` changer or a real `device:` drive **prompts you to load it and waits**. An
unattended run errors instead of hanging.

### Labelling and loading

Tapes are **labelled**, not address-identified. Each slot reports a physical
**barcode** the library scanner reads without loading the cartridge; the on-tape
**label** is read only after a cartridge is loaded into a drive. You label a
blank, inventory a medium, and load a tape:

```bash
nb label lto lto-0001     # label a blank (a robot grabs a blank slot)
nb medium lto             # inventory: drives (loaded barcode + label) and slots
nb load lto 2             # load slot 2 (or: nb load --label lto lto-0007)
```

Each tape carries a self-describing label that NBackup **verifies before every
write**, so a foreign or wrong-pool reel is never clobbered.

### Appending and spanning

```yaml
media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape
    slots: 20
    drives: 1
    volume_size: 6TB
    appendable: true        # default: pack many runs per tape; false = one run per tape
```

`appendable: true` (the default) packs many runs onto one tape; `appendable:
false` writes one run per tape. A run that fills a tape mid-write **spans onto the
next automatically** — a robot loads the next writable slot (auto-labelling a
blank, or recycling the oldest tape past retention), while a manual drive prompts
for a swap. See [Robotic tape library](../scenarios/tape-library) for the full
walkthrough.

A tape medium's capacity derives as `slots × volume_size` (`0` = unbounded).

## Inspecting media

```bash
nb medium          # overview: each medium's type, runs, usage / capacity, volume
nb medium lto      # one medium's volume + runs (incl. drives + slots for tape)
```

`nb medium` with no argument lists every medium; with a name it details that one
— a disk or cloud medium's usage, or a tape changer's drives (each with the loaded
cartridge's barcode, label, and fill) and its occupied slots (by barcode).

## Capacity (per medium)

Capacity is the headline knob, set **per medium**:

```yaml
media:
  disk: { type: disk, path: /var/lib/nbackup/catalog, capacity: 20TB }
  lto:  { type: tape, dir: /var/lib/nbackup/vtape, slots: 20, volume_size: 6TB }
```

Disk and cloud spell it directly (`capacity: 20TB`); a tape changer derives it as
`slots × volume_size`. The planner fills free capacity with promoted fulls, and
pruning reclaims to stay within it. An optional `minimum_age` is a per-medium
safety floor (defaults to one cycle) below which a run is never retired. See
[Planning](planning) and [Pruning](pruning) for how each consumes capacity.

## Bandwidth politeness (per medium)

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
    throughput: 50MB/s     # cap to/from this medium (default: uncapped)
```

A medium may declare a `throughput` cap — bytes per second, the `/s` is optional.
It is the network analogue of the `nice` CPU politeness NBackup already applies:
it keeps a dump or sync from saturating the office uplink. The cap is **symmetric
on reads** — a restore or drill download from the same medium honors it too — and
concurrent workers writing one medium **share** the single budget. Set it on the
medium whose link you must protect, typically a cloud or other remote tier.

## The three types at a glance

| Type | Identified by | Labels? | Swaps? |
|---|---|---|---|
| `disk` | Address (path) | No | No |
| `cloud` | Address (URL key) | No | No |
| `tape` | Label | Yes | Robot loads slots; manual prompts you |

---

See also [Holding disk](holding-disk) for feeding a slow tape or cloud at disk
speed, [Replication](replication) for landing fast then mirroring offsite, and the
tape and cloud [Scenarios](../scenarios) for complete worked setups.
