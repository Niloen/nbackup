package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// MediumList is a landing route: one or more medium names, primary first. The
// primary is the medium the single-landing consumers (accounting, read preference,
// sync's default source) treat as "the landing"; the rest are fan-out targets every
// archive is also written to. YAML accepts a scalar (`landing: s3`) or a sequence
// (`landing: [s3, gdrive]`), so the common single-medium spelling stays a plain name.
type MediumList []string

// UnmarshalYAML decodes a scalar into a one-entry list, or a sequence into its entries.
func (l *MediumList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var name string
		if err := node.Decode(&name); err != nil {
			return err
		}
		*l = MediumList{name}
		return nil
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		*l = names
		return nil
	default:
		return fmt.Errorf("landing must be a media name or a list of them")
	}
}

// MarshalYAML renders a one-entry list back as its scalar form, so a config written
// by `nb init` round-trips to the plain spelling; longer lists render as a sequence.
func (l MediumList) MarshalYAML() (any, error) {
	if len(l) == 1 {
		return l[0], nil
	}
	return []string(l), nil
}

// Primary is the route's first medium — "the landing" for every single-medium
// consumer — or "" for an empty route.
func (l MediumList) Primary() string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}
