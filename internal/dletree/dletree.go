// Package dletree groups DLE identities by their path relationships, for display.
// With partitioned sources one source resolves into many DLEs whose names embed
// full absolute paths (host:/data/projects/customer-alpha, …, plus "the rest" at
// host:/data); rendered flat, those names widen every table and repeat the same
// prefix per row. Build recovers the structure from the names alone — no config or
// catalog knowledge — so every surface (report, nb dle, web) groups the same way,
// works on pre-feature history and rebuilt catalogs, and also groups hand-written
// sibling DLEs that merely share a directory. It is purely presentational: slugs,
// display ids, and state chains are never derived from it.
package dletree

import (
	"fmt"
	"sort"
	"strings"
)

// Item is one DLE's identity as the grouper needs it. Rest marks a partition's
// remainder (catalog.ResolvedDLE.Rest) when the caller knows it; it only affects
// how the covering row is labeled ("the rest" vs "all of <base>"), never the
// grouping itself. An Item with no host (a bare-slug fallback) is never grouped.
type Item struct {
	Host string
	Path string
	Rest bool
}

// Split parses a "host:path" display identity into an Item. A display with no ':'
// (the bare-slug fallback) has no host; it comes back with the whole string as
// Path and ok false, and Build keeps such items flat.
func Split(display string) (it Item, ok bool) {
	i := strings.IndexByte(display, ':')
	if i < 0 {
		return Item{Path: display}, false
	}
	return Item{Host: display[:i], Path: display[i+1:]}, true
}

// Child is one row inside a group, pointing back at the caller's slice by Index.
// Label is the path relative to the group base; it is "" for the covering DLE
// itself, which renderers show as "(the rest)" (Rest) or "all of <base>" (a plain
// DLE that overlaps its listed children).
type Child struct {
	Index int
	Label string
	Rest  bool
}

// Group is one entry of the arranged list: either a flat single DLE (Children nil,
// Index pointing at it) or a group of ≥2 DLEs under Base. Rooted marks a group
// whose Base is itself a member DLE (rendered as the trailing "" child); an
// unrooted group's header is synthesized from the shared parent directory.
type Group struct {
	Host     string
	Base     string
	Index    int // the flat item's index; -1 when Children is set
	Rooted   bool
	Children []Child
}

// ID is the group's display identity for headers: "host:base" for a group,
// the item's own "host:path" for a flat entry with a host.
func (g Group) ID() string { return g.Host + ":" + g.Base }

// Label names a child for rendering: its base-relative path, "(the rest)" for a
// partition's remainder, or "all of <base>" for a plain DLE that covers its
// listed siblings (an overlapping declaration, not a carved-out remainder).
func (g Group) Label(c Child) string {
	switch {
	case c.Label != "":
		return c.Label
	case c.Rest:
		return "(the rest)"
	default:
		return "all of " + g.Base
	}
}

// Branch is the tree glyph before the i-th of n group members — the same tree
// `nb plan` draws.
func Branch(i, n int) string {
	if i == n-1 {
		return "└─"
	}
	return "├─"
}

// Build arranges items into display groups, sorted by (host, path) with hostless
// items trailing. Within a host, consecutive DLEs sharing any non-root directory
// form one run ("/data" relates to "/data/x" but not "/database" or "/home"),
// and arrange decides how the run renders: one group, several per-subdirectory
// groups, or flat rows. Items with no related path stay flat, so a catalog
// without sibling DLEs renders exactly as an ungrouped list.
func Build(items []Item) []Group {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := items[idx[a]], items[idx[b]]
		if (ia.Host == "") != (ib.Host == "") {
			return ib.Host == "" // hostless last
		}
		if ia.Host != ib.Host {
			return ia.Host < ib.Host
		}
		return cleanPath(ia.Path) < cleanPath(ib.Path)
	})

	var out []Group
	for i := 0; i < len(idx); {
		it := items[idx[i]]
		if it.Host == "" {
			out = append(out, Group{Base: it.Path, Index: idx[i]})
			i++
			continue
		}
		p := cleanPath(it.Path)
		j := i + 1
		for ; j < len(idx); j++ {
			m := items[idx[j]]
			if m.Host != it.Host || commonDir(p, cleanPath(m.Path)) == "/" {
				break
			}
		}
		out = append(out, arrange(items, idx[i:j])...)
		i = j
	}
	return out
}

// noiseBudget is how much repeated path text one merged group may carry: the
// sum over members of label segments beyond the first. Under it, a merged
// group is compact (a /data group with projects/alpha, projects/beta, qa/x
// reads fine); past it, the repetition drowns the rows and per-subdirectory
// groups with single-segment labels read better, even at the cost of one
// header line per subdirectory.
const noiseBudget = 8

