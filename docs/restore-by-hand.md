# Restoring without NBackup

NBackup's artifacts are plain GNU-tar archives, compressed (and optionally
encrypted) with stock tools. **Recovery never requires NBackup** — this page is
the by-hand procedure for when the binary, config, or catalog are all gone. For
normal restores use `nb recover`; to rehearse this exact bare-tools path (and have
the commands printed for you), run `nb drill --tier stock`.

The on-disk layout is described in the [README](../README.md#artifacts-you-can-read):
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

Order slots by date **then** same-day sequence. A plain glob mis-sorts a `.2`
rerun before its own date (since `'.' < '/'`), so normalize first:

```bash
dle=app01-home
slots=$(ls -d slots/slot-* | sed -E 's#(/slot-[0-9-]+)$#\1.1#' \
          | sort -t. -k1,1 -k2,2n | sed -E 's#\.1$##')
# keep only the slots from this DLE's most recent full onward:
full=$(for d in $slots; do ls "$d"/0*-"$dle"-L0.tar* 2>/dev/null; done | tail -1)
chain=$(printf '%s\n' "$slots" | sed -n "\#^$(dirname "$full")\$#,\$p")
for lvl in $(seq 0 9); do
  a=$(for d in $chain; do ls "$d"/0*-"$dle"-L"$lvl".tar* 2>/dev/null; done | tail -1)
  [ -n "$a" ] && zstd -dc "$a" | tar --extract --listed-incremental=/dev/null
done
```

## Encrypted archives

An encrypted archive keeps the same `.tar.<ext>` name and reverses the same way,
decrypting first:

```bash
# public-key (the private key is in the operator's keyring):
gpg -d < 000000-app01-home-L0.tar.zst | zstd -dc | tar -xf -

# symmetric (passphrase_file) — supply the passphrase non-interactively, or a bare
# `gpg -d` blocks on a pinentry prompt:
gpg --batch --pinentry-mode loopback --passphrase-file /etc/nbackup/secret -d \
    < 000000-app01-home-L0.tar.zst | zstd -dc | tar -xf -
```

A public-key dump restores on any host with the private key in its keyring; a
symmetric (`passphrase_file`) dump needs the same passphrase supplied to gpg.

## From tape

Tape frames each payload with a fixed 32 KB inline header — skip it first:

```bash
dd bs=32k skip=1 < file | zstd -dc | tar -xf -
```

A **spanned** archive is split into parts across volumes. `nb slot show <slot>`
lists the volume chain and each part's position (e.g. `bay-NN/000001`). Strip each
part's 32 KB header and concatenate before decompressing:

```bash
for p in bay-01/000001 bay-02/000001 …; do dd bs=32k skip=1 < "$p"; done \
  | zstd -dc | tar -xf -
```
