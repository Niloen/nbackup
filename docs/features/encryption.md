---
title: Encryption
layout: default
parent: Features
nav_order: 6
description: "Source-tied gpg encryption that keeps copies interchangeable: encrypted once, verified and replicated offsite without the key."
---

# Encryption
{: .no_toc }

Encrypt each archive once at the source with gpg — verifying and replicating offsite never need the key.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Turn it on

Add an `encrypt` block and each archive is piped through **gpg** after
compression. Set it config-wide as the default, or per dumptype to override it
for a specific class of data. There are two modes.

**Public-key (`recipient`).** gpg encrypts to a public key; only the matching
private key can decrypt.

```yaml
encrypt:
  scheme: gpg                      # gpg | none (default none)
  recipient: backups@example.com   # gpg public-key recipient (asymmetric)
  # program: /usr/bin/gpg          # optional binary override
```

**Symmetric (`passphrase_file`).** gpg encrypts with a passphrase read from a
file, instead of a recipient.

```yaml
encrypt:
  scheme: gpg
  passphrase_file: /etc/nbackup/secret   # gpg symmetric, instead of a recipient
```

A dumptype can override the config-wide default wholesale — handy for a stricter
key on sensitive data (the block replaces the default, it does not merge field by
field):

```yaml
dumptypes:
  finance:
    archiver: default
    encrypt:
      scheme: gpg
      recipient: finance-key@example.com
```

## Encrypted once, at the source

Encryption is **source-tied**: the dump is encrypted a single time as it is
written, and every copy holds the **same ciphertext**. Vaulting offsite with
`nb sync` (see [Replication](replication)) just moves those bytes — it never needs
the key. The archive records only the **scheme name** (`gpg`), never a key, so the
artifact is self-describing: restore reverses the cipher from the archive alone, no
config required.

**Public-key dumps restore anywhere.** gpg writes the key-id into the ciphertext,
so on restore it finds the right private key in the operator's keyring on its own.
A public-key dump therefore restores on **any host with the private key**, even
with the NBackup config long gone.

**Symmetric dumps still need the config.** A `passphrase_file` dump carries no
key-id in the ciphertext, so restore still needs the `encrypt` block to point gpg
at the passphrase file. Keep that config (and the passphrase file) alongside the
keyring.

{: .warning }
> **Lose the key and the data is unrecoverable.** NBackup holds no copy of your
> key or passphrase by design — the config references a recipient or a file path,
> never the secret itself. Back up the private key (or passphrase) somewhere other
> than the backups it unlocks.

## What stays readable without the key

Each archive's **commit footer** (its identity, sizes, and checksums) and its
**member index** (the file list) stay **plaintext**. That is deliberate: it lets
`nb recover` browse a DLE's contents and pick files **without the key**, touching
the catalog only — the key is needed solely to extract the bytes you select.

{: .note }
> The trade is that **filenames and checksums are readable on the medium**. This
> is a documented trade, not an oversight: it is what keeps catalog browsing,
> integrity checks, and rebuilds keyless. If filenames themselves are sensitive,
> note the deferred work below.

## Keyless vs. key-needing operations

Because encryption is the **outermost** transform, the checksum is taken over the
ciphertext that lands on the volume. So most of NBackup's day-to-day work never
touches the key:

| Operation | Needs the key? |
|-----------|----------------|
| `nb verify` (re-hash payload vs. checksum) | No |
| `nb copy` / `nb sync` (replicate ciphertext) | No |
| `nb recover` browse (read the plaintext index) | No |
| `nb verify --deep` (decrypt to list the stream) | **Yes** |
| `nb recover` extract (decrypt the bytes) | **Yes** |

See [Verification & drills](verification) for how `--deep` and `nb drill`
exercise the key + scheme end to end.

## Per-dumptype keys in one run

A run can hold archives encrypted under **different keys** — each dumptype with
its own `recipient` — and still restores cleanly. Each public-key archive carries
its own key-id in its ciphertext, so gpg resolves the right private key per
archive; nothing in the run assumes a single key.

## Where it sits in the pipeline

Encryption is the peer of compression, one transform further out. On write the
stream is **tar → compress → encrypt → land**; on read it reverses **decrypt →
decompress**. Encryption is always the **outermost** transform, which is exactly
why verifying and replicating stay keyless — the seal covers the ciphertext.

Restoring by hand just adds `gpg -d` at the front of the stock pipe — see
[Restore by hand](../restore-by-hand).

## Encrypted archives are stored as atoms

An encrypted archive can't be re-decrypted from a concatenation of gpg messages
(GnuPG rejects that by design), so it can't carry the invisible decode-restart
points an unencrypted archive uses for ranged reads. Instead each part is written as
one complete, sealed gpg message — an **atom** — a self-contained
`…-L0.pNNN.tar.zst.gpg` file. That keeps encrypted archives from being all-or-nothing
after all: a selective `nb recover` decrypts **only the atoms covering the members you
ask for**, and a recovery drill proves the key by decrypting a **single atom** rather
than the whole payload. Copies carry atoms 1:1 (re-cutting one would need the key), so
`nb sync` stays keyless. A whole-DLE restore still reads every atom, as it must.

Atom size is the tuning lever, set with `part_size` on the dumptype (or globally):

```yaml
part_size: 10GiB              # global default atom size (matches the cloud slice size)
dumptypes:
  finance:
    archiver: default
    part_size: 2GiB           # smaller atoms → finer selective restore, cheaper drills
    encrypt:
      scheme: gpg
      recipient: finance-key@example.com
```

Smaller atoms give finer selective-restore granularity and cheaper key-proving drills,
at the cost of more objects on the medium. A sealed atom can't shrink without the key,
so it can't land on a medium whose per-part **ceiling** is below it — flagged at
`nb plan` time and refused per-archive by `nb sync`, never a silent failure. See
[Recovery → Efficient partial reads](recovery#efficient-partial-reads-archive-shapes)
for the whole shape model and how unencrypted archives get ranged reads instead.

## Related and future work

Per-medium **at-rest** encryption (S3 SSE, LTO hardware) for the
untrusted-destination posture, and **client-side** encryption with remote sources
(so plaintext never leaves the source), are noted as future work — see
[Remote sources](remote-sources).

---

See also: [Verification & drills](verification), [Replication](replication),
[Concepts](../concepts).
