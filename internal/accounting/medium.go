package accounting

import (
	"sort"

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
