---
title: Restore by hand
layout: default
parent: Reference
nav_order: 3
description: "Recover an NBackup archive with stock gpg/zstd/tar and no NBackup binary, config, or catalog."
---

# Restoring without NBackup

NBackup's artifacts are plain GNU-tar archives, compressed (and optionally
encrypted) with stock tools. **Recovery never requires NBackup** — this page is
the by-hand procedure for when the binary, config, or catalog are all gone. For
normal restores use `nb recover`; to rehearse this exact bare-tools path (and have
the commands printed for you), run `nb drill --tier stock`.

The on-disk layout is described in [Concepts](concepts#artifacts-you-can-read):
each archive is a clean `NNNNNN-<dle>-L<n>.tar.<ext>` payload with a `.hdr`
sidecar, plus a small commit footer and member index per archive.

## A single full

The disk payload is a clean `tar.<ext>` — no header to skip:

```bash
zstd -dc 000000-app01-home-L0.tar.zst | tar -xf -
```

(`gzip -dc` for a `.tar.gz`; a `none` scheme is a plain `.tar`.)

## A full + its incrementals

Replay **one archive per level** — the newest of each, from the last full forward
— in level order. Each level is cumulative since the one below, so only its newest
dump is needed. Replaying an *older* same-level rerun re-applies GNU tar's
rename/delete directives and aborts (`tar: Cannot rename …`).

Order runs by date **then** same-day sequence. A plain glob mis-sorts a `.2`
rerun before its own date (since `'.' < '/'`), so normalize first:

```bash
dle=app01-home
runs=$(ls -d runs/run-* | sed -E 's#(/run-[0-9-]+)$#\1.1#' \
          | sort -t. -k1,1 -k2,2n | sed -E 's#\.1$##')
# keep only the runs from this DLE's most recent full onward:
full=$(for d in $runs; do ls "$d"/0*-"$dle"-L0.tar* 2>/dev/null; done | tail -1)
chain=$(printf '%s\n' "$runs" | sed -n "\#^$(dirname "$full")\$#,\$p")
for lvl in $(seq 0 9); do
  a=$(for d in $chain; do ls "$d"/0*-"$dle"-L"$lvl".tar* 2>/dev/null; done | tail -1)
  [ -n "$a" ] && zstd -dc "$a" | tar --extract --listed-incremental=/dev/null
done
```

## Encrypted archives

An encrypted archive's payload carries a `.gpg` suffix on top of the `.tar.<ext>`
name (`…-L0.tar.zst.gpg`), signalling that it is ciphertext; reverse it the same
way, decrypting first:

```bash
# public-key (the private key is in the operator's keyring):
gpg -d < 000000-app01-home-L0.tar.zst.gpg | zstd -dc | tar -xf -

# symmetric (passphrase_file) — supply the passphrase non-interactively, or a bare
# `gpg -d` blocks on a pinentry prompt:
gpg --batch --pinentry-mode loopback --passphrase-file /etc/nbackup/secret -d \
    < 000000-app01-home-L0.tar.zst.gpg | zstd -dc | tar -xf -
```

A public-key dump restores on any host with the private key in its keyring; a
symmetric (`passphrase_file`) dump needs the same passphrase supplied to gpg.

## From tape

Tape frames each payload with a fixed 32 KB inline header — skip it first. On a
file-backed (dir-backed) library each volume is a `slot-NN/` directory of plain
files, so a regular `dd` reads one:

```bash
dd bs=32k skip=1 < file | zstd -dc | tar -xf -
```

On a real drive (`/dev/nst0`) the backend writes in variable-block mode, so position
to the file with `mt` and read with a block size at least the medium's `block_size`
(the records are that big — `bs=256k` covers the 256k ceiling):

```bash
mt -f /dev/nst0 asf 1                              # position to tape file 1 (file 0 is the label)
dd if=/dev/nst0 bs=256k skip=1 | zstd -dc | tar -xf -   # skip the 32 KB header record
```

A **spanned** archive is split into parts written across several volumes. On a
dir-backed library each volume is a `slot-NN/` directory whose file `000000` is the
volume's identity label; the data files follow as `000001`, `000002`, …. `nb run
<run>` prints the volume chain — in write order — and, per archive, the file
position of each part (a position of `1` is the file `000001`). Match each volume
label to its `slot-NN/` directory by reading that directory's `000000` label file.
Then read each part as `<dir>/slot-NN/<position>`, strip its 32 KB header, and
concatenate the parts in chain order before decompressing:

```bash
# one part per volume, in the chain order `nb run` prints (positions here are 1):
for p in vtape/slot-08/000001 vtape/slot-01/000001 vtape/slot-02/000001; do
  dd bs=32k skip=1 < "$p"
done | zstd -dc | tar -xf -
```
