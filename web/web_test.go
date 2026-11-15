package web

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestIndexLoadsEveryScript checks index.html loads each dashboard script
// once, with core.js first and main.js (routing and startup) last.
func TestIndexLoadsEveryScript(t *testing.T) {
	html, err := fs.ReadFile(FS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	var loaded []string
	for _, m := range regexp.MustCompile(`<script src="js/([^"]+)"></script>`).FindAllStringSubmatch(string(html), -1) {
		loaded = append(loaded, m[1])
	}
	files, err := fs.Glob(FS, "static/js/*.js")
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		files[i] = strings.TrimPrefix(files[i], "static/js/")
	}
	sorted := slices.Clone(loaded)
	slices.Sort(sorted)
	if !slices.Equal(sorted, files) {
		t.Errorf("index.html loads %v; js/ has %v", loaded, files)
	}
	if len(loaded) < 2 || loaded[0] != "core.js" || loaded[len(loaded)-1] != "main.js" {
		t.Errorf("load order %v: core.js must come first and main.js last", loaded)
	}
}
