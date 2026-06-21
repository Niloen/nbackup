// Package config loads and validates NBackup configuration files.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
	"gopkg.in/yaml.v3"
)

// Config is the top-level NBackup configuration, mirroring the PRD example.
type Config struct {
	Storage struct {
		Budget string `yaml:"budget"`
	} `yaml:"storage"`

	Cycle struct {
		MinimumAge               string `yaml:"minimum_age"`
		RequireVerifiedSuccessor bool   `yaml:"require_verified_successor"`
	} `yaml:"cycle"`

	Landing struct {
		Media string `yaml:"media"`
	} `yaml:"landing"`

	// Planner holds optional tuning. Sensible defaults apply when omitted.
	Planner struct {
		FullIntervalDays int `yaml:"full_interval_days"`
	} `yaml:"planner"`

	// GnuTarPath overrides the GNU tar binary used for archives (default "tar";
	// use "gtar" on systems where GNU tar is not the default tar).
	GnuTarPath string `yaml:"gnutar_path"`

	Media struct {
		S3 struct {
			Bucket string `yaml:"bucket"`
		} `yaml:"s3"`
		Tape struct {
			Enabled   bool   `yaml:"enabled"`
			Retention string `yaml:"retention"`
		} `yaml:"tape"`
		LocalDisk struct {
			Path string `yaml:"path"`
		} `yaml:"local-disk"`
	} `yaml:"media"`

	Sources []Source `yaml:"sources"`
}

// Source is a DLE (a backup source): one path on one host.
type Source struct {
	Host string `yaml:"host"`
	Path string `yaml:"path"`
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Name returns a stable, filesystem-safe identifier for a DLE, e.g.
// host "app01" + path "/home" -> "app01-home".
func (s Source) Name() string {
	p := strings.Trim(s.Path, "/")
	p = strings.ReplaceAll(p, "/", "-")
	if p == "" {
		p = "root"
	}
	name := s.Host + "-" + p
	name = slugStrip.ReplaceAllString(name, "_")
	return name
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("config has no sources")
	}
	for i, s := range c.Sources {
		if s.Host == "" || s.Path == "" {
			return fmt.Errorf("source %d: host and path are required", i)
		}
	}
	if c.Landing.Media == "" {
		c.Landing.Media = "local-disk"
	}
	if _, err := c.BudgetBytes(); c.Storage.Budget != "" && err != nil {
		return fmt.Errorf("storage.budget: %w", err)
	}
	if _, err := c.MinimumAge(); c.Cycle.MinimumAge != "" && err != nil {
		return fmt.Errorf("cycle.minimum_age: %w", err)
	}
	return nil
}

// BudgetBytes returns the configured storage budget in bytes, or 0 if unset.
func (c *Config) BudgetBytes() (int64, error) {
	if c.Storage.Budget == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(c.Storage.Budget)
}

// MinimumAge returns the cycle minimum age, or 0 if unset.
func (c *Config) MinimumAge() (time.Duration, error) {
	if c.Cycle.MinimumAge == "" {
		return 0, nil
	}
	return sizeutil.ParseDuration(c.Cycle.MinimumAge)
}

// FullIntervalDays returns the target days between full backups for a DLE.
func (c *Config) FullIntervalDays() int {
	if c.Planner.FullIntervalDays > 0 {
		return c.Planner.FullIntervalDays
	}
	return 7
}

// TarPath returns the configured GNU tar binary, defaulting to "tar".
func (c *Config) TarPath() string {
	if c.GnuTarPath != "" {
		return c.GnuTarPath
	}
	return "tar"
}

// MaxLevel is the highest incremental level the planner will assign (Amanda
// uses levels 0-9).
const MaxLevel = 9
