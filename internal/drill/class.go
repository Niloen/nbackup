// Package drill is NBackup's recovery-drill orchestration: the layer that proves
// backups are actually *recoverable*, not merely intact. Where `nb verify` is the
// atomic, stateless integrity primitive (re-checksum, structural list), a drill
// selects a risk-biased subset of DLEs, exercises them end-to-end — checksum,
// structural list, a real point-in-time chain restore to scratch, or the documented
// stock-tools one-liner — and records the outcome in an inspectable ledger. It is
// NBackup's contribution of the "0 errors" digit of the 3-2-1-1-0 rule.
//
// This package holds the *pure* parts: the failure taxonomy and drill tiers
// (class.go), the recoverability ledger and its file persistence (ledger.go), and
// the risk-biased target selection (select.go). The engine performs the actual I/O
// (verify, restore, the WORM probe) and consumes these types; this package never
// imports the engine, so it stays a leaf the way retention/restore are.
package drill

import "fmt"

// Class is the failure taxonomy of a drill (or a verify it drives). Each class
// implies a distinct remediation, so a failed drill says not just "broken" but
// *what kind of broken* — which is the difference between a useful alert and a
// pager that just says "look at the backups".
type Class int

const (
	// ClassNone is a passing check (or one not yet run).
	ClassNone Class = iota
	// ClassIntegrity is a content fault: a checksum mismatch against the seal, or a
	// member list that no longer matches what was sealed — i.e. corruption.
	ClassIntegrity
	// ClassPipeline is a read-pipeline/key fault: decrypt, decompress, or tar failed
	// to turn the stored bytes back into a valid stream — a lost/wrong key, a scheme
	// or tar drift, a truncated object.
	ClassPipeline
	// ClassChain is an incremental-composition fault: the individual archives read
	// fine but the full+incrementals do not compose into a restorable filesystem.
	ClassChain
	// ClassMissing is a placement fault: the run (or a needed archive) has no copy
	// on the requested medium — a lost offsite copy, a gap in replication.
	ClassMissing
	// ClassSkipped is not a failure: the target could not be drilled *unattended*
	// because reaching its copy needs a human (a tape swap). Surfaced as an SLO
	// warning, never as a hard failure or non-zero exit.
	ClassSkipped
)

// String returns the short, stable token used in the ledger and reports.
func (c Class) String() string {
	switch c {
	case ClassNone:
		return "ok"
	case ClassIntegrity:
		return "integrity"
	case ClassPipeline:
		return "pipeline"
	case ClassChain:
		return "chain"
	case ClassMissing:
		return "missing"
	case ClassSkipped:
		return "skipped"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// ParseClass resolves a Class from its stable token (the inverse of String) — used
// to recover the failure class recorded in the ledger so a report can print its
// Remedy. An unknown token (including "ok") yields ClassNone.
func ParseClass(s string) Class {
	switch s {
	case "integrity":
		return ClassIntegrity
	case "pipeline":
		return ClassPipeline
	case "chain":
		return ClassChain
	case "missing":
		return ClassMissing
	case "skipped":
		return ClassSkipped
	default:
		return ClassNone
	}
}

// IsFailure reports whether the class counts as a drill failure — the outcomes that
// must fail the run loudly. A skip (needs operator) and a pass do not.
func (c Class) IsFailure() bool {
	switch c {
	case ClassIntegrity, ClassPipeline, ClassChain, ClassMissing:
		return true
	default:
		return false
	}
}

// Remedy is the one-line operator guidance for a failure class — what to actually
// do about it.
func (c Class) Remedy() string {
	switch c {
	case ClassIntegrity:
		return "data is corrupt on this copy — restore/re-replicate it from a known-good copy, and check the storage medium"
	case ClassPipeline:
		return "the archive would not decrypt/decompress/untar — verify the gpg key is present and the compressor/tar match the recorded scheme"
	case ClassChain:
		return "the incremental chain does not compose — force a fresh full for this DLE and check the incremental-state library"
	case ClassMissing:
		return "no copy on the drilled medium — fix replication (`nb sync`) so this DLE has the expected copies"
	case ClassSkipped:
		return "needs an operator to load a tape — drill this copy attended, or keep an online copy for unattended drills"
	default:
		return ""
	}
}

// Tier is how deeply a drill exercises a target, cheapest to strongest. A routine
// (especially offsite) drill picks a cheaper tier; an occasional drill proves the
// full restore. They form a ladder: each higher tier subsumes the proof of the ones
// below.
type Tier int

const (
	// TierChecksum re-hashes the stored payload against the seal — integrity only,
	// keyless, no decode. The cheapest check (today's `nb verify`).
	TierChecksum Tier = iota
	// TierStructural streams the archive through the real read pipeline
	// (decrypt → decompress → `tar -t`) and asserts the members match the seal —
	// proving the bytes are a *valid restorable stream* and exercising the key and
	// scheme, while writing nothing.
	TierStructural
	// TierChain performs a real point-in-time chain restore (full + incrementals,
	// deletion-faithful) into a scratch dir, then discards it — the strong proof.
	TierChain
	// TierStock restores via the documented stock one-liner (gpg/zstd/tar) instead of
	// NBackup's own restore code, validating the "recovery never requires NBackup"
	// promise.
	TierStock
)

// String returns the tier's config/CLI token.
func (t Tier) String() string {
	switch t {
	case TierChecksum:
		return "checksum"
	case TierStructural:
		return "structural"
	case TierChain:
		return "chain"
	case TierStock:
		return "stock"
	default:
		return fmt.Sprintf("tier(%d)", int(t))
	}
}

// ParseTier resolves a config/CLI tier token. An empty string defaults to
// structural — the no-write tier appropriate for routine (incl. offsite) drills.
func ParseTier(s string) (Tier, error) {
	switch s {
	case "", "structural":
		return TierStructural, nil
	case "checksum":
		return TierChecksum, nil
	case "chain":
		return TierChain, nil
	case "stock":
		return TierStock, nil
	default:
		return 0, fmt.Errorf("unknown drill tier %q (known: checksum, structural, chain, stock)", s)
	}
}
