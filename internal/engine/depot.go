package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// depot is the engine's medium resolution service: it answers "give me an opened
// medium" — the lazily opened landing volume (with its one-time catalog
// bootstrap), a fresh handle on any other configured medium, the librarian over a
// medium's volumes, and the per-medium write knobs (part size, shared bandwidth
// cap). It never touches hosts or tools; that is the toolchain's half.
type depot struct {
	cfg         *config.Config
	cat         *catalog.Catalog
	landingName string        // name of the medium new dumps land on
	landingDef  config.Media  // its definition
	profile     media.Profile // the landing medium's capacity profile
	minAge      time.Duration // the landing medium's retention floor

	vol     media.Volume // the landing volume, opened lazily by landing() — nil until a command actually needs the medium
	volOnce sync.Once    // guards the one-time landing open + catalog bootstrap
	volErr  error        // remembers a failed landing open so retries don't re-probe

	limiters map[string]*ratelimit.Limiter // per-medium bandwidth cap (nil entry = uncapped); shared so a medium's concurrent streams share one budget
	op       librarian.Operator            // optional: handles manual tape swaps (nil = unattended)

	// writeHeld is the run window's medium-ownership table: the media the open window is
	// writing (its landings + holding disks). OpenForWrite takes the claim, the handle's
	// Close releases it, and OpenForRead/OpenAdmin refuse a held medium — so a window
	// reader fails over to another copy instead of touching a drive the run is writing:
	// the medium-access half of the window's one-owner-per-medium split (see
	// docs/design/catalog-window.md). Claims are made before the window's producers start
	// and released after they have joined, so the map is never written concurrently with a
	// read; the process runs one command, so no lock is needed.
	writeHeld map[string]bool
}

// OpenForRead opens a medium for reading archive data — the only mint of the librarian's
// read face. Refused while a run window write-owns the medium; the clerk's copy selection
// treats that like any unavailable copy and fails over. Read opens are not tracked (many
// readers may share a medium), so Close is a no-op kept for lifecycle symmetry.
func (d *depot) OpenForRead(name string) (librarian.ReadMedium, error) {
	if d.writeHeld[name] {
		return nil, fmt.Errorf("medium %q is write-owned by the running window", name)
	}
	lib, _, err := d.buildLibrarian(name)
	if err != nil {
		return nil, err
	}
	return readMedium{lib}, nil
}

// OpenForWrite opens a medium for run authoring and takes the window's exclusive write
// claim — the only mint of the librarian's write face. A medium already held is a wiring
// bug (two windows, or one medium as both landing and holding disk). Close releases the
// claim. The medium's config definition rides along for the write-path knobs
// (appendable, min_age).
func (d *depot) OpenForWrite(name string) (librarian.WriteMedium, config.Media, error) {
	if d.writeHeld[name] {
		return nil, config.Media{}, fmt.Errorf("medium %q is already write-claimed by this run", name)
	}
	lib, def, err := d.buildLibrarian(name)
	if err != nil {
		return nil, config.Media{}, err
	}
	d.writeHeld[name] = true
	return &writeMedium{Librarian: lib, name: name, d: d}, def, nil
}

// OpenAdmin opens a medium's operator face (label, load, inventory, introspection) —
// refused while a run window write-owns it, for the same reason reads are.
func (d *depot) OpenAdmin(name string) (librarian.AdminMedium, config.Media, error) {
	if d.writeHeld[name] {
		return nil, config.Media{}, fmt.Errorf("medium %q is write-owned by the running window", name)
	}
	lib, def, err := d.buildLibrarian(name)
	if err != nil {
		return nil, config.Media{}, err
	}
	return adminMedium{lib}, def, nil
}

// readMedium / writeMedium / adminMedium narrow the librarian to one face. The embedded
// *Librarian satisfies the face's methods; the static interface type is what keeps the
// rest of the surface out of reach at the call sites.
type readMedium struct{ *librarian.Librarian }

func (readMedium) Close() error { return nil }

type adminMedium struct{ *librarian.Librarian }

func (adminMedium) Close() error { return nil }

type writeMedium struct {
	*librarian.Librarian
	name   string
	d      *depot
	closed bool
}

func (m *writeMedium) Name() string { return m.name }

// Close releases the window's write claim. Idempotent — a window may close its
// PreparedWriters through deferred releases that can run more than once on error paths.
func (m *writeMedium) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	delete(m.d.writeHeld, m.name)
	return nil
}

// ErrUnknownMedium marks a medium name absent from the current config — a copy
// recorded in the catalog on a medium this config does not define. It is a scoping
// condition, not corruption: verify skips such a copy rather than failing it.
var ErrUnknownMedium = errors.New("unknown medium")

