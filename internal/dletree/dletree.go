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
// items trailing. Within a host, a DLE whose path is a path-boundary prefix of
// the ones after it roots a group ("/data" covers "/data/x" but not "/database");
// ≥2 DLEs with no covering DLE get a synthesized header at the shallowest
// non-root directory they all share, so members carry multi-segment labels in
// one group rather than splitting into several tiny per-directory groups.
// Everything else stays flat, so a catalog without related paths renders
// exactly as an ungrouped list.
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
		base, rooted := groupBase(items, idx, i)
		if base == "" {
			out = append(out, Group{Host: it.Host, Base: cleanPath(it.Path), Index: idx[i]})
			i++
			continue
		}
		g := Group{Host: it.Host, Base: base, Index: -1, Rooted: rooted}
		var rest *Child
		for ; i < len(idx); i++ {
			m := items[idx[i]]
			if m.Host != it.Host {
				break
			}
			p := cleanPath(m.Path)
			if p != base && !covers(base, p) {
				break
			}
			c := Child{Index: idx[i], Label: relLabel(base, p), Rest: m.Rest}
			if p == base {
				rest = &c // the covering DLE renders last, like nb plan's "the rest"
				continue
			}
			g.Children = append(g.Children, c)
		}
		if rest != nil {
			g.Children = append(g.Children, *rest)
		}
		out = append(out, g)
	}
	return out
}

// groupBase decides whether the sorted run starting at position pos opens a group,
// returning its base path ("" for a flat item). A DLE covering its successor roots
// a group at its own path. Otherwise the base is synthesized as the SHALLOWEST
// directory (never the root, which would swallow unrelated DLEs) shared by the
// whole run of successors — one /data group whose members carry two-segment
// labels (projects/alpha, qa/x) reads better than several two-row /data/<dir>
// groups, and matches what a rooted group already shows for the same tree.
func groupBase(items []Item, idx []int, pos int) (base string, rooted bool) {
	if pos+1 >= len(idx) {
		return "", false
	}
	cur := items[idx[pos]]
	p := cleanPath(cur.Path)
	if next := items[idx[pos+1]]; next.Host != cur.Host {
		return "", false
	} else if covers(p, cleanPath(next.Path)) {
		return p, true
	}
	for j := pos + 1; j < len(idx); j++ {
		m := items[idx[j]]
		if m.Host != cur.Host {
			break
		}
		d := commonDir(p, cleanPath(m.Path))
		// d == p would make this item the group's covering DLE mid-run (a sort
		// quirk: "-" < "/" can order "/data-x" between "/data" and "/data/x");
		// stop rather than fabricate a rest row the rooted path didn't claim.
		if d == "/" || d == p {
			break
		}
		base = d
	}
	return base, false
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

// covers reports whether base contains p at a path boundary: "/data" covers
// "/data/x" but not "/database", and the root never covers (it would group
// every absolute path into one).
func covers(base, p string) bool {
	if base == "/" || len(p) <= len(base) {
		return false
	}
	return strings.HasPrefix(p, base) && p[len(base)] == '/'
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
