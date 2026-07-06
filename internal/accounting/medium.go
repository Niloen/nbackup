package accounting

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
)

// MediumInfo is a per-medium summary for catalog visibility (`nb medium`): what
// the medium is, how much it holds against its capacity, and (for labeled media)
// the volume currently associated with it in the catalog.
type MediumInfo struct {
	Name     string
	Type     string
	Runs     int
	Used     int64
	Capacity int64  // 0 = unbounded
	Volume   string // label name when the pool holds exactly one volume; "" otherwise (address-identified media, or several volumes — see Volumes)
	Epoch    int
	Volumes  int // labeled volumes in the pool; 0 for address-identified media (disk, s3)
}

// MediumAppendable reports whether a medium packs many runs per volume (the
// default) rather than one run per volume — so inventory can label a written
// non-appendable reel "used" instead of "append".
func (a *Accountant) MediumAppendable(name string) bool {
	if m, ok := a.d.Cfg.Media[name]; ok {
		return m.IsAppendable()
	}
	return true
}

// Media returns a summary of every configured medium, sorted by name.
func (a *Accountant) Media() []MediumInfo {
	names := make([]string, 0, len(a.d.Cfg.Media))
	for n := range a.d.Cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]MediumInfo, 0, len(names))
	for _, n := range names {
		info, _ := a.Medium(n)
		out = append(out, info)
	}
	return out
}

// Medium returns the summary for one configured medium; ok is false if the name
// is unknown.
func (a *Accountant) Medium(name string) (MediumInfo, bool) {
	d, ok := a.d.Cfg.Media[name]
	if !ok {
		return MediumInfo{}, false
	}
	info := MediumInfo{
		Name: name,
		Type: d.Type,
		Runs: len(a.d.Cat.RunsOn(name)),
		Used: a.d.Cat.MediumBytes(name),
	}
	if prof, err := media.OpenProfile(d.Type, media.Options(d.ProfileOptions())); err == nil {
		info.Capacity = prof.TotalBytes()
	}
	// Summarize the medium's labeled volumes from the catalog (no medium type
	// special-casing): address-identified media (disk, s3) carry no label so the
	// pool is empty and Volumes stays 0; a single labeled volume also carries its
	// name and epoch; a pool of several (a tape library/station) reports only the
	// count, with the per-volume detail in `nb medium <name>`.
	pool := a.volumesInPool(name)
	info.Volumes = len(pool)
	if len(pool) == 1 {
		info.Volume, info.Epoch = pool[0].Label.Name, pool[0].Label.Epoch
	}
	return info, true
}

// UsagePoint is one step in a medium's used-capacity-over-time series: the running
// total of stored bytes on the medium after a run landed its archives there. The
// series is derived from the archives currently on the medium (each carries its
// Compressed size and CreatedAt commit time), oldest run first — the same
// derive-from-the-cache stance run History takes, so there is no separately-persisted
// per-medium fill log to drift out of sync. Because a reclaimed archive has left the
// catalog, the curve tracks the growth of what is *currently retained* rather than a
// live fill gauge: a prune shows up as the absence of old points, never as a drop.
type UsagePoint struct {
	Run   string    // the run whose archives put these bytes on the medium
	At    time.Time // when the run's last archive still on this medium committed
	Added int64     // bytes this run added to the medium (its archives still retained here)
	Used  int64     // cumulative stored bytes on the medium after this run
}

// MediumStats is a medium's usage picture: the current composition derived from the
// catalog (full/incremental byte split, the span the retained archives cover, the
// per-run cumulative breakdown) plus the catalog's recorded usage ledger — the true
// used-over-time curve, which also shows the prune/relabel declines the retained
// picture cannot — and the growth statistics Summarize reads off it. It embeds the
// same MediumInfo `nb medium` lists and is a pure read like Medium(), so `nb web`
// serves it lock-free.
type MediumStats struct {
	MediumInfo
	Archives  int          // archives currently on the medium
	FullBytes int64        // stored bytes from full (level-0) archives
	IncrBytes int64        // stored bytes from incremental (level>=1) archives
	First     time.Time    // earliest archive commit on the medium (zero if none)
	Last      time.Time    // latest archive commit on the medium (zero if none)
	ByRun     []UsagePoint // the cumulative retained-bytes series per run, oldest first

	Usage  []catalog.UsageSample // the recorded used-over-time curve (the catalog's ledger)
	Growth UsageStats            // growth/projection statistics over Usage

	// PerVolume is the pool's per-volume inventory when the medium's pool holds one
	// or more labeled volumes (tape libraries/stations, keyed on MediumInfo.Volumes
	// > 0 — never on medium type, per the media layer's type-neutrality); nil for
	// address-identified media (disk, s3, gdrive), which carry no volume inventory
	// to report. A healthy rotation keeps most volumes permanently near-full by
	// design (retention holds N-1 of N at capacity), so the aggregate Used/Capacity
	// above is a structurally misleading capacity signal for a pool — PerVolume is
	// what a pool page should render instead.
	PerVolume []VolumeUsage
	// PoolVolumes is the pool's configured volume count (media.Profile.Volumes),
	// which may exceed len(PerVolume) when slots are configured but not yet
	// labeled. 0 when the medium has no volume pool (address-identified) or an
	// unbounded one (a hand-loaded drive with no fixed slot count).
	PoolVolumes int64
}

