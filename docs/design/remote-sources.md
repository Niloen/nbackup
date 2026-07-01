# Design note: remote sources over SSH

Status: **dump path implemented; client-side restore/drill is a follow-on.** Backing up
DLEs whose data lives on another host, with **no NBackup software on the client** — SSH is
the transport and stock GNU tar (plus, optionally, the compressor and gpg) is the "agent."
It also specifies the **Amanda-faithful configurable encryption/compression point**
(`client` vs `server`) and the three trust postures that fall out of it. See
[ARCHITECTURE.md](../../ARCHITECTURE.md) for the vocabulary (DLE / Archive / Run /
Archiver / incremental state / seal / filter / crypt / xfer.Meter).

**Ergonomics (implemented).** A source host is remote **by default** — anything but
`localhost` is backed up over SSH; `hosts:` is override-only, not what makes a host remote.
A top-level `ssh:` block sets global SSH defaults (Amanda's global auth) that a per-host
`hosts.<name>.ssh` block field-merges over. The incremental-state root is a host property,
`hosts.<name>.state_dir` (fleet default `state_dir:`, else `nbackup-state` under the
client's home) — shared by every archiver on the host, which the engine namespaces by type
(`<state_dir>/gnutar`). **`nb check`** is the amcheck
analogue: it verifies the server and every host — connecting to each remote client by
default (reachable, GNU tar, source readable, client tools, state_dir), or `--offline` to
just resolve and report — and exits non-zero on failure. Every probe runs through the
host's executor (`Local` for a localhost DLE, SSH otherwise), so local and remote checks
are the same code.

**What is implemented.** The unified execution model: a neutral `internal/hostexec`
package (`Executor` with `RunPipe` + filesystem ops; `Local` and `SSH` implementations)
injected into archivers and the compress/encrypt stages, so a backup is **one** pipeline
of program stages — `tar → compress → encrypt` — each carrying the host it runs on, with
same-host adjacent stages fused (`internal/archiveio.Writer.WriteArchive`). A `hosts:` config
block, the `compress`/`encrypt.at` `server|client` selectors, and estimate/dump over SSH are
in. Restore is opt-in onto a client (`nb recover --all --to host:path`); for an
`encrypt.at: client` DLE the **decode runs on that client** (`gpg -d | … | tar -x` fused
there), so an untrusted-server / client-symmetric archive restores with the key never
leaving the client. A server-side restore (or file-level recover) of a client-only
**symmetric** key **fails fast** (no escrow path); a client-side **public-key** dump warns
(its private key may be escrowed on the server). Default restore stays server-side.
**Follow-on (structurally enabled, not yet wired):** driving the drill recoverability tiers
and file-level recover on the client. The SSH paths are untested in CI (no sshd); the
executor, quoting, config, the all-local pipeline, **and the client-side encrypt+decode
round-trip** (real gpg/gzip/tar via the executor) are covered.

## The problem

A DLE is `host` + `path`, but today **`host` is metadata**: `path` is read from the
local filesystem (`SourcePath = item.DLE.Path`, opened by the local `tar`). Multi-host
backup is therefore fiction — you must run `nb` where the data lives, or NFS-mount it.
We want true remote sources without installing, versioning, or CVE-patching a custom
agent on every client. Amanda requires a client daemon (`amandad`) even when it tunnels
over SSH; NBackup can do better, because its archiver already orchestrates `tar` as a
child process — the process just needs to run on another host.

## The shape: SSH is the transport, GNU tar is the agent

For a DLE whose `host` resolves to a remote, NBackup runs the **same dump pipeline** it
runs locally, but the source `tar` (and optionally the compress/encrypt stages)
execute on the client via a single SSH command; the server receives the byte stream and
writes it to the landing volume exactly as for a local dump:

```
nb (server) ──ssh host──> tar --create --listed-incremental=<client .snar> -f - <path>
            <──────────── raw tar stream on stdout ────────────────────────────
            <──────────── --totals + framed member index on stderr ────────────
            └─ filter → crypt → meter(checksum) → volume → seal   (server-side, unchanged)
```

The client needs only **sshd + GNU tar** (and, for client-side transforms, the
compressor and gpg). No NBackup binary, no daemon, no open port beyond SSH. This is the
purest expression of "orchestrate external tools as processes," and it reuses the
archiver refactor verbatim: the `Archiver` interface already speaks only
`DLE`/`Level`/`BaseLevel`/`Exclude` and **owns its own incremental state**
(ARCHITECTURE.md:118 explicitly named remote sources as this seam's payoff).

## Load-bearing constraints (read before building)

These are the facts about `gnutar.Backup` that determine the mechanism. Skipping them
leads to a design that "passes argv unchanged" and then can't build the seal.

1. **The snapshot is the atom, and it lives on the client.** Decided: **stateful
   client.** A remote DLE's gnutar `state_dir` is a path *on the client* (Amanda's
   `GNUTAR-LISTDIR`, where amgtar keeps it). The `.snar` is seeded and updated in place
   on the client; it never crosses the wire. Consequences: **zero round-trip**;
   `HasBase` is a remote `test -f`; "nothing in catalog/engine/planner changes" is
   literally true; the failure mode of a lost/reimaged client is a **forced full on its
   next run** — the weakest possible failure (self-healing, already surfaced by the
   drill posture). This is Amanda's behavior. (Rejected for v1: a *stateless* client
   that round-trips the snar to centralize the precious state — more machinery for a
   degraded-to-full failure we already tolerate. Revisit only if "a lost client must
   never force a full" becomes a requirement.)

2. **`Backup` returns three things, and only one is the stdout stream.**
   `BackupResult{Uncompressed, FileCount, Members}`:
   - **data** → tar stdout (→ volume).
   - **`Uncompressed`** → parsed from `Total bytes written:` on tar **stderr**.
   - **`Members` + `FileCount`** → tar writes them to a local temp via `--index-file=`,
     read back by `readIndex`. **This feeds the seal**, and the seal's member list is
     what lets `nb recover`/`rebuild` browse without an index server
     (ARCHITECTURE.md:135) — it is not optional.

   Over SSH, tar gives exactly one stdout, so the member index and totals must return
   **out of band on stderr**. With the stateful client (no snar round-trip) this is the
   *whole* multiplexing problem and it is small: **stdout = raw tar stream; stderr =
   `--totals` line, then a framed member index** emitted only after tar exits. The
   server already reads tar stderr locally to scrape totals; over SSH it reads the same
   stderr and additionally de-frames the index. **Atomicity falls out for free** — the
   framed index arrives only on a clean tar exit, so a dropped connection yields no
   index → no `Produced` → no seal, the same "no partial seal" guarantee as a local
   dump.

3. **"argv unchanged" is true for tar *flags*, not for the I/O choreography.** Excludes,
   one-file-system, sparse, level/base all pass through as argv. But `seedSnapshot`
   (snapshot copy), `--index-file` (temp file), and the `/dev/null` estimate are
   **filesystem-coupled operations interleaved with the process**. A plain `exec` →
   `ssh` swap is not enough; the index capture re-routes from a local temp to stderr
   frames. The clean seam is a **runner** inside package `gnutar` with `local` and
   `ssh` implementations; `Estimate`, `Backup`, `Restore` each route through it. Still
   localized to `archiver/gnutar`; the generic layer is untouched.

4. **The meter + checksum cannot move off the server.** The seal's `SHA256` must cover
   *exactly the bytes that land on the volume* — that is what keeps `nb verify`,
   `CopyRun`, and `nb sync` **keyless** and makes one run = N byte-identical copies
   (ARCHITECTURE.md:230). So wherever compress/encrypt run, `meter → volume` stays
   server-side. The network cut therefore lands **at or before the meter**.

## The configurable point: `client` vs `server` (Amanda-faithful)

Amanda sets the encryption and compression *point* per-dumptype (`encrypt client` /
`encrypt server`; `compress client … / server …`). NBackup mirrors this. The pipeline
is a linear chain `tar → compress → encrypt → meter → volume`; the **network cut is a
single point** along it, and the per-stage `client`/`server` setting chooses where:

| Cut location | `compress` | `encrypt.at` | On the wire | Win |
|---|---|---|---|---|
| after `tar` (**thin client**) | server | server | plaintext | works on any client with just tar |
| after `compress` | client | server | compressed plaintext | WAN bandwidth |
| after `encrypt` (**full client-side**) | client | client | ciphertext | bandwidth **+** plaintext never leaves client |

**Validation rule:** because encrypt is downstream of compress and the cut is one point,
`encrypt.at: client` implies `compress.at: client` (you cannot encrypt server-side on the
client's behalf without first shipping plaintext). The loader rejects
`encrypt.at: client` + `compress.at: server`.

### The key never moves — three trust postures

The invariant is **NBackup never transports a secret; the key stays home on whoever
runs the crypto.** That single rule yields three postures, all already honored by the
keyless-server design:

| Posture | Key's home | Encrypt runs | Restore runs | Server can decrypt? |
|---|---|---|---|---|
| **Server-side** (today) | server keyring | server | server | yes |
| **Asymmetric, centralized** | public key on client; private key escrowed / on server | client | wherever the private key is | operator's choice |
| **Untrusted server** (restic/Borg/tarsnap-style) | client only | client | **client** | **never** |

- **Asymmetric (gpg public-key, `amgpgcrypt`-style)** is the clean default for client-side:
  backup needs *no secret at all* on the client — just the recipient public key — and
  the key-id travels inside the ciphertext, so restore resolves the private key from
  whichever host's keyring holds it. It **decouples** where-encryption-runs (client),
  where-the-key-lives (anywhere), and where-restore-runs (anywhere with the private key).
- **Untrusted server** is asymmetric or symmetric with the key born on the client and
  never shared: a compromised backup server yields only ciphertext and cannot be coerced
  into decrypting. Restore runs on the client (or an operator workstation with the key)
  via the documented stock-tools one-liner — NBackup needs no new server machinery for
  this, because the server is *already* keyless for every routine operation.
- This **corrects** the earlier "passphrase must stay server-side" worry: that held only
  when the key's home was the server (client-side crypto would have meant *shipping* the
  secret outward). With the key born on the client, symmetric is fine — nothing is
  shipped. The real gate on `encrypt.at: client` is **capability** (does the client have
  gpg/zstd) and **trust choice**, not symmetric-vs-asymmetric.

## Config surface

A host-keyed `hosts:` map carries the SSH connection (Amanda's
`amanda-client.conf` analogue); the encryption/compression point is per-dumptype.
A DLE with no remote host reads locally exactly as today (default unchanged).

```yaml
hosts:
  app01:
    ssh:
      user: backup
      port: 22
      identity_file: ~/.ssh/nbackup        # else the operator's ssh-agent/config
      options: ["-o", "StrictHostKeyChecking=accept-new"]
    state_dir: /var/lib/nbackup/snar       # host-level .snar root (Amanda's GNUTAR-LISTDIR), all archivers
    archivers:
      gnutar:
        tar_path: /usr/bin/tar             # per-host gnutar binary, verified GNU tar over ssh

dumptypes:
  remote-secure:
    compress:
      at: client                           # server (default) | client   (Amanda's compress directive)
    encrypt:
      scheme: gpg
      at: client                           # server | client          (Amanda's encrypt client/server)
      recipient: backups@example.com       # public key on the client; private key off-client

sources:
  - { host: app01, path: /home, dumptype: remote-secure }
```

- **`host`** is resolved against `hosts:`; an unlisted host (or `localhost`/empty) is
  **local** — today's behavior, no SSH.
- **Secrets are never stored.** SSH keys come from the operator's ssh config/agent
  (consistent with how cloud creds and gpg keys are already handled); `identity_file`
  is a *path*, not a key. A literal key in config is rejected structurally, mirroring
  the `notify` secrets stance.

## Implementation plan (phased, each slice independently shippable)

### Slice 1 — SSH transport for `tar` (thin client). Makes multi-host real.

- **Config:** `hosts:` map + per-host `ssh:` block (`config` package, validated at
  load; `KnownFields` for the new keys). Resolve `host → ssh config` once.
- **Runner seam in `archiver/gnutar`:** extract the `exec.Command` calls behind a
  `runner` with `local` (today's code verbatim) and `ssh` implementations. The `ssh`
  runner:
  - **Backup:** one ssh exec running a small remote wrapper — seed the snapshot
    (`cp L<base> L<level>`, or `rm -f L0` for a full) on the client, then
    `tar --create --listed-incremental=<client state_dir>/… --index-file=<client tmp>
    -f - …`; **data on stdout**; after tar exits, emit `--totals` and a **framed
    base64 member index on stderr**. gnutar parses the index from stderr frames instead
    of a local temp.
  - **Estimate:** the same `/dev/null --totals` estimate, run remotely.
  - **`Check`:** `tar --version` over SSH, asserting GNU tar (the local check, relocated).
  - **`HasBase`:** remote `test -f <client snap>`.
- **Engine wiring:** `archiverFor` becomes host-aware — a remote DLE builds a gnutar
  instance whose runner is the host's `ssh` runner and whose `state_dir` is the client
  path. `BackupRequest` is **unchanged** (`SourcePath` is the client-local path; the
  generic interface never learns "ssh"). The server-side pipeline
  (`filter → crypt → meter → volume`) is **unchanged** — it sees the plaintext tar
  stream exactly as for a local dump.
- **Failure:** unreachable host / dropped connection → actionable error, **no partial
  seal** (no framed index → no `Produced`).
- **Out of scope here:** compress/encrypt still run server-side; `encrypt.at`/`compress`
  accept only `server` until Slice 2.

### Slice 2 — client-side compress + encrypt (`compress.at: client`, `encrypt.at: client`).

- **Composed remote pipeline:** the single ssh exec becomes `tar … | zstd … | gpg -e -r
  <recipient>`. The engine composes the remote command from the gnutar tar args + the
  `filter` scheme args + the `crypt` scheme args (the only place these three meet for a
  remote dump).
- **`archiveio.Writer` "pre-transformed" mode:** when the produce side already delivers
  final bytes, the Writer **skips its internal filter/crypt wrap** and runs only
  `meter → volume`, while **still recording `Compress`/`Encrypt` scheme names** from config
  so restore reverses them from the artifact (ARCHITECTURE.md:236). This is the one
  cross-package change; it is small.
- **Capability `Check` on the client:** `zstd --version` / `gpg` over SSH (the
  server-side `filter.Check`/`crypt.Check` relocated for client-side DLEs).
- **Postures:** asymmetric (public key on client) is the documented default; symmetric
  with a client-resident passphrase is supported (key born on the client, never shipped).
- **Progress fidelity:** the uncompressed `xfer.Counter` lives on the tar→compressor
  stream, now on the client, so the server sees only compressed/ciphertext bytes;
  per-DLE % falls back to **compressed-against-estimate**. Documented, acceptable.
- **Validation:** reject `encrypt.at: client` + `compress.at: server`.

### Slice 3 — restore to the client (`nb restore --to host:path`) for the untrusted-server posture.

- Symmetric counterpart of the dump transport: stream the restore chain to
  `ssh host tar --extract -C <path>`, with **decrypt + decompress running on the
  client** (the key need never touch the server). Until this lands, the
  untrusted-server posture restores via the README stock-tools one-liner
  (`gpg -d | zstd -d | tar -x`) run on the client — which already works, since the
  server hands out ciphertext.

## Verification & drills

**Verify and drill are source-agnostic by construction.** Once bytes land and the seal is
written, a remote-sourced archive is byte-identical to a local one — same `Entry`/
`Placement`, same ciphertext `SHA256`, same member list; nothing records "this came over
SSH." So `nb verify` (checksum + `--deep` structural) and drill selection are unchanged.
And **restore never touches the client or its `.snar`** (`restore.Chain` rebuilds from the
*archive's* dumpdir with `--listed-incremental=/dev/null`), so a lost client never weakens
recoverability — it only forces a future *dump* to a full. The stateful-client choice costs
nothing on the drill axis.

**The one seam is the key, not the source.** Only `encrypt.at: client` in the
untrusted-server posture (no key on the server) perturbs verify/drill, and only the
decode-needing tiers:

| Drill tier | Needs key? | Server-side, untrusted posture |
|---|---|---|
| checksum (ciphertext SHA) | no | runs, unchanged — proves integrity |
| `--deep` structural / `chain` / `stock` | yes | cannot run server-side |

Decision (specified; the classification is the follow-on): **key-absent is a coverage
*skip*, not a failure** — the same attended/unattended skip-not-fail rule a to-be-loaded
reel already uses (ARCHITECTURE.md "unattended … skips (not fails)"). The `pipeline-key`
class splits into **key-absent-by-design** (a DLE whose `encrypt.at: client` marks the key
client-only → coverage skip) and **key-wrong / stream-corrupt** (a real failure). A
client unreachable for the recoverability tier is likewise a skip.

**The faithful recoverability proof for the untrusted-server posture runs on the client.**
The `stock` tier (the gpg/zstd/tar one-liner that proves recovery needs no NBackup) run
over SSH on the client — pulling ciphertext to where the key is — is the *most* faithful
drill of that posture, not a degraded one. It is the read-side twin of the dump transport
(server streams raw parts → client decode), and reuses the same `Executor`. Default routine
drills to the keyless checksum tier and **sample** the client tiers (pulling full
ciphertext costs egress, paced by the read `xfer.Limiter`, forecast by cost).

### Key → execution-host, with escrow as the pressure valve

| Posture | Key's home | Recoverability tier runs |
|---|---|---|
| server-side | server keyring | server (today) |
| asymmetric, **escrowed** | private key on/near the server | **server** — collapses to today's path |
| untrusted server, key on live client only | client | over SSH **on the client**, reachability-gated |

Escrowing the asymmetric private key near the server restores server-side drillability, so
client-side SSH drilling is needed *only* for the strict "key lives nowhere but the client"
choice — making **escrow the documented recommendation**. The drill posture audit
(`nb report`) should flag a client-key DLE whose client is long unreachable: its
recoverability is unverified and, if the key was client-only, possibly unrecoverable.

## Recover

`nb recover` splits cleanly across the key boundary. **Browse is keyless, catalog-only,
and works for every posture**: the member list is in the plaintext seal (captured at the
tar stage, upstream of compress/encrypt — populated even for a client-side-encrypted
dump), so `recovery.BuildTree`/`Collect` walk the as-of-date tree from the catalog alone —
no client, no key, even for an untrusted-server archive. Only **extract** crosses the
boundary: whole-DLE restore can target a client opt-in (`nb recover --all --to host:path`).
For a server-key/asymmetric DLE decode stays server-side and only `tar -x` runs on the
client; for an `encrypt.at: client` DLE the **whole read pipeline** (`gpg -d | … | tar -x`)
runs on the client, so a client-only key decrypts where it lives. File-level
recover-to-client is the remaining follow-on (it decodes server-side, so a client-symmetric
key fails fast there).

**The honest tension: keyless browse ⟂ filename secrecy.** Keyless browse *requires* the
plaintext seal, so the untrusted-server posture protects file **contents, not file names** —
the server (and anyone reading the medium) sees the member list, and the dump even ships
those filenames to the server to populate the seal. Hiding *what files exist* needs the
deferred opt-in encrypted seal, which would **disable** `nb recover`'s keyless browse
(forcing the client into the loop even to list). You can have one or the other, not both.

## Security recipe (document with Slice 1)

- A dedicated backup SSH user; a `command=` **forced command** in `authorized_keys`
  restricting the key to the dump invocation.
- The root-read problem (backing up a full tree usually needs root) solved with a
  **narrowly-scoped `sudo tar`** or the forced command — never a root login.
- NBackup stores **no SSH secret**: keys come from the operator's ssh config/agent.

## Constraints & acceptance

- **Source-side only** — lives in the archiver/engine transport, never the medium layer
  (media stay medium-neutral; the generic archiver/engine/planner layer never says
  "ssh", which stays inside `archiver/gnutar` + engine wiring, like `tar`/`.snar` today).
- **Default unchanged:** a DLE with no remote host reads locally, byte-for-byte as today.
- **Acceptance:** a remote DLE dumps over SSH with no NBackup software on the client;
  estimate, exclude, one-file-system, level/incremental, and seal/verify behave
  identically to a local dump; incremental chains restore correctly across runs (the
  client-resident `.snar` updates in place); a client-side-encrypted remote dump puts
  only ciphertext on the wire and restores with the documented stock-tools one-liner;
  connection loss fails cleanly with no partial seal.
- **Verify** per repo conventions: `gofmt -l`, `go vet ./...`, `go test -race ./...`;
  tests stub the SSH runner or use a loopback `ssh localhost` guarded by availability
  (skip when absent, like the GNU-tar skips), scheme `none`.
```
