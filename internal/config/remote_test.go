package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadYAML writes cfg to a temp file and loads it, returning the config or the load error.
func loadYAML(t *testing.T, cfg string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "nbackup.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const baseMedia = `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
`

func TestHostsParseAndResolve(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
hosts:
  app01:
    ssh:
      user: backup
      port: "2222"
      identity_file: /keys/id
      options: ["-o", "StrictHostKeyChecking=accept-new"]
    state_dir: /var/lib/nbackup/snar
    archivers:
      gnutar:
        tar_path: /usr/bin/tar
sources:
  default:
    app01: [/home]
    localhost: [/etc]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ssh, ok := c.RemoteHost("app01")
	if !ok {
		t.Fatal("app01 should resolve as remote")
	}
	if ssh.User != "backup" || ssh.Port != "2222" {
		t.Fatalf("ssh fields wrong: %+v", ssh)
	}
	if len(ssh.Options) != 2 {
		t.Fatalf("options not parsed: %v", ssh.Options)
	}
	// state_dir is host-level (not an ssh field); tar_path is an archiver-type override.
	if got := c.StateDirFor("app01"); got != "/var/lib/nbackup/snar" {
		t.Fatalf("state_dir wrong: %q", got)
	}
	if got := c.ArchiverOverrides("app01", "gnutar")["tar_path"]; got != "/usr/bin/tar" {
		t.Fatalf("gnutar tar_path override wrong: %q", got)
	}
	// An unlisted non-localhost host is remote by default (auto-remote); localhost is local.
	if _, ok := c.RemoteHost("unlisted"); !ok {
		t.Fatal("unlisted non-localhost host must be remote by default")
	}
	if _, ok := c.RemoteHost("localhost"); ok {
		t.Fatal("localhost must be local")
	}
}

func TestStateDirResolution(t *testing.T) {
	// Precedence: per-host state_dir wins; else the fleet-wide state_dir; else the default.
	c, err := loadYAML(t, baseMedia+`
state_dir: /srv/state
hosts:
  app01:
    ssh: { user: backup }
    state_dir: /var/lib/nbackup/snar
sources:
  default:
    app01: [/home]
    web01: [/srv]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.StateDirFor("app01"); got != "/var/lib/nbackup/snar" {
		t.Fatalf("per-host state_dir should win, got %q", got)
	}
	if got := c.StateDirFor("web01"); got != "/srv/state" {
		t.Fatalf("host without override should get the fleet-wide state_dir, got %q", got)
	}
}

func TestStateDirDefault(t *testing.T) {
	// Unset everywhere, a host falls back to the dedicated default (beside the workdir).
	c, err := loadYAML(t, baseMedia+`
sources:
  default:
    app01: [/home]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.StateDirFor("app01"); got != DefaultStateDir {
		t.Fatalf("unset state_dir should default to %q, got %q", DefaultStateDir, got)
	}
}

func TestHostStateDirOptional(t *testing.T) {
	// state_dir is optional; an unset host resolves remote and uses the default root.
	c, err := loadYAML(t, baseMedia+`
hosts:
  app01:
    ssh: { user: backup }
sources:
  default:
    app01: [/home]
`)
	if err != nil {
		t.Fatalf("host without state_dir should load: %v", err)
	}
	if _, ok := c.RemoteHost("app01"); !ok {
		t.Fatal("app01 should resolve as remote")
	}
	if got := c.StateDirFor("app01"); got != DefaultStateDir {
		t.Fatalf("host without state_dir should use the default, got %q", got)
	}
}

func TestHostsKnownFieldsRejectsStray(t *testing.T) {
	_, err := loadYAML(t, baseMedia+`
hosts:
  app01:
    ssh: { user: backup, password: hunter2 }
sources:
  default:
    app01: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("a literal secret key must be rejected, got %v", err)
	}
}

