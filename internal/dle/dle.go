// Package dle defines the DLE (Disk List Entry) domain type: a single backup
// source. It is Amanda's central unit of "what to back up" and carries no I/O.
package dle

import (
	"regexp"
	"strings"
)

// DLE is one backup source: a path on a host, dumped by a named method.
type DLE struct {
	Host   string `yaml:"host"`
	Path   string `yaml:"path"`
	Method string `yaml:"method"` // dump method (default "gnutar")
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Name returns a stable, filesystem-safe identifier for the DLE, e.g.
// host "app01" + path "/home" -> "app01-home".
func (d DLE) Name() string {
	p := strings.Trim(d.Path, "/")
	p = strings.ReplaceAll(p, "/", "-")
	if p == "" {
		p = "root"
	}
	name := d.Host + "-" + p
	return slugStrip.ReplaceAllString(name, "_")
}

// MethodName returns the dump method, defaulting to "gnutar".
func (d DLE) MethodName() string {
	if d.Method != "" {
		return d.Method
	}
	return "gnutar"
}
