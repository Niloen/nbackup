# NBackup — agent guide

NBackup is an Amanda-inspired, run-based backup system in Go (module
`github.com/Niloen/nbackup`). It orchestrates GNU tar + a compressor as child
processes and produces immutable, self-describing artifacts.

**Read [ARCHITECTURE.md](ARCHITECTURE.md) first** — it is the internal map: the
concept vocabulary (DLE / Run / Archive / Cycle / Medium=Volume /
catalog Entry+Placement / Label / Bay), the load-bearing decisions and their
rationale, and the conventions below in full. [README.md](README.md) is the
user-facing front page; [PRD.md](PRD.md) is the product vision.

## Always

- **Commit only when the user explicitly asks.** **Never push** (no credentials).
  End commit messages with the `Co-Authored-By` trailer.
- **Amanda-faithful, greenfield:** research upstream before inventing; no
  back-compat shims or migrations; don't add concepts/layers speculatively.
- **Verify every change:** `gofmt -l`, `go vet ./...`, `go test -race ./...`.
- **Test env has no `zstd`** — use scheme `none` in tests (tar/gzip/nice present).
- Keep the generic media/changer layer **medium-neutral** (`slots`, `drives`,
  `volume_size`, `nb medium`); tape specifics stay in the `tape` package.
- Keep the generic `archiver`/catalog/engine layer **archiver-neutral** (`Archiver`,
  `BackupRequest{DLE, Level, BaseLevel}`, `HasBase`, "incremental state", the host-level
  `state_dir` that roots it); GNU tar specifics (`.snar`, snapshots, `tar_path`) stay in
  `archiver/gnutar`. A archiver owns its own incremental state, not the catalog; *where* that
  state lives is a host property (`state_dir`, shared across archivers, engine-namespaced
  by type), not an archiver option. The concurrency unit is a **worker**
  (`parallelism.workers`); "archiver" means only the plugin.
