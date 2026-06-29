package engine

import "github.com/Niloen/nbackup/internal/dumper"

// dump.go resolves a dumptype's write-side encode recipe from config — the per-dumptype
// compression and encryption scheme/opts and where each transform runs — for the producer
// (package dumper) to apply. The producer owns the tar source and the encode pipeline; the engine
// owns only this config resolution.
func (e *Engine) encodePlacement(dumpType string) dumper.EncodePlacement {
	compScheme, compOpts := e.compressionFor(dumpType)
	encScheme, encOpts := e.encryptionFor(dumpType)
	return dumper.EncodePlacement{
		CompressScheme: compScheme,
		CompressOpts:   compOpts,
		CompressClient: e.cfg.CompressionFor(dumpType).At == "client",
		EncryptScheme:  encScheme,
		EncryptOpts:    encOpts,
		EncryptClient:  e.cfg.EncryptionFor(dumpType).At == "client",
	}
}
