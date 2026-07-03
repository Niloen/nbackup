# Design note: remote sources over SSH

Status: partially implemented — the dump path over SSH is shipped; client-side
restore/drill is future work.

Back up DLEs whose data lives on another host with **no NBackup software on the
client**: SSH is the transport and stock GNU tar (plus, optionally, the compressor and
gpg) is the "agent." This also fixes the **configurable encryption/compression point**
(`client` vs `server`) and the three trust postures that fall out of it. See
ARCHITECTURE.md, "Execution is an injected transport," for the zone model this builds on
(DLE / Archive / Run / incremental state / meter / xfer).

## The shape: SSH is the transport, GNU tar is the agent

Amanda requires a client daemon (`amandad`) even when it tunnels over SSH. NBackup does
better: its archiver already orchestrates `tar` as a child process, so the process just
needs to run on another host. For a DLE whose `host` resolves to a remote, NBackup runs
the **same dump pipeline** it runs locally, but the source `tar` (and optionally the
compress/encrypt stages) execute on the client via a single SSH command; the server
receives the byte stream and writes it to the landing volume exactly as for a local dump:

```
nb (server) ──ssh host──> tar --create --listed-incremental=<client .snar> -f - <path>
            <──────────── raw tar stream on stdout ────────────────────────────
            <──────────── --totals + framed member index on stderr ────────────
            └─ filter → crypt → meter(checksum) → volume → seal   (server-side, unchanged)
```

The client needs only **sshd + GNU tar** (and, for client-side transforms, the
compressor and gpg) — no NBackup binary, no daemon, no open port beyond SSH. The
transport lives in **package `programs`** (a `Cmd` plus an `Execution` — `Local` or
`SSH` — running a pipe of commands on one host), injected into archivers via
`archiver.Open(name, opts, ex, stateRoot)`. SSH is part of no archiver, so a new archiver
gets remote execution for free as long as its binaries are on the client. The generic
archiver/engine/planner layer never says "ssh"; it stays in the transport + engine wiring,
like `tar`/`.snar` today.

A source host is remote **by default** — anything but `localhost` is backed up over SSH;
`hosts:` is override-only, not what makes a host remote. `nb check` (the amcheck analogue)
verifies the server and every host through the host's executor — reachable, GNU tar,
source readable, client tools, `state_dir` — so local and remote checks are the same code.

## Load-bearing constraints

Four facts about `gnutar.Backup` determine the mechanism; skipping them yields a design
that "passes argv unchanged" and then can't build the seal.

1. **The snapshot is the atom, and it lives on the client (stateful client).** A remote
   DLE's gnutar `state_dir` is a path *on the client* (Amanda's `GNUTAR-LISTDIR`). The
   `.snar` is seeded and updated in place on the client; it never crosses the wire.
   Consequences: **zero round-trip**; `HasBase` is a remote `test -f`; "nothing in
   catalog/engine/planner changes" is literally true; a lost/reimaged client's failure
   mode is a **forced full on its next run** — the weakest possible failure (self-healing,
   already surfaced by the drill posture). This is Amanda's behavior. Rejected: a
   *stateless* client that round-trips the snar to centralize the precious state — more
   machinery for a degraded-to-full failure we already tolerate.

2. **`Backup` returns three things, and only one is the stdout stream.** `data` → tar
   stdout (→ volume); `Uncompressed` → parsed from tar stderr's `Total bytes written:`;
   `Members` + `FileCount` → the member index that feeds the seal (what lets `nb
   recover`/`rebuild` browse without an index server — not optional). Over SSH, tar gives
   exactly one stdout, so the index and totals return **out of band on stderr**: raw tar
   stream on stdout; a `--totals` line then a framed member index on stderr, emitted only
   after tar exits. **Atomicity falls out for free** — the framed index arrives only on a
   clean tar exit, so a dropped connection yields no index → no `Produced` → no seal, the
   same "no partial seal" guarantee as a local dump.

3. **"argv unchanged" is true for tar *flags*, not for the I/O choreography.** Excludes,
   one-file-system, sparse, level/base pass through as argv. But snapshot seeding,
   `--index-file` capture, and the `/dev/null` estimate are filesystem-coupled operations
   interleaved with the process — a plain `exec` → `ssh` swap is not enough; the index
   capture re-routes from a local temp to stderr frames. The seam is an `Execution`
   (`Local`/`SSH`) that `Estimate`, `Backup`, `Restore` route through, localized to
   `archiver/gnutar` + the transport.

4. **The meter + checksum cannot move off the server.** The seal's `SHA256` must cover
   *exactly the bytes that land on the volume* — that is what keeps `nb verify`,
   `CopyRun`, and `nb sync` keyless and makes one run = N byte-identical copies. So
   wherever compress/encrypt run, `meter → volume` stays server-side, and the network cut
   lands **at or before the meter**.

## The configurable point: `client` vs `server`

Amanda sets the encryption/compression point per-dumptype; NBackup mirrors it. The
pipeline is a linear chain `tar → compress → encrypt → meter → volume`; the **network cut
is a single point** along it, chosen by the per-stage `client`/`server` setting:

| Cut location | `compress` | `encrypt.at` | On the wire | Win |
|---|---|---|---|---|
| after `tar` (**thin client**) | server | server | plaintext | works on any client with just tar |
| after `compress` | client | server | compressed plaintext | WAN bandwidth |
| after `encrypt` (**full client-side**) | client | client | ciphertext | bandwidth **+** plaintext never leaves client |

Because encrypt is downstream of compress and the cut is one point, `encrypt.at: client`
implies `compress.at: client` — you cannot encrypt server-side on the client's behalf
without first shipping plaintext. The loader rejects `encrypt.at: client` +
`compress.at: server`.

