package dletree

import (
	"reflect"
	"testing"
)

// render flattens Build's output into readable "header" / "  label" lines keyed by
// the original items, so each case states the expected arrangement at a glance.
func render(items []Item) []string {
	var out []string
	for _, g := range Build(items) {
		if g.Children == nil {
			it := items[g.Index]
			if it.Host == "" {
				out = append(out, it.Path)
			} else {
				out = append(out, it.Host+":"+it.Path)
			}
			continue
		}
		hdr := g.ID()
		if g.Rooted {
			hdr += " [rooted]"
		}
		out = append(out, hdr)
		for _, c := range g.Children {
			label := c.Label
			if label == "" {
				if c.Rest {
					label = "(the rest)"
				} else {
					label = "(all of base)"
				}
			}
			out = append(out, "  "+label)
		}
	}
	return out
}

func check(t *testing.T, items []Item, want []string) {
	t.Helper()
	if got := render(items); !reflect.DeepEqual(got, want) {
		t.Errorf("arranged\n  %q\nwant\n  %q", got, want)
	}
}

// TestPartitionedSource is the feature's main case: matches plus the rest DLE at
// the base, arriving in any order, become one rooted group with the rest last.
func TestPartitionedSource(t *testing.T) {
	check(t, []Item{
		{Host: "app01", Path: "/data/projects/customer-beta"},
		{Host: "app01", Path: "/data", Rest: true},
		{Host: "app01", Path: "/data/archived-financials"},
		{Host: "app01", Path: "/home"},
	}, []string{
		"app01:/data [rooted]",
		"  archived-financials",
		"  projects/customer-beta",
		"  (the rest)",
		"app01:/home",
	})
}

// TestOverlappingPlainDLE: a hand-written covering DLE (not a partition rest)
// still roots the group, but its row is labeled as the whole tree, not the rest.
func TestOverlappingPlainDLE(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/data"},
		{Host: "h", Path: "/data/x"},
	}, []string{
		"h:/data [rooted]",
		"  x",
		"  (all of base)",
	})
}

// TestSynthesizedSiblings: ≥2 DLEs sharing a parent directory group under a
// synthesized header even though no DLE covers them.
func TestSynthesizedSiblings(t *testing.T) {
	check(t, []Item{
		{Host: "web01", Path: "/srv/web-alpha"},
		{Host: "web01", Path: "/srv/web-beta"},
		{Host: "web01", Path: "/var/log"},
	}, []string{
		"web01:/srv",
		"  web-alpha",
		"  web-beta",
		"web01:/var/log",
	})
}

// TestSynthesizedMergesUp: siblings under different subdirectories of one
// ancestor form a single group at the shallowest shared directory, with
// two-segment labels — not several tiny per-directory groups. (The selection
// pattern "*/*" with no rest is exactly this shape.)
func TestSynthesizedMergesUp(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/data/projects/alpha"},
		{Host: "h", Path: "/data/projects/beta"},
		{Host: "h", Path: "/data/qa/x"},
		{Host: "h", Path: "/data/loose"},
		{Host: "h", Path: "/home"},
	}, []string{
		"h:/data",
		"  loose",
		"  projects/alpha",
		"  projects/beta",
		"  qa/x",
		"h:/home",
	})
}

// TestNoRootGroup: top-level DLEs share only "/", which must never group them.
func TestNoRootGroup(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/home"},
		{Host: "h", Path: "/var"},
	}, []string{"h:/home", "h:/var"})
}

// TestPathBoundary: "/data" must not capture "/database".
func TestPathBoundary(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/data"},
		{Host: "h", Path: "/database"},
	}, []string{"h:/data", "h:/database"})
}

// TestHostsSeparate: identical paths on different hosts never mix, and hosts
// render in name order with hostless items trailing flat.
func TestHostsSeparate(t *testing.T) {
	check(t, []Item{
		{Path: "bare-slug"},
		{Host: "b", Path: "/data/x"},
		{Host: "b", Path: "/data", Rest: true},
		{Host: "a", Path: "/data"},
	}, []string{
		"a:/data",
		"b:/data [rooted]",
		"  x",
		"  (the rest)",
		"bare-slug",
	})
}

// TestDeepChildren: children below the immediate level still join a rooted group
// with multi-segment relative labels.
func TestDeepChildren(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/data", Rest: true},
		{Host: "h", Path: "/data/projects/alpha"},
		{Host: "h", Path: "/data/projects/beta"},
	}, []string{
		"h:/data [rooted]",
		"  projects/alpha",
		"  projects/beta",
		"  (the rest)",
	})
}

// TestTrailingSlash: "/data/" and "/data" are the same path for grouping.
func TestTrailingSlash(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/data/", Rest: true},
		{Host: "h", Path: "/data/x"},
	}, []string{
		"h:/data [rooted]",
		"  x",
		"  (the rest)",
	})
}

func TestSplit(t *testing.T) {
	if it, ok := Split("app01:/data/x"); !ok || it.Host != "app01" || it.Path != "/data/x" {
		t.Errorf("Split display = %+v, %v", it, ok)
	}
	if it, ok := Split("bare-slug"); ok || it.Path != "bare-slug" {
		t.Errorf("Split slug = %+v, %v", it, ok)
	}
}