// landing opens the depot's own (landing) volume on first use and bootstraps the
// catalog against it, memoizing the handle. Opening is deferred to here — never done
// at construction — so a catalog-only command (report, run, dle, status) never
// touches the medium: no bucket LIST for a cloud landing, no physical mount for a
// tape. The commands that genuinely need the medium (dump, restore, verify, prune,
// rebuild, and `nb check`, which probes it on purpose) reach it through this, so the
// open error surfaces at the point of use rather than on every invocation. The
// catalog bootstrap (EnsureFresh) is a no-op once the local cache exists, so this is
// cheap on every call after the first; an open failure is remembered so a retry
// within the same process does not re-probe an unreachable store.
func (d *depot) landing() (media.Volume, error) {
	d.volOnce.Do(func() {
		vol, err := media.OpenVolume(d.landingDef.Type, media.Options(d.landingDef.Params))
		if err != nil {
			// Opening a cloud volume lists the bucket, so this is where absent SDK
			// credentials or an unreachable store first surface. Name the medium and
			// point at the credential source rather than leaking the raw provider error.
			if d.landingDef.Type == "cloud" {
				err = fmt.Errorf("cannot reach landing medium %q: %w\n(a cloud store reads its credentials from the SDK environment: AWS_*, GOOGLE_APPLICATION_CREDENTIALS, or AZURE_*)", d.landingName, err)
			} else {
				err = fmt.Errorf("cannot open landing medium %q: %w", d.landingName, err)
			}
			d.volErr = err
			return
		}
		// The one-time bootstrap scan indexes whatever the landing medium already
		// holds; once the local catalog cache exists, planning/listing is fully
		// offline. A cloud store fails here only when its SDK credentials are absent or
		// it is unreachable — surface that legibly with the medium named, rather than
		// the raw provider SDK error.
		if err := d.cat.EnsureFresh(d.landingName, vol); err != nil {
			hint := ""
			if d.landingDef.Type == "cloud" {
				hint = " — a cloud store reads its credentials from the SDK environment (AWS_*, GOOGLE_APPLICATION_CREDENTIALS, or AZURE_*); set them, or run where the catalog cache already exists"
			}
			d.volErr = fmt.Errorf("cannot reach landing medium %q to index existing backups: %w%s", d.landingName, err, hint)
			return
		}
		d.vol = vol
	})
	return d.vol, d.volErr
}

// mediumVolume returns a Volume for the named medium. For the depot's own
// medium it returns the already-open handle (own=true) so that handle's cached
// state stays coherent and the catalog — which caches exactly this medium — can be
// rebuilt against it; any other medium is opened as a fresh handle. This is the
// single place that distinguishes "my medium" from the rest, so the rest of the
// engine never compares medium names itself.
func (d *depot) mediumVolume(name string) (vol media.Volume, def config.Media, own bool, err error) {
	if name == d.landingName {
		v, err := d.landing()
		if err != nil {
			return nil, config.Media{}, false, err
		}
		return v, d.landingDef, true, nil
	}
	md, ok := d.cfg.Media[name]
	if !ok {
		return nil, config.Media{}, false, fmt.Errorf("%w %q", ErrUnknownMedium, name)
	}
	v, err := media.OpenVolume(md.Type, media.Options(md.Params))
	return v, md, false, err
}

// buildLibrarian builds a librarian for a configured medium's open volume — the shared
// core behind the three Open faces. For the depot's own medium it wraps the already-open
// landing handle, so its cached state stays coherent and the catalog — which caches
// exactly this medium — can be rebuilt against it.
func (d *depot) buildLibrarian(name string) (lib *librarian.Librarian, def config.Media, err error) {
	vol, md, _, err := d.mediumVolume(name)
	if err != nil {
		return nil, config.Media{}, err
	}
	return librarian.New(vol, name, d.cat, d.op, d.cfg.AutoLabel, d.cfg.MinAgeFor(md)), md, nil
}

// limiter returns a medium's shared bandwidth cap (nil = uncapped).
func (d *depot) limiter(medium string) *ratelimit.Limiter { return d.limiters[medium] }

// partSizeFor resolves a medium's per-part chunk bound: the explicit part_size param
// when set, otherwise the medium type's registered default (10 GB for cloud, none for
// disk/tape). An explicit value must be at least two header blocks so a part can carry
// payload, and must not exceed the type's registered maximum — the cloud cap keeps a
// part-object's multipart upload below the object store's 10000-part ceiling so the
// knob can never silently reproduce the original over-large-object failure.
func (d *depot) partSizeFor(medium string) (int64, error) {
	md, ok := d.cfg.Media[medium]
	if !ok {
		return 0, fmt.Errorf("unknown medium %q", medium)
	}
	policy := media.PartSizeFor(md.Type)
	s := md.Params["part_size"]
	if s == "" {
		return policy.Default, nil // 0 (unbounded single part) unless the type defaults one
	}
	n, err := sizeutil.ParseBytes(s)
	if err != nil {
		return 0, fmt.Errorf("medium %q part_size: %w", medium, err)
	}
	if n < 2*record.HeaderBlock {
		return 0, fmt.Errorf("medium %q part_size %s is too small; use at least %s", medium, sizeutil.FormatBytes(n), sizeutil.FormatBytes(2*record.HeaderBlock))
	}
	if policy.Max > 0 && n > policy.Max {
		return 0, fmt.Errorf("medium %q part_size %s exceeds the maximum %s: %s", medium, sizeutil.FormatBytes(n), sizeutil.FormatBytes(policy.Max), policy.MaxNote)
	}
	return n, nil
}
