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

// TestSynthesizedMergesUp: while only ONE subdirectory has ≥2 DLEs, related
// paths form a single group at the shallowest shared directory, with
// two-segment labels — not one tiny per-directory group plus stray flat rows.
// (The selection pattern "*/*" with no rest is exactly this shape.)
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

// TestSplitsPopulatedSubdirs: past the noise budget, a shared-ancestor header
// would lump unrelated trees under repetitive multi-segment labels — each
// populated subdirectory gets its own group and singletons stay flat.
func TestSplitsPopulatedSubdirs(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/mnt/photo/2019"},
		{Host: "h", Path: "/mnt/photo/2020"},
		{Host: "h", Path: "/mnt/photo/2021"},
		{Host: "h", Path: "/mnt/photo/2022"},
		{Host: "h", Path: "/mnt/photo/2023"},
		{Host: "h", Path: "/mnt/photo/2024"},
		{Host: "h", Path: "/mnt/photo/raw"},
		{Host: "h", Path: "/mnt/docs/work"},
		{Host: "h", Path: "/mnt/docs/private"},
		{Host: "h", Path: "/mnt/loose"},
	}, []string{
		"h:/mnt/docs",
		"  private",
		"  work",
		"h:/mnt/loose",
		"h:/mnt/photo",
		"  2019",
		"  2020",
		"  2021",
		"  2022",
		"  2023",
		"  2024",
		"  raw",
	})
}

// TestSmallClustersStayMerged: two 4-DLE subdirectories sit exactly at the
// noise budget — one group with two-segment labels beats two small groups
// whose headers repeat the shared prefix. (One more member would tip it.)
func TestSmallClustersStayMerged(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/tank/customers/acme"},
		{Host: "h", Path: "/tank/customers/globex"},
		{Host: "h", Path: "/tank/customers/initech"},
		{Host: "h", Path: "/tank/customers/umbrella"},
		{Host: "h", Path: "/tank/internal/wiki"},
		{Host: "h", Path: "/tank/internal/crm"},
		{Host: "h", Path: "/tank/internal/mail"},
		{Host: "h", Path: "/tank/internal/www"},
	}, []string{
		"h:/tank",
		"  customers/acme",
		"  customers/globex",
		"  customers/initech",
		"  customers/umbrella",
		"  internal/crm",
		"  internal/mail",
		"  internal/wiki",
		"  internal/www",
	})
}

// TestDeepClustersSplit: depth counts against the budget like breadth — a 3+3
// shape splits once one cluster sits two segments below the shared directory
// (the same six members all one level down would merge), and each cluster's
// group forms at its own deepest common dir.
func TestDeepClustersSplit(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/mnt/photo/2024/summer"},
		{Host: "h", Path: "/mnt/photo/2024/winter"},
		{Host: "h", Path: "/mnt/photo/2024/fall"},
		{Host: "h", Path: "/mnt/docs/work"},
		{Host: "h", Path: "/mnt/docs/private"},
		{Host: "h", Path: "/mnt/docs/archive"},
	}, []string{
		"h:/mnt/docs",
		"  archive",
		"  private",
		"  work",
		"h:/mnt/photo/2024",
		"  fall",
		"  summer",
		"  winter",
	})
}

// TestBroadSiblingsAbsorbSmallCluster: a wide spread of direct children plus
// one small subdirectory stays merged whatever its size — splitting could only
// scatter the direct children into full-path flat rows.
func TestBroadSiblingsAbsorbSmallCluster(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/big/a"},
		{Host: "h", Path: "/big/b"},
		{Host: "h", Path: "/big/c"},
		{Host: "h", Path: "/big/d"},
		{Host: "h", Path: "/big/e"},
		{Host: "h", Path: "/big/f"},
		{Host: "h", Path: "/big/sub/x"},
		{Host: "h", Path: "/big/sub/y"},
	}, []string{
		"h:/big",
		"  a",
		"  b",
		"  c",
		"  d",
		"  e",
		"  f",
		"  sub/x",
		"  sub/y",
	})
}

// TestRootedNeighborSplitsOut: a partition's rooted group keeps its own header
// even when an unrelated sibling tree would otherwise pull the run up to a
// shared ancestor — coverage is explicit structure, never merged away.
func TestRootedNeighborSplitsOut(t *testing.T) {
	check(t, []Item{
		{Host: "h", Path: "/srv/data", Rest: true},
		{Host: "h", Path: "/srv/data/x"},
		{Host: "h", Path: "/srv/other"},
	}, []string{
		"h:/srv/data [rooted]",
		"  x",
		"  (the rest)",
		"h:/srv/other",
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
