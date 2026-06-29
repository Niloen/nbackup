---
title: Cloud-only
layout: default
parent: Scenarios
nav_order: 3
description: "Dump straight to an object store (S3 or compatible) with no local copy — bandwidth-capped and encrypted."
---

# Cloud-only
{: .no_toc }

Dump straight into an object store as the only copy — bandwidth-capped so it stays polite, encrypted because the bucket is untrusted.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

You have no room or appetite for local backup storage, so the object store **is**
the backup — `nb dump` writes directly to it. This trades the fast local copy of
[Disk → S3 offsite](disk-to-s3) for simplicity. Two things make it safe to run:
a `throughput` cap so dumps don't saturate the office uplink, and encryption so a
single, possibly untrusted destination never holds readable data.

## Config

Save this as `nbackup.yaml`. The single cloud medium is both the only medium and
the landing.

```yaml
cycle: 7d

compress:
  scheme: zstd
  level: 3

media:
  # The bucket is the only copy. The backend is the URL scheme.
  cloud:
    type: cloud
    url: s3://company-backups?region=eu-north-1

    # For an S3-compatible store at a custom URL (MinIO, Wasabi, Synology C2,
    # Backblaze B2…), add an endpoint to the query instead:
    #   url: s3://my-bucket?region=eu-005&endpoint=https://s3.example.com

    capacity: 50TB
    throughput: 50MB/s     # cap the uplink so nb dump stays polite

# The only medium is also the landing.
landing: cloud

# Recommended for an untrusted destination: encrypt each archive with a public-key
# recipient after compression. Only the public key need be present here; restore
# finds the matching private key in the operator's keyring.
encrypt:
  scheme: gpg
  recipient: backups@example.com

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

sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /var/log]
```

**Credentials never live in the config.** They come from each SDK's standard
environment — `AWS_*` (and `endpoint`/`region` are passed through to the AWS SDK),
`GOOGLE_APPLICATION_CREDENTIALS`, or `AZURE_*`.

## Commands

```bash
nb plan                          # preview the run AND the storage $/month
nb dump                          # dump straight to the bucket

# Routine recoverability check, structural tier (decrypt + decompress + list,
# no extract) to keep egress down:
nb drill --tier structural
```

An offsite drill spends the **full archive bytes** in egress — an encrypted,
compressed stream is all-or-nothing to read. Watch the egress `$` the dry-run
forecast prints before running a heavier tier:

```bash
nb drill --dry-run
```

## Caveats

{: .warning }
> **A single copy is not 3-2-1.** One bucket is one copy on one medium with no
> offsite redundancy. For real durability, add a second medium and replicate to
> it — see [Full 3-2-1-1-0](full-321).

- **Immutability is operator-side.** Enable Object Lock on the bucket yourself;
  NBackup only **detects** it. `nb drill`'s WORM probe checks that deleting a fixed
  probe object is *refused* and reports it in the posture audit. See
  [Verification & drills](../features/verification).
- **Restores cost egress.** Every recovery pulls the full bytes back out of the
  store. `nb recover` estimates the egress `$` before it pulls, and `nb plan` shows
  the running storage `$/month`. See [Cost forecasting](../features/cost).

For more on the recipient model and per-dumptype overrides, see
[Encryption](../features/encryption); for the cloud medium and its URL schemes,
see [Storage media](../features/media).
