// write.go — make-writable: PrepareWrite runs the label protocol (pool/epoch/appendable/auto-label) before any write — ARCHITECTURE.md's overwrite and wrong-tape protection.
package librarian

import (
	"errors"
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// PrepareWrite enforces the label protocol on the loaded volume before writing,
// prompting a swap when the medium is a single drive whose loaded reel won't do.
// Robotic libraries and unattended runs fall straight through to the underlying
// error. It returns the accepted volume's identity to record in a placement.
func (l *Librarian) PrepareWrite(appendable bool, expect string, now time.Time, logf Logf) (string, int, error) {
	name, epoch, err := l.verifyWritable(appendable, now)
	if err == nil {
		l.reserve(name) // the loaded tape is writable; claim it against concurrent recycling
		return name, epoch, nil
	}
	if !isReloadable(err) {
		return "", 0, err
	}
	// The loaded volume won't do, but loading another might. A robotic library rolls
	// to its next writable slot on its own (auto-labeling a blank if enabled) — the
	// same selection Advance does mid-span, applied here so a run can also *start* on
	// a blank/empty slot (e.g. nothing loaded yet, or the loaded reel already holds a
	// run on a one-run-per-tape medium) rather than failing with "no tape loaded".
	if l.isChanger && !l.manual {
		name, epoch, _, aerr := l.Advance(appendable, map[string]bool{}, expect, now, logf)
		if aerr != nil {
			// A full pool of still-protected tapes is the rotation's fail-loud verdict —
			// more actionable than the loaded volume's bare "won't do" reason, so surface
			// it. Any other advance failure (out of slots) keeps the original reason, which
			// is more specific for a blank/foreign loaded tape ("label it first").
			if errors.Is(aerr, ErrAllVolumesProtected) {
				return "", 0, aerr
			}
			return "", 0, err
		}
		return name, epoch, nil
	}
	// A manual (hand-loaded) drive prompts the operator to load a writable cartridge —
	// the same swap loop the spanning roll uses (swapForWrite), with this path's own
	// unattended/aborted wording layered over the "why the loaded reel won't do" cause.
	if l.manual {
		name, epoch, _, serr := l.swapForWrite(appendable, map[string]bool{}, expect, err,
			func(cause error) error {
				return fmt.Errorf("%v (load a writable volume into the drive and retry)", cause)
			},
			func(cause error) error { return fmt.Errorf("%v (no volume loaded)", cause) },
			now, logf)
		return name, epoch, serr
	}
	return "", 0, err
}

// resolveLabel reads the loaded labeled volume's label, auto-labeling a blank one when
// allowed, and returns the resolved label. It isolates the read-and-maybe-write step
// (the cases a swap can fix become a reloadable error: no volume, foreign data, blank
// without auto-label) from the pool/epoch/appendable policy that verifyWritable layers
// on top.
func (l *Librarian) resolveLabel(lv media.Labeled, now time.Time) (record.Label, error) {
	lbl, labeled, err := lv.ReadLabel()
	switch {
	case errors.Is(err, media.ErrNoVolume):
		// A bare fact: the surfaces attach the advice their context allows — the
		// interactive prompt enumerates insertable options itself, and the
		// unattended write wrapper says how to retry (where the run's lock is
		// released, so `nb label` is actually runnable).
		return record.Label{}, reloadableErr("medium %q has no volume loaded", l.medium)
	case errors.Is(err, media.ErrForeignVolume):
		return record.Label{}, reloadableErr("medium %q holds non-NBackup data; refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", l.medium, l.medium)
	case err != nil:
		// Unparseable/corrupt header (e.g. "parse file header: invalid character …"):
		// the volume is not a recognizable NBackup tape, so treat it like foreign data —
		// a clear refusal a swap can resolve, not the raw decoder error.
		return record.Label{}, reloadableErr("medium %q holds unrecognized or corrupt data (%v); refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", l.medium, err, l.medium)
	case !labeled: // blank volume
		name := l.autoLabelName(now)
		if !l.autoLabel {
			// auto_label:false forbids labeling UNATTENDED; an interactive
			// operator's explicit yes is the authorization it withholds. Declining
			// re-prompts for a different reel (reloadable) rather than failing the
			// whole run — only the unattended case keeps the fail-fast (see
			// errBlankNeedsLabel: re-prompting cannot fix it without a human).
			if l.op == nil {
				return record.Label{}, reloadable{fmt.Errorf("medium %q has a blank/unlabeled reel loaded: %w", l.medium, errBlankNeedsLabel)}
			}
			if !l.op.ConfirmLabel(l.medium, name) {
				return record.Label{}, reloadableErr("medium %q: the blank reel was not labeled; insert a different tape", l.medium)
			}
		}
		lbl = record.Label{Name: name, Pool: l.medium, Epoch: 1, WrittenAt: now}
		if err := lv.WriteLabel(lbl); err != nil {
			return record.Label{}, err
		}
	}
	return lbl, nil
}

// verifyWritable enforces the label protocol before writing to a medium. Address-
// identified media (disk, s3) are trusted by their path/bucket and return that name
// with epoch 0. For labeled (tape) media it refuses a foreign, blank (unless
// autoLabel), wrong-pool, wrong, or relabeled-since volume — the overwrite and
// wrong-tape protection — records the accepted label, and returns the volume
// identity to record in a placement.
func (l *Librarian) verifyWritable(appendable bool, now time.Time) (volName string, epoch int, err error) {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return "", 0, nil // address-identified: no label — the medium is its own volume
	}
	lbl, err := l.resolveLabel(lv, now)
	if err != nil {
		return "", 0, err
	}
	if lbl.Pool != "" && lbl.Pool != l.medium {
		return "", 0, reloadableErr("mounted volume %q belongs to pool %q, not %q — wrong volume", lbl.Name, lbl.Pool, l.medium)
	}
	// Relabeled-since check: a tape we know whose epoch advanced means the catalog
	// is stale for it. (A genuinely different tape is not an error — that is what
	// loading another tape in the pool is for.)
	if known, ok := l.cat.Volume(lbl.Name); ok && known.Label.Epoch != lbl.Epoch {
		return "", 0, fmt.Errorf("volume %q was relabeled since the catalog was updated (epoch %d mounted vs %d cached); run `nb rebuild`", lbl.Name, lbl.Epoch, known.Label.Epoch)
	}
	// One-run-per-tape media refuse to append onto a tape that already holds a run.
	if !appendable {
		if held := l.cat.RunsOnLabel(lbl.Name); len(held) > 0 {
			return "", 0, reloadableErr("medium %q is not appendable and volume %q already holds %d run(s); load a fresh volume", l.medium, lbl.Name, len(held))
		}
	}
	if err := l.cat.RecordVolume(lbl); err != nil {
		return "", 0, err
	}
	l.learnLoadedBarcode(lbl.Name)
	// Snapshot the accepted reel's stored fill for Remaining() — a tape cannot see
	// its own, so the catalog's figure (maintained at record time, see
	// VolumeRecord.Used) is the truth (see volumeFill). Taken once per accept — the
	// one choke point every write path (start, roll, swap, recycle) funnels through.
	if v, ok := l.cat.Volume(lbl.Name); ok {
		l.fill.accept(lbl.Name, lbl.Epoch, v.Used)
	}
	return lbl.Name, lbl.Epoch, nil
}

// autoLabelName picks a unique auto-label for a blank volume: medium-date, or
// medium-date-N when an earlier name is taken — so a single run that rolls across
// several blank tapes (a filling library) does not stamp every fresh tape with the
// same name (which would collide in the catalog, keyed by label name).
func (l *Librarian) autoLabelName(now time.Time) string {
	base := fmt.Sprintf("%s-%s", l.medium, record.DateString(now))
	name := base
	for n := 2; ; n++ {
		if _, ok := l.cat.Volume(name); !ok {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}
