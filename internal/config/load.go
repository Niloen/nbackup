package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config file at %s — copy nbackup.example.yaml to %s and edit it, run nb init, or pass -c <path>", path, path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("config %s is empty — copy nbackup.example.yaml to %s and edit it", path, path)
	}
	var c Config
	// KnownFields rejects unknown keys so a typo in a safety-relevant field
	// (a misspelled `landing`, `cycle`, a nested compress/encrypt key) is a hard
	// error rather than a silently-ignored default. Type-specific medium and
	// archiver options still flow through their inline maps, so connection keys
	// (path, url, slots, tar_path, …) are unaffected.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %s", path, cleanYAMLError(err))
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Anchor the local/server-side default paths (workdir, state_dir, secrets_dir) to
	// where this config file lives, not wherever nb's cwd happens to be when it runs
	// (see resolveLocal) — an absolute path here still wins if the config itself sits
	// under a relative/symlinked location by the time nb reads it back.
	if abs, aerr := filepath.Abs(path); aerr == nil {
		c.configDir = filepath.Dir(abs)
	}
	return &c, nil
}

// yamlUnknownField rewrites go-yaml's "field X not found in type <T>" — which leaks
// an internal Go type name (a named `pkg.Type`, or a whole anonymous `struct { … }`
// literal for an inline block) — into a user-facing "unknown key X". The type part
// is matched to end-of-line so an anonymous struct's body (it contains spaces) is
// dropped wholesale rather than leaving its field dump behind.
var yamlUnknownField = regexp.MustCompile(`field (\S+) not found in type .*`)

// secretKeys are config keys a user might reach for to store a secret inline; the
// config never holds secrets, so the error points them at the env-var indirection.
var secretKeys = map[string]string{
	"password": "password_env", "passwd": "password_env",
	"token": "url_env", "secret": "url_env", "url": "url_env",
}

// cleanYAMLError turns go-yaml's decode error into a config-author-facing message:
// it drops the "yaml: unmarshal errors:" banner and the internal Go type name,
// leaving the line number and the offending key (e.g. `line 1: unknown key "cyle"`).
// A rejected secret-looking key gets an extra hint toward the env-var reference.
func cleanYAMLError(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "yaml: unmarshal errors:\n", "")
	s = yamlUnknownField.ReplaceAllString(s, `unknown key "$1"`)
	s = strings.TrimSpace(s)
	for key, envKey := range secretKeys {
		if strings.Contains(s, `unknown key "`+key+`"`) {
			s += fmt.Sprintf("\n(secrets are never stored in the config — reference the environment-variable name instead, e.g. `%s`)", envKey)
			break
		}
	}
	// A raw go-yaml syntax error (bad indentation, an unclosed quote, a stray tab) has
	// no "unknown key" of ours to rewrite, and reads noticeably rawer than the
	// strict-field errors above. It still carries go-yaml's own line number, so point
	// the reader at it rather than leaving a bare parser message.
	if strings.HasPrefix(s, "yaml:") && !strings.Contains(s, "unknown key") {
		s += "\n(a YAML syntax problem, not a config field — check indentation, tabs, and quoting on or just before that line)"
	}
	return s
}
