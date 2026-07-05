package mount

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// backend builds a fake Backend over two runs: a full of DLE "app" on day 1
// (etc/hosts, etc/passwd) and an incremental on day 2 (rewrites etc/hosts,
// adds etc/new.conf); DLE "web" is dumped only on day 1. Extract writes each
// requested member as "content of <member>@<run>" into the destination.
func backend() Backend {
	archives := []record.Archive{
		{Run: "run-2026-06-21.001", DLE: "app", Level: 0, Archiver: "gnutar", Compress: "none",
			Members: mems("./", "./etc/", "./etc/hosts", "./etc/passwd")},
		{Run: "run-2026-06-21.001", DLE: "web", Level: 0, Archiver: "gnutar", Compress: "none",
			Members: mems("./", "./index.html")},
		{Run: "run-2026-06-22.001", DLE: "app", Level: 1, Archiver: "gnutar", Compress: "none",
			Members: mems("./", "./etc/", "./etc/hosts", "./etc/new.conf")},
	}
	// web's index carries a browse alias — grafted as a symlink in the tree.
	archives[1].Members[1].Alias = "pages/home.html"
	runs := []*catalog.Run{
		{ID: "run-2026-06-21.001", Archives: archives[:2]},
		{ID: "run-2026-06-22.001", Archives: archives[2:]},
	}
	membersOf := func(dle string) func(string, int) ([]record.Member, error) {
		return func(runID string, level int) ([]record.Member, error) {
			for i := range archives {
				if archives[i].Run == runID && archives[i].DLE == dle && archives[i].Level == level {
					return archives[i].Members, nil
				}
			}
			return nil, nil
		}
	}
	return Backend{
		Runs: func() []*catalog.Run { return runs },
		Tree: func(dle, runID string) (*recovery.Tree, error) {
			return recovery.BuildTreeForRun(archives, dle, runID, membersOf(dle), nil)
		},
		Extract: func(steps []recovery.ExtractStep, _ []recovery.Assembly, destDir string) error {
			for _, st := range steps {
				for _, m := range st.Members {
					p := strings.Trim(strings.TrimPrefix(m, "./"), "/")
					if p == "" {
						continue
					}
					dst := filepath.Join(destDir, filepath.FromSlash(p))
					if strings.HasSuffix(m, "/") {
						if err := os.MkdirAll(dst, 0o755); err != nil {
							return err
						}
						continue
					}
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(dst, []byte("content of "+p+"@"+st.RunID), 0o644); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

func mems(paths ...string) []record.Member {
	out := make([]record.Member, len(paths))
	for i, p := range paths {
		out[i] = record.Member{Path: p, Off: int64(i) * 512}
	}
	return out
}

func TestDLEsAtOrBeforeRun(t *testing.T) {
	m := &mountFS{b: backend()}
	// Run 2 dumped only "app", but the snapshot lists every DLE with a dump at
	// or before it.
	if got := m.dlesAt("run-2026-06-22.001"); !reflect.DeepEqual(got, []string{"app", "web"}) {
		t.Fatalf("dlesAt(run 2) = %v", got)
	}
	if got := m.dlesAt("run-2026-06-21.001"); !reflect.DeepEqual(got, []string{"app", "web"}) {
		t.Fatalf("dlesAt(run 1) = %v", got)
	}
}

// TestServe mounts the filesystem for real and walks it: run dirs at the top,
// DLE snapshots inside, content recovered on first read. It skips where FUSE
// is unavailable (no /dev/fuse or no fusermount helper).
func TestServe(t *testing.T) {
	mp := t.TempDir()
	srv, err := Serve(Options{Mountpoint: mp, CacheDir: t.TempDir(), Logf: t.Logf}, backend())
	if err != nil {
		t.Skipf("FUSE unavailable: %v", err)
	}
	defer func() {
		if err := srv.Unmount(); err != nil {
			t.Errorf("unmount: %v", err)
		}
		srv.Wait()
	}()

	names := func(dir string) []string {
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir %s: %v", dir, err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}

	if got := names(mp); !reflect.DeepEqual(got, []string{"run-2026-06-21.001", "run-2026-06-22.001"}) {
		t.Fatalf("root = %v", got)
	}
	run2 := filepath.Join(mp, "run-2026-06-22.001")
	if got := names(run2); !reflect.DeepEqual(got, []string{"app", "web"}) {
		t.Fatalf("run 2 = %v", got)
	}
	if got := names(filepath.Join(run2, "app", "etc")); !reflect.DeepEqual(got, []string{"hosts", "new.conf", "passwd"}) {
		t.Fatalf("run 2 app/etc = %v", got)
	}

	// Unopened file lists with size 0; content appears in full on read.
	hosts := filepath.Join(run2, "app", "etc", "hosts")
	if fi, err := os.Stat(hosts); err != nil || fi.Size() != 0 {
		t.Fatalf("pre-open stat = %v, %v; want size 0", fi, err)
	}
	got, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatal(err)
	}
	// hosts was rewritten by the incremental → sourced from run 2.
	if want := "content of etc/hosts@run-2026-06-22.001"; string(got) != want {
		t.Fatalf("hosts = %q, want %q", got, want)
	}
	// passwd was untouched → still sourced from the full.
	got, err = os.ReadFile(filepath.Join(run2, "app", "etc", "passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "content of etc/passwd@run-2026-06-21.001"; string(got) != want {
		t.Fatalf("passwd = %q, want %q", got, want)
	}
	// After the read the cached size is real.
	if fi, err := os.Stat(hosts); err != nil || fi.Size() == 0 {
		t.Fatalf("post-read stat = %v, %v; want real size", fi, err)
	}

	// web wasn't dumped in run 2 — its snapshot resolves to the run-1 dump.
	got, err = os.ReadFile(filepath.Join(run2, "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "content of index.html@run-2026-06-21.001"; string(got) != want {
		t.Fatalf("web index = %q, want %q", got, want)
	}

	// The alias appears as a real symlink and reads THROUGH it resolve to the
	// physical file's content.
	link := filepath.Join(run2, "web", "pages", "home.html")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alias lstat = %v, %v; want symlink", fi, err)
	}
	if target, err := os.Readlink(link); err != nil || target != "../index.html" {
		t.Fatalf("alias readlink = %q, %v", target, err)
	}
	got, err = os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if want := "content of index.html@run-2026-06-21.001"; string(got) != want {
		t.Fatalf("alias read = %q, want %q", got, want)
	}

	// Read-only: writes are refused.
	if err := os.WriteFile(hosts, []byte("nope"), 0o644); err == nil {
		t.Fatal("write should be refused")
	}
	if _, err := os.Stat(filepath.Join(run2, "app", "nope")); err == nil {
		t.Fatal("missing path should ENOENT")
	}
}

// TestNodeModeSymlink: an alias graft renders as a FUSE symlink; files and
// dirs keep their modes. (The full FUSE walk is TestServe, gated on /dev/fuse.)
func TestNodeModeSymlink(t *testing.T) {
	archives := []record.Archive{{
		Run: "run-2026-06-21.001", DLE: "db", Level: 0, Archiver: "postgres", Compress: "none",
		Members: []record.Member{
			{Path: "base/", Off: 0},
			{Path: "base/5/", Off: 512},
			{Path: "base/5/2619", Off: 1024, Alias: "tables/app/public.users/data"},
		},
	}}
	members := func(runID string, level int) ([]record.Member, error) { return archives[0].Members, nil }
	tree, err := recovery.BuildTreeForRun(archives, "db", "run-2026-06-21.001", members, nil)
	if err != nil {
		t.Fatal(err)
	}
	link, ok := tree.Lookup("tables/app/public.users/data")
	if !ok {
		t.Fatal("alias missing")
	}
	if nodeMode(link) != fuse.S_IFLNK {
		t.Fatalf("alias mode = %#o, want S_IFLNK", nodeMode(link))
	}
	file, _ := tree.Lookup("base/5/2619")
	dir, _ := tree.Lookup("base/5")
	if nodeMode(file) != fuse.S_IFREG || nodeMode(dir) != fuse.S_IFDIR {
		t.Fatal("file/dir modes changed")
	}
	if got := link.LinkTarget(); got != "../../../base/5/2619" {
		t.Fatalf("link target = %q", got)
	}
}
