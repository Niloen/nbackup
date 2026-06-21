// Package backup orchestrates a single run: it executes a plan into a new,
// sealed slot on the landing medium and updates catalog state.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/archive"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/state"
)

// Options controls a run.
type Options struct {
	Catalog string
	Logf    func(format string, args ...any) // progress logging; may be nil
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Run executes the plan, producing one sealed slot. It follows the PRD
// sealing workflow: write archives, write manifests, verify checksums, then
// write SLOT.json to seal the slot, after which it is immutable.
func Run(cfg *config.Config, st *state.State, p *planner.Plan, opts Options) (*slot.Slot, error) {
	if cfg.Landing.Media != "local-disk" {
		return nil, fmt.Errorf("landing medium %q is not implemented in this version (only local-disk)", cfg.Landing.Media)
	}

	slotID := slot.ID(p.Date)
	dir := filepath.Join(opts.Catalog, slotID)
	if slot.IsSealed(dir) {
		return nil, fmt.Errorf("a sealed slot already exists for %s; slots are immutable", slot.DateString(p.Date))
	}
	if err := os.MkdirAll(filepath.Join(dir, slot.DirArchives), 0o755); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	s := &slot.Slot{
		ID:        slotID,
		Date:      slot.DateString(p.Date),
		CreatedAt: now,
		Status:    slot.StatusOpen,
		Generator: "nbdump",
	}
	manifest := &slot.Manifest{SlotID: slotID}
	checksums := map[string]string{}

	for _, item := range p.Items {
		levelTag := fmt.Sprintf("L%d", item.Level)
		fileName := fmt.Sprintf("%s-%s.tar.zst", item.Name, levelTag)
		relPath := filepath.ToSlash(filepath.Join(slot.DirArchives, fileName))
		outFile := filepath.Join(dir, slot.DirArchives, fileName)

		var base archive.Snapshot
		if item.Level >= 1 {
			base = st.DLE(item.Name).BaseSnapshot
			if base == nil {
				return nil, fmt.Errorf("DLE %s: incremental requested but no base snapshot exists", item.Name)
			}
		}

		opts.logf("archiving %s (%s) from %s", item.Name, levelTag, item.Source.Path)
		res, err := archive.Create(archive.CreateOptions{
			SourcePath: item.Source.Path,
			OutFile:    outFile,
			Base:       base,
		})
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", item.Name, err)
		}

		s.Archives = append(s.Archives, slot.Archive{
			DLE:          item.Name,
			Host:         item.Source.Host,
			Path:         item.Source.Path,
			Level:        item.Level,
			File:         relPath,
			Compressed:   res.Compressed,
			Uncompressed: res.Uncompressed,
			FileCount:    res.FileCount,
			SHA256:       res.SHA256,
			BaseSlot:     item.BaseSlot,
		})
		s.TotalBytes += res.Compressed
		checksums[relPath] = res.SHA256
		manifest.Archives = append(manifest.Archives, slot.ArchiveFiles{
			DLE:   item.Name,
			Level: item.Level,
			Files: res.Entries,
		})

		// Update planner state.
		d := st.DLE(item.Name)
		if item.Level == 0 {
			d.LastFullDate = s.Date
			d.LastFullSlot = slotID
			d.BaseSnapshot = res.Snapshot
		}
		d.Runs = append(d.Runs, state.RunRecord{Date: s.Date, Slot: slotID, Level: item.Level})

		opts.logf("  %d files, %s compressed", res.FileCount, human(res.Compressed))
	}

	// Write manifest and checksums.
	if err := manifest.Write(dir); err != nil {
		return nil, err
	}
	if err := slot.WriteChecksums(dir, checksums); err != nil {
		return nil, err
	}

	// Verify checksums against what is on disk before sealing.
	opts.logf("verifying %d archive checksum(s)", len(checksums))
	for rel, want := range checksums {
		got, err := archive.HashFile(filepath.Join(dir, rel))
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", rel, err)
		}
		if got != want {
			return nil, fmt.Errorf("checksum mismatch for %s before sealing", rel)
		}
	}

	// Seal: write SLOT.json last.
	s.Status = slot.StatusSealed
	s.SealedAt = time.Now().UTC()
	if err := s.Write(dir); err != nil {
		return nil, err
	}
	if err := st.Save(opts.Catalog); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}
	return s, nil
}

func human(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
