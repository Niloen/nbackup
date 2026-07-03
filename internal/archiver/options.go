package archiver

import "fmt"

// Options are generic key/value parameters from an archiver definition (e.g.
// "tar_path", "one-file-system"). The incremental-state root is not among them: it is a
// host-level location passed to Open separately (see Factory), not a format property.
type Options map[string]string

// Get returns the value for a key, or "".
func (o Options) Get(key string) string { return o[key] }

// Bool parses a boolean option, returning def when unset. An unparseable value
// is an error — a typo'd value must be as loud as the typo'd key the registry's
// KnownOptions check already rejects, not silently the default.
func (o Options) Bool(key string, def bool) (bool, error) {
	switch v := o[key]; v {
	case "":
		return def, nil
	case "true", "yes", "1", "on":
		return true, nil
	case "false", "no", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("option %q: invalid boolean %q (use true/false, yes/no, 1/0, on/off)", key, v)
	}
}
