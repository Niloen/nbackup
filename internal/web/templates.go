package web

import (
	"embed"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
)

//go:embed templates/*.html
var files embed.FS

// funcs are the display helpers templates call — thin wrappers over sizeutil so the
// browser view formats bytes and timestamps exactly like the CLI.
var funcs = template.FuncMap{
	"bytes": sizeutil.FormatBytes,
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return sizeutil.FormatStamp(t)
	},
	"stampsec": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return sizeutil.FormatStampSec(t)
	},
	"pct":   func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
	"level": func(l int) string { return fmt.Sprintf("L%d", l) },
	"lower": strings.ToLower,
	// encrypt renders an archive's cipher, defaulting an empty field to "none" like
	// `nb run <id>` does.
	"encrypt": func(s string) string {
		if s == "" {
			return "none"
		}
		return s
	},
}

// pages maps each page name to its parsed template set: the shared base layout plus
// that page's body, so a page's {{define "content"}} overrides the layout's block.
// Parsing once at init keeps request handling allocation-light and fails loudly at
// startup if a template is malformed.
var pages = parsePages()

func parsePages() map[string]*template.Template {
	names := []string{"home", "runs", "run", "dles", "dle", "media", "report", "status"}
	m := make(map[string]*template.Template, len(names))
	for _, n := range names {
		m[n] = template.Must(template.New("base.html").Funcs(funcs).
			ParseFS(files, "templates/base.html", "templates/"+n+".html"))
	}
	return m
}
