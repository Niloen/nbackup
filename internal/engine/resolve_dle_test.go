package engine

import "testing"

// TestTrimDLESlash pins the normalization ResolveDLE applies to user-supplied
// DLE references: trailing slashes (tab completion) are stripped, but a root
// path's slash is the path and stays.
func TestTrimDLESlash(t *testing.T) {
	cases := map[string]string{
		"localhost:/home/":     "localhost:/home",
		"localhost:/home//":    "localhost:/home",
		"localhost:/home":      "localhost:/home",
		"localhost:/":          "localhost:/", // root path keeps its slash
		"/":                    "/",
		"/data/":               "/data",
		"localhost-data-slug/": "localhost-data-slug",
	}
	for in, want := range cases {
		if got := trimDLESlash(in); got != want {
			t.Errorf("trimDLESlash(%q) = %q, want %q", in, got, want)
		}
	}
}
