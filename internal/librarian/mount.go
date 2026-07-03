// mount.go — MountForRead: present and verify the volume holding an archive part so a reader can seek it — the ARCHITECTURE.md read path a tape/S3 source works through.
package librarian

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/media"
)

// MountForRead loads the named volume (a part's volume) and verifies its identity so
// the reader can seek it. A robotic library mounts the bay automatically; a
// single-drive station prompts the operator to swap the reel in; non-changer media
// are a no-op (a single addressable volume). Reading an archive that spans volumes
// calls this once per part, in order — a single drive holds only one tape at a time.
func (l *Librarian) MountForRead(volume string, epoch int) error {
	if err := l.mount(volume); err != nil {
		return err
	}
	return l.assertVolume(volume, epoch)
}

func (l *Librarian) mount(volume string) error {
	if !l.isChanger {
		return nil // address-identified: a single volume, nothing to mount
	}
	if l.mountedMatches(volume) {
		return nil // the right tape is already in the drive
	}
	// A hand-loaded drive prompts the operator to swap the needed cartridge in.
	if l.manual {
		return l.mountViaShelf(volume)
	}
	// A robot loads the slot holding the needed label, into whichever drive holds it. The
	// read drive is set so the read accessors (driveVol) act on that cartridge — a
	// multi-drive dump can leave the wanted tape in a drive other than 0.
	_, drive, ok, err := l.findSlot(volume)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: volume %q (holding a copy of an archive on %q) is not in the library; load it with `nb load %s <slot>`", ErrVolumeUnavailable, volume, l.medium, l.medium)
	}
	l.drive = drive
	return nil
}

// mountViaShelf loads the named reel on a single-drive station: if it is not already
// in the drive, it prompts the operator to swap it in, looping until the right tape
// is loaded or the operator aborts. Unattended, it returns an actionable error rather
// than blocking.
func (l *Librarian) mountViaShelf(volume string) error {
	for {
		if l.mountedMatches(volume) {
			return nil
		}
		switch _, out, err := l.requestSwap(volume, "", fmt.Errorf("need volume %q", volume), nil); {
		case err != nil:
			return err
		case out == swapUnattended:
			return fmt.Errorf("%w: medium %q needs volume %q in the drive (a copy of the archive is on it); load it and retry", ErrVolumeUnavailable, l.medium, volume)
		case out == swapAborted:
			return fmt.Errorf("%w: volume %q was not loaded into the %q drive", ErrVolumeUnavailable, volume, l.medium)
		}
	}
}

// assertVolume confirms the volume mounted on the medium matches the recorded
// identity (label name + epoch) of a part to read, before reading from it.
func (l *Librarian) assertVolume(volume string, epoch int) error {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	if err != nil {
		return err
	}
	if !labeled || lbl.Name != volume || lbl.Epoch != epoch {
		return fmt.Errorf("medium %q has volume %q (epoch %d) mounted, but an archive part is on %q (epoch %d) — mount it or run `nb rebuild`",
			l.medium, lbl.Name, lbl.Epoch, volume, epoch)
	}
	return nil
}

// readVolumeLabel reads the loaded volume's label name, if any. It is a no-op for
// address-identified media that carry no label.
func (l *Librarian) readVolumeLabel() (name string, labeled bool, err error) {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return "", false, nil
	}
	lbl, ok, err := lv.ReadLabel()
	return lbl.Name, ok, err
}

// mountedMatches reports whether the volume currently in the drive carries the given
// label. A read error, an empty drive, or address-identified media all count as no
// match — the caller then mounts the right bay or prompts for a swap.
func (l *Librarian) mountedMatches(label string) bool {
	name, labeled, err := l.readVolumeLabel()
	return err == nil && labeled && name == label
}
