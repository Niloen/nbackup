package archiver

// Options are generic key/value parameters from an archiver definition (e.g.
// "tar_path", "one-file-system"). The incremental-state root is not among them: it is a
// host-level location passed to Open separately (see Factory), not a format property.
type Options map[string]string

// Get returns the value for a key, or "".
func (o Options) Get(key string) string { return o[key] }

// Bool parses a boolean option, returning def when unset or unparseable.
func (o Options) Bool(key string, def bool) bool {
	switch o[key] {
	case "":
		return def
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return def
	}
}