// VolumeUsage is one labeled volume's contribution to its medium's pool: the
// catalog's identity for it (label, epoch, barcode) plus what has actually landed
// there (bytes, runs, archives, last write) against its per-volume capacity.
type VolumeUsage struct {
	Label    string
	Epoch    int
	Barcode  string
	Bytes    int64
	Runs     int
	Archives int
	Last     time.Time // latest archive commit landed on this volume (zero if none)
	Capacity int64     // 0 = unbounded (the pool's per-volume capacity is not derivable)
	HasRoom  bool      // more data could still be written here (see volumeHasRoom)
}

// MediumStats returns a medium's usage picture; ok is false if the name is unknown.
// The per-run series groups the medium's archives by run (one point per run that put
// bytes here), so it mirrors what `nb run`/`nb medium` count as a run's size on the
// medium; the Usage curve comes verbatim from the catalog's ledger.
func (a *Accountant) MediumStats(name string) (MediumStats, bool) {
	info, ok := a.Medium(name)
	if !ok {
		return MediumStats{}, false
	}
	st := MediumStats{MediumInfo: info}

	// Sum each run's bytes on the medium and track that run's latest commit — the
	// archive-granular ArchivesOn already excludes copies pruned off this medium.
	type agg struct {
		at    time.Time
		bytes int64
	}
	byRun := map[string]*agg{}
	order := make([]string, 0)
	for _, ar := range a.d.Cat.ArchivesOn(name) {
		st.Archives++
		if ar.Level == 0 {
			st.FullBytes += ar.Compressed
		} else {
			st.IncrBytes += ar.Compressed
		}
		g := byRun[ar.Run]
		if g == nil {
			g = &agg{}
			byRun[ar.Run] = g
			order = append(order, ar.Run)
		}
		g.bytes += ar.Compressed
		if ar.CreatedAt.After(g.at) {
			g.at = ar.CreatedAt
		}
	}
	// Run ids are datestamps, so plain-text order is chronological order.
	sort.Strings(order)
	var cum int64
	for _, id := range order {
		g := byRun[id]
		cum += g.bytes
		st.ByRun = append(st.ByRun, UsagePoint{Run: id, At: g.at, Added: g.bytes, Used: cum})
	}
	if n := len(st.ByRun); n > 0 {
		st.First, st.Last = st.ByRun[0].At, st.ByRun[n-1].At
	}
	st.Usage = a.d.Cat.MediumUsage(name)
	st.Growth = Summarize(st.Usage, info.Capacity)
	st.PerVolume, st.PoolVolumes = a.perVolume(name, info)
	return st, true
}