// arrange turns one run of members — sorted indexes into items, all sharing a
// non-root directory — into display groups. A run whose first member covers the
// rest stays one rooted group (partition semantics: the covering row belongs
// with its carve-outs). Otherwise members bucket by their first path segment
// under the run's shared directory, and readability cost decides: the merged
// group's noise is the path text its labels repeat (segments beyond the first,
// summed), and splitting trades that noise for a header per bucket. A small
// cluster ("/tank/customers/{acme,globex}" next to "/tank/internal/{wiki,crm}")
// stays one /tank group — four short two-segment labels beat two long headers —
// but "/mnt/photo/*" next to "/mnt/docs/*" at any real size splits, since a
// "/mnt" header lumping unrelated trees repeats its subdirectory names down
// the whole column. Populated buckets (≥2 DLEs) recurse into their own groups, singleton
// buckets fall back to flat rows, and a bucket carrying its own covering DLE
// always splits out — coverage is explicit structure, never merged away.
func arrange(items []Item, members []int) []Group {
	if len(members) == 1 {
		it := items[members[0]]
		return []Group{{Host: it.Host, Base: cleanPath(it.Path), Index: members[0]}}
	}
	d := cleanPath(items[members[0]].Path)
	for _, m := range members[1:] {
		d = commonDir(d, cleanPath(items[m].Path))
	}
	if cleanPath(items[members[0]].Path) == d {
		return []Group{group(items, members, d)}
	}
	var segs []string // first-appearance order ≈ sorted by each bucket's first member
	buckets := map[string][]int{}
	for _, m := range members {
		s := segment(cleanPath(items[m].Path), d)
		if _, ok := buckets[s]; !ok {
			segs = append(segs, s)
		}
		buckets[s] = append(buckets[s], m)
	}
	populated, noise, split := 0, 0, false
	for _, s := range segs {
		b := buckets[s]
		if len(b) >= 2 {
			populated++
			if cleanPath(items[b[0]].Path) == d+"/"+s {
				split = true // a covering DLE deserves its own rooted group
			}
		}
		for _, m := range b {
			noise += strings.Count(relLabel(d, cleanPath(items[m].Path)), "/")
		}
	}
	// Splitting needs a populated bucket to split out; without one it would
	// only scatter singletons into full-path flat rows.
	if !split && (populated == 0 || noise <= noiseBudget) {
		return []Group{group(items, members, d)}
	}
	var out []Group
	for _, s := range segs {
		out = append(out, arrange(items, buckets[s])...)
	}
	return out
}

// group builds one display group at base d: children keep sorted order with
// d-relative labels, and a member at d itself roots the group and renders
// last, like nb plan's "the rest".
func group(items []Item, members []int, d string) Group {
	g := Group{Host: items[members[0]].Host, Base: d, Index: -1}
	var covering []Child
	for _, m := range members {
		it := items[m]
		p := cleanPath(it.Path)
		c := Child{Index: m, Label: relLabel(d, p), Rest: it.Rest}
		if p == d {
			g.Rooted = true
			covering = append(covering, c)
			continue
		}
		g.Children = append(g.Children, c)
	}
	g.Children = append(g.Children, covering...)
	return g
}

// segment is p's first path element below directory d ("photo" for
// p=/mnt/photo/2024, d=/mnt). p is strictly under d.
func segment(p, d string) string {
	rest := p[len(d)+1:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// commonDir is the deepest directory containing both paths ("/" when they share
// nothing below the root): the byte-wise common prefix cut back to a path
// boundary, with an ancestor path counting as the directory itself.
func commonDir(a, b string) string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i == len(a) && (i == len(b) || b[i] == '/') {
		return a
	}
	if i == len(b) && a[i] == '/' {
		return b
	}
	if j := strings.LastIndexByte(a[:i], '/'); j > 0 {
		return a[:j]
	}
	return "/"
}

// relLabel is a child's label relative to the group base ("" for the base itself).
func relLabel(base, p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
}

// cleanPath normalizes a path for comparison: trailing slashes dropped ("/data/"
// and "/data" are the same DLE), the bare root kept as "/".
func cleanPath(p string) string {
	if c := strings.TrimRight(p, "/"); c != "" {
		return c
	}
	return "/"
}

// listCap bounds any one-line DLE list: past it, the tail folds into "…and N
// more" (the full picture lives on the list's own surface — nb dle, nb drill,
// the web).
const listCap = 6

// FoldList compacts a list of DLE identities for a one-line note: path siblings
// fold to their group's "host:base (N DLEs)", the result comes back in Build
// order, and anything past listCap folds into a trailing count (CapList).
func FoldList(names []string) string {
	items := make([]Item, len(names))
	for i, n := range names {
		items[i], _ = Split(n)
	}
	var parts []string
	for _, g := range Build(items) {
		if g.Children == nil {
			parts = append(parts, names[g.Index])
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%d DLEs)", g.ID(), len(g.Children)))
	}
	return CapList(parts)
}

// CapList joins entries with ", ", folding everything past listCap into a count.
func CapList(parts []string) string {
	if len(parts) > listCap {
		n := len(parts) - (listCap - 1)
		parts = append(parts[:listCap-1:listCap-1], fmt.Sprintf("…and %d more", n))
	}
	return strings.Join(parts, ", ")
}
