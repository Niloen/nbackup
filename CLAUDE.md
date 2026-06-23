# NBackup — agent guide

NBackup is an Amanda-inspired, slot-based backup system in Go (module
`github.com/Niloen/nbackup`). It orchestrates GNU tar + a compressor as child
processes and produces immutable, self-describing artifacts.

**Read [ARCHITECTURE.md](ARCHITECTURE.md) first** — it is the internal map: the
concept vocabulary (DLE / Run / Slot / Archive / Cycle / Medium=Volume /
catalog Entry+Placement / Label / Bay), the load-bearing decisions and their
rationale, and the conventions below in full. [README.md](README.md) is the
user-facing front page; [PRD.md](PRD.md) is the product vision.

## Always

- **Commit only when the user explicitly asks.** **Never push** (no credentials).
  End commit messages with the `Co-Authored-By` trailer.
- **Amanda-faithful, greenfield:** research upstream before inventing; no
  back-compat shims or migrations; don't add concepts/layers speculatively.
- **Verify every change:** `gofmt -l`, `go vet ./...`, `go test -race ./...`.
- **Test env has no `zstd`** — use codec `none` in tests (tar/gzip/nice present).
- Keep the generic media/changer layer **medium-neutral** (`bays`, `volume_size`,
  `nb changer`); tape specifics stay in the `tape` package.