### The key never moves — three trust postures

The invariant is **NBackup never transports a secret; the key stays home on whoever runs
the crypto.** That single rule yields three postures, all already honored by the
keyless-server design:

| Posture | Key's home | Encrypt runs | Restore runs | Server can decrypt? |
|---|---|---|---|---|
| **Server-side** | server keyring | server | server | yes |
| **Asymmetric, centralized** | public key on client; private key escrowed / on server | client | wherever the private key is | operator's choice |
| **Untrusted server** (restic/Borg/tarsnap-style) | client only | client | **client** | **never** |

- **Asymmetric (gpg public-key)** is the clean default for client-side: backup needs *no
  secret at all* on the client — just the recipient public key — and the key-id travels
  inside the ciphertext, so restore resolves the private key from whichever host's keyring
  holds it. It decouples where-encryption-runs, where-the-key-lives, and where-restore-runs.
- **Untrusted server** is asymmetric or symmetric with the key born on the client and
  never shared: a compromised server yields only ciphertext and cannot be coerced into
  decrypting. Restore runs on the client via the documented stock-tools one-liner —
  NBackup needs no new server machinery, because the server is *already* keyless for every
  routine operation. With the key born on the client, symmetric is fine (nothing is
  shipped); the real gate on `encrypt.at: client` is **capability** (does the client have
  gpg/zstd) and **trust choice**, not symmetric-vs-asymmetric.

## Config surface

A host-keyed `hosts:` map carries the SSH connection (Amanda's `amanda-client.conf`
analogue); the encryption/compression point is per-dumptype. A DLE with no remote host
(`localhost`/empty/unlisted) reads locally exactly as today. **Secrets are never stored** —
SSH keys come from the operator's ssh config/agent; `identity_file` is a *path*, and a
literal key in config is rejected structurally (mirroring the `notify` stance).

```yaml
hosts:
  app01:
    ssh: { user: backup, identity_file: ~/.ssh/nbackup }
    state_dir: /var/lib/nbackup/snar         # host-level .snar root, all archivers
    archivers:
      gnutar: { tar_path: /usr/bin/tar }     # per-host binary, verified GNU tar over ssh

dumptypes:
  remote-secure:
    compress: { at: client }
    encrypt:  { scheme: gpg, at: client, recipient: backups@example.com }

sources:
  - { host: app01, path: /home, dumptype: remote-secure }
```

## What is shipped vs follow-on

**Shipped:** the `programs` execution model (`Local`/`SSH`, same-host adjacent stages
fused); the `hosts:` block and `compress`/`encrypt.at` selectors; estimate and dump over
SSH; `nb check`. Restore is opt-in onto a client (`nb recover --all --to host:path`) — for
an `encrypt.at: client` DLE the **decode runs on that client** (`gpg -d | … | tar -x`
fused there) so a client-only key never leaves the client; a server-side restore of a
client-only **symmetric** key fails fast (no escrow path), a client-side **public-key**
dump warns (its private key may be escrowed on the server). Default restore stays
server-side.

**Follow-on (structurally enabled, not wired):** driving the drill recoverability tiers
and file-level recover on the client.

**Testing:** the SSH paths are untested in CI (no sshd); the executor, quoting, config,
the all-local pipeline, and the client-side encrypt+decode round-trip (real gpg/gzip/tar
via the executor) are covered.

## Verify, drill, and recover across the key boundary

Once bytes land, a remote-sourced archive is **byte-identical to a local one** — same
`Entry`/`Placement`, same ciphertext `SHA256`, same member list; nothing records "this
came over SSH." So `nb verify` and drill selection are source-agnostic, and restore never
touches the client or its `.snar` (`recovery.Chain` rebuilds from the archive's dumpdir
with `--listed-incremental=/dev/null`) — a lost client only forces a future *dump* to a
full, never weakens recoverability.

The one seam is the **key, not the source**. Only `encrypt.at: client` in the
untrusted-server posture (no key on the server) perturbs the decode-needing tiers:

| Drill tier | Needs key? | Server-side, untrusted posture |
|---|---|---|
| checksum (ciphertext SHA) | no | runs — proves integrity |
| `--deep` structural / `chain` / `stock` | yes | cannot run server-side |

Decision: **key-absent is a coverage *skip*, not a failure** — the same skip-not-fail rule
an unloaded reel uses. The `pipeline-key` class splits into **key-absent-by-design** (an
`encrypt.at: client` DLE → coverage skip) and **key-wrong / stream-corrupt** (a real
failure). The most faithful proof of the untrusted-server posture runs the `stock` tier
**over SSH on the client** — the read-side twin of the dump transport, reusing the same
`Execution`. Escrowing the asymmetric private key near the server restores server-side
drillability, so client-side drilling is needed only for the strict "key lives nowhere but
the client" choice — making **escrow the documented recommendation**.

`nb recover` splits cleanly across the boundary. **Browse is keyless, catalog-only, and
works for every posture**: the member list is in the plaintext seal (captured at the tar
stage, upstream of compress/encrypt), so `recovery.BuildTree`/`Collect` walk the
as-of-date tree from the catalog alone. Only **extract** crosses the boundary. The honest
tension: keyless browse *requires* the plaintext seal, so the untrusted-server posture
protects file **contents, not file names** — the server sees the member list. Hiding *what
files exist* needs the deferred opt-in encrypted seal, which would disable keyless browse.
You can have one or the other, not both.

## Security recipe

- A dedicated backup SSH user with a `command=` forced command in `authorized_keys`.
- The root-read problem solved with a narrowly-scoped `sudo tar` or the forced command —
  never a root login.
- NBackup stores no SSH secret: keys come from the operator's ssh config/agent.