// perVolume builds a labeled pool's per-volume inventory: nil (and 0) when the
// medium carries no volume pool at all (address-identified media). Otherwise it
// reports every labeled volume the catalog knows of, even one the medium's
// current archives never touch (a blank or freshly recycled reel still occupies a
// slot and still counts against "how many have room").
func (a *Accountant) perVolume(name string, info MediumInfo) ([]VolumeUsage, int64) {
	pool := a.volumesInPool(name)
	if len(pool) == 0 {
		return nil, 0
	}
	var configured, perVolCap int64
	if d, ok := a.d.Cfg.Media[name]; ok {
		if prof, err := media.OpenProfile(d.Type, media.Options(d.ProfileOptions())); err == nil {
			configured = prof.Volumes()
			if configured > 0 {
				perVolCap = prof.TotalBytes() / configured
			}
		}
	}
	appendable := a.MediumAppendable(name)

	byLabel := make(map[string]*VolumeUsage, len(pool))
	order := make([]string, 0, len(pool))
	for _, v := range pool {
		byLabel[v.Label.Name] = &VolumeUsage{Label: v.Label.Name, Epoch: v.Label.Epoch, Barcode: v.Barcode, Capacity: perVolCap}
		order = append(order, v.Label.Name)
	}

	// runsOnLabel tracks which runs have already been counted against a label, so a
	// run whose several archives land on the same volume counts once.
	runsOnLabel := map[string]map[string]bool{}
	for _, pai := range a.d.Cat.PlacedArchivesOn(name) {
		labels := pai.Placed.Labels()
		if len(labels) == 0 {
			continue // address-identified copy; the pool is non-empty so this should not occur, but stay defensive
		}
		// A spanned archive's bytes are attributed to only the first label it
		// appears on, so a multi-volume archive is never double-counted into the
		// pool's total; every label it touches still counts the archive and its run,
		// since each of those volumes genuinely holds part of it.
		if vu := byLabel[labels[0]]; vu != nil {
			vu.Bytes += pai.Archive.Compressed
		}
		for _, lbl := range labels {
			vu := byLabel[lbl]
			if vu == nil {
				continue
			}
			vu.Archives++
			if runsOnLabel[lbl] == nil {
				runsOnLabel[lbl] = map[string]bool{}
			}
			if !runsOnLabel[lbl][pai.Run] {
				runsOnLabel[lbl][pai.Run] = true
				vu.Runs++
			}
			if pai.Archive.CreatedAt.After(vu.Last) {
				vu.Last = pai.Archive.CreatedAt
			}
		}
	}

	out := make([]VolumeUsage, len(order))
	for i, lbl := range order {
		vu := *byLabel[lbl]
		vu.HasRoom = volumeHasRoom(vu, appendable)
		out[i] = vu
	}
	// An unbounded pool (media.Profile.Volumes == 0, a hand-loaded drive with no
	// fixed slot count) has no configured total to compare the labeled count
	// against, so report the labeled count itself — a zero headroom gap, which
	// reads as "nothing more expected" rather than falsely as "unbounded slots
	// still unlabeled."
	if configured == 0 {
		configured = int64(len(pool))
	}
	return out, configured
}

// volumeHasRoom reports whether a labeled volume could still take more data. An
// appendable volume (the default) has room while its stored bytes are under its
// per-volume capacity — or always, if that capacity is not derivable. A
// non-appendable medium writes at most one run per volume, so any run at all
// exhausts it regardless of how few bytes that run used.
func volumeHasRoom(v VolumeUsage, appendable bool) bool {
	if !appendable {
		return v.Runs == 0
	}
	return v.Capacity == 0 || v.Bytes < v.Capacity
}

// UsageStats summarizes a medium's recorded usage curve: how long it has been
// recorded, its average growth over that span, and — for a bounded medium still
// filling — a naive linear projection of when it reaches capacity. A glanceable hint
// (the dollar/byte forecasts in cost.go stay the planner's real machinery), computed
// from the ledger so it reflects prunes, not just currently-retained bytes.
type UsageStats struct {
	Samples  int
	First    time.Time // first / last recorded sample
	Last     time.Time
	PerDay   int64     // average growth over [First,Last], bytes/day; 0 when under a day or shrinking
	ProjFull time.Time // projected capacity-reached date; zero when unbounded, at/over capacity, or not growing
}

// Summarize computes the growth statistics for a medium's usage curve against its
// current capacity (capacity is config knowledge, so the caller passes it fresh
// rather than reading it out of old samples). Fewer than two samples, a sub-day
// span, or a net decline all yield no rate — the projection must never mislead.
func Summarize(series []catalog.UsageSample, capacity int64) UsageStats {
	st := UsageStats{Samples: len(series)}
	if len(series) == 0 {
		return st
	}
	first, last := series[0], series[len(series)-1]
	st.First, st.Last = first.At, last.At
	if len(series) < 2 {
		return st
	}
	days := last.At.Sub(first.At).Hours() / 24
	if days < 1 {
		return st // too short a baseline to read a daily rate from
	}
	grew := last.Used - first.Used
	if grew <= 0 {
		return st // flat or shrinking: no meaningful fill projection
	}
	st.PerDay = int64(float64(grew) / days)
	if st.PerDay > 0 && capacity > 0 && last.Used < capacity {
		daysToFull := float64(capacity-last.Used) / float64(st.PerDay)
		st.ProjFull = last.At.Add(time.Duration(daysToFull * float64(24*time.Hour)))
	}
	return st
}

// volumesInPool returns the labeled volumes the catalog tracks for a medium
// (matched by the label pool == medium name), sorted by name. (catalog.Volumes is
// already name-sorted.)
func (a *Accountant) volumesInPool(medium string) []catalog.VolumeRecord {
	var out []catalog.VolumeRecord
	for _, v := range a.d.Cat.Volumes() {
		if v.Label.Pool == medium {
			out = append(out, v)
		}
	}
	return out
}