func TestEncryptAtClientRequiresCompressClient(t *testing.T) {
	_, err := loadYAML(t, baseMedia+`
hosts:
  app01: { ssh: { user: b } }
dumptypes:
  secure:
    compress: { at: server }
    encrypt: { scheme: gpg, recipient: k@x, at: client }
sources:
  secure:
    app01: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "requires compress.at: client") {
		t.Fatalf("want compress.at:client requirement, got %v", err)
	}
}

func TestTransformClientRequiresRemoteHost(t *testing.T) {
	// compress/encrypt: client on a localhost DLE is rejected — there is no client to run
	// the transform on. (A non-localhost host is remote by default and is allowed.)
	_, err := loadYAML(t, baseMedia+`
dumptypes:
  secure:
    compress: { at: client }
sources:
  secure:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "requires a remote host") {
		t.Fatalf("want remote-host requirement, got %v", err)
	}
}

func TestNonLocalhostIsRemoteByDefault(t *testing.T) {
	// No hosts: block at all — a non-localhost source host resolves remote with defaults.
	c, err := loadYAML(t, baseMedia+`
sources:
  default:
    app01: [/home]
    localhost: [/etc]
`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.RemoteHost("app01"); !ok {
		t.Fatal("app01 (not localhost, no hosts: entry) must be remote by default")
	}
	if _, ok := c.RemoteHost("localhost"); ok {
		t.Fatal("localhost must be local")
	}
}

func TestGlobalSSHDefaultsAndOverride(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
ssh:
  user: backup
  identity_file: /keys/global
  options: ["-o", "StrictHostKeyChecking=accept-new"]
hosts:
  app01:
    ssh: { user: root }
sources:
  default:
    app01: [/home]
    web01: [/srv]
`)
	if err != nil {
		t.Fatal(err)
	}
	// web01: inherits all global defaults (no per-host block).
	web, _ := c.RemoteHost("web01")
	if web.User != "backup" || web.IdentityFile != "/keys/global" || len(web.Options) != 2 {
		t.Fatalf("web01 should inherit global ssh defaults: %+v", web)
	}
	// app01: overrides user, inherits identity_file + options.
	app, _ := c.RemoteHost("app01")
	if app.User != "root" || app.IdentityFile != "/keys/global" || len(app.Options) != 2 {
		t.Fatalf("app01 should override user, inherit the rest: %+v", app)
	}
}

func TestValidClientSideEncryptionLoads(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
hosts:
  app01: { ssh: { user: b } }
dumptypes:
  secure:
    compress: { at: client }
    encrypt: { scheme: gpg, recipient: k@x, at: client }
sources:
  secure:
    app01: [/home]
`)
	if err != nil {
		t.Fatalf("valid client-side config should load: %v", err)
	}
	if c.EncryptionFor("secure").At != "client" || c.CompressionFor("secure").At != "client" {
		t.Fatal("client-side placement not parsed")
	}
}

// TestCompressionForOverridesWholesale locks the per-dumptype compression override as
// the peer of encryption: a dumptype's own `compress` block replaces the config-wide
// default wholesale, while a dumptype without one inherits it.
func TestCompressionForOverridesWholesale(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
compress:
  scheme: zstd
  level: 3
dumptypes:
  fast:
    compress:
      scheme: gzip
      level: 9
  plain:
    archiver: default
sources:
  fast:
    localhost: [/a]
  plain:
    localhost: [/b]
`)
	if err != nil {
		t.Fatalf("config should load: %v", err)
	}
	if got := c.CompressionFor("fast"); got.SchemeName() != "gzip" || got.Level != 9 {
		t.Errorf("CompressionFor(fast) = %+v, want gzip/9 (wholesale override)", got)
	}
	// "plain" sets no compress block, so it inherits the config-wide default.
	if got := c.CompressionFor("plain"); got.SchemeName() != "zstd" || got.Level != 3 {
		t.Errorf("CompressionFor(plain) = %+v, want zstd/3 (inherited default)", got)
	}
	// An unknown dumptype also falls back to the default.
	if got := c.CompressionFor("nope"); got.SchemeName() != "zstd" {
		t.Errorf("CompressionFor(nope) = %+v, want the zstd default", got)
	}
}
