package recovery

import (
	"testing"

	"github.com/Niloen/nbackup/internal/format"
)

// scenario builds two slots: a full on day 1 and an incremental on day 2 that
// rewrites etc/hosts and adds etc/new.conf.
func scenario() []*format.Slot {
	full := &format.Slot{ID: "slot-2026-06-21", Date: "2026-06-21", Archives: []format.Archive{{
		DLE: "app", Level: 0, Archiver: "gnutar", Codec: "none",
		Members: []string{
			"./", "./etc/", "./etc/hosts", "./etc/passwd",
			"./var/", "./var/log/", "./var/log/a.log",
		},
	}}}
	incr := &format.Slot{ID: "slot-2026-06-22", Date: "2026-06-22", Archives: []format.Archive{{
		DLE: "app", Level: 1, Archiver: "gnutar", Codec: "none",
		Members: []string{"./", "./etc/", "./etc/hosts", "./etc/new.conf"},
	}}}
	return []*format.Slot{full, incr}
}

func TestAsOf(t *testing.T) {
	slots := scenario()
	for _, tc := range []struct {
		date, want string
		wantErr    bool
	}{
		{"2026-06-22", "slot-2026-06-22", false},
		{"2026-06-21", "slot-2026-06-21", false},
		{"2026-06-25", "slot-2026-06-22", false}, // latest on/before
		{"2026-06-20", "", true},                 // before all slots
	} {
		got, err := AsOf(slots, tc.date)
		if tc.wantErr {
			if err == nil {
				t.Errorf("AsOf(%s): want error, got %q", tc.date, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("AsOf(%s) = %q, %v; want %q", tc.date, got, err, tc.want)
		}
	}
}

func TestBuildTreeMostRecentWins(t *testing.T) {
	tree, err := BuildTree(scenario(), "app", "2026-06-22")
	if err != nil {
		t.Fatal(err)
	}
	if tree.TargetSlot != "slot-2026-06-22" {
		t.Fatalf("target = %s", tree.TargetSlot)
	}

	// hosts was rewritten on day 2 → sourced from the incremental.
	hosts, ok := tree.Lookup("etc/hosts")
	if !ok || hosts.src == nil || hosts.src.SlotID != "slot-2026-06-22" {
		t.Fatalf("etc/hosts source = %+v", hosts)
	}
	// passwd was untouched → still sourced from the full.
	passwd, ok := tree.Lookup("etc/passwd")
	if !ok || passwd.src == nil || passwd.src.SlotID != "slot-2026-06-21" {
		t.Fatalf("etc/passwd source = %+v", passwd)
	}
	// new.conf appeared on day 2.
	if _, ok := tree.Lookup("etc/new.conf"); !ok {
		t.Fatal("etc/new.conf missing from tree")
	}
	// deep file from the full survives.
	if _, ok := tree.Lookup("var/log/a.log"); !ok {
		t.Fatal("var/log/a.log missing")
	}
}

func TestBuildTreeAsOfEarlierDate(t *testing.T) {
	tree, err := BuildTree(scenario(), "app", "2026-06-21")
	if err != nil {
		t.Fatal(err)
	}
	// As of day 1, hosts comes from the full and new.conf does not exist yet.
	hosts, ok := tree.Lookup("etc/hosts")
	if !ok || hosts.src.SlotID != "slot-2026-06-21" {
		t.Fatalf("etc/hosts should be from the full, got %+v", hosts.src)
	}
	if _, ok := tree.Lookup("etc/new.conf"); ok {
		t.Fatal("etc/new.conf should not exist as of day 1")
	}
}

func TestBuildTreeNoFull(t *testing.T) {
	if _, err := BuildTree(scenario(), "other", "2026-06-22"); err == nil {
		t.Fatal("expected error for a DLE with no backup")
	}
}

func TestChildrenSortedDirsFirst(t *testing.T) {
	tree, _ := BuildTree(scenario(), "app", "2026-06-22")
	root, _ := tree.Lookup("/")
	kids := root.Children()
	if len(kids) != 2 || !kids[0].IsDir() || kids[0].Name() != "etc" || kids[1].Name() != "var" {
		var got []string
		for _, k := range kids {
			got = append(got, k.Name())
		}
		t.Fatalf("root children = %v", got)
	}
}

func TestCollectDirectoryGroupsByArchive(t *testing.T) {
	tree, _ := BuildTree(scenario(), "app", "2026-06-22")
	steps, err := tree.Collect([]string{"etc"})
	if err != nil {
		t.Fatal(err)
	}
	// etc/hosts + etc/new.conf live on the incremental; etc/passwd on the full;
	// the etc/ directory member itself comes from the incremental (most recent).
	bySlot := map[string][]string{}
	for _, st := range steps {
		bySlot[st.SlotID] = st.Members
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 archive steps, got %d: %+v", len(steps), steps)
	}
	if !contains(bySlot["slot-2026-06-22"], "./etc/hosts") || !contains(bySlot["slot-2026-06-22"], "./etc/new.conf") {
		t.Errorf("incremental members = %v", bySlot["slot-2026-06-22"])
	}
	if !contains(bySlot["slot-2026-06-21"], "./etc/passwd") {
		t.Errorf("full members = %v", bySlot["slot-2026-06-21"])
	}
}

func TestCollectSingleFilePullsAncestors(t *testing.T) {
	tree, _ := BuildTree(scenario(), "app", "2026-06-22")
	steps, err := tree.Collect([]string{"/var/log/a.log"})
	if err != nil {
		t.Fatal(err)
	}
	// a.log plus its ancestor directory members (var/, var/log/) all live on the
	// full, so a single step pulls them together.
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	m := steps[0].Members
	for _, want := range []string{"./var/", "./var/log/", "./var/log/a.log"} {
		if !contains(m, want) {
			t.Errorf("members %v missing %s", m, want)
		}
	}
}

func TestCollectNotFound(t *testing.T) {
	tree, _ := BuildTree(scenario(), "app", "2026-06-22")
	if _, err := tree.Collect([]string{"nope"}); err == nil {
		t.Fatal("expected not-found error")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
