// Package docs holds the documentation. Its test checks that every
// relative link in the repository's Markdown files leads somewhere: the
// file exists, and a #fragment names a heading in it.
package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	fence   = regexp.MustCompile("^\\s*(```|~~~)")
	heading = regexp.MustCompile(`^#{1,6} (.*)$`)
	link    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	nonSlug = regexp.MustCompile(`[^\p{L}\p{N}_\- ]`)
)

// slug makes a heading's anchor the way GitHub does.
func slug(text string) string {
	text = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), "`", ""))
	return strings.ReplaceAll(nonSlug.ReplaceAllString(text, ""), " ", "-")
}

type mdFile struct {
	anchors map[string]bool
	links   []string // "line: url"
}

func parse(t *testing.T, path string) mdFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := mdFile{anchors: map[string]bool{}}
	seen := map[string]int{}
	fenced := false
	for i, line := range strings.Split(string(b), "\n") {
		if fence.MatchString(line) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			s := slug(m[1])
			if n, ok := seen[s]; ok {
				seen[s] = n + 1
				s = fmt.Sprintf("%s-%d", s, n+1)
			} else {
				seen[s] = 0
			}
			f.anchors[s] = true
		}
		for _, m := range link.FindAllStringSubmatch(line, -1) {
			f.links = append(f.links, fmt.Sprintf("%d: %s", i+1, m[1]))
		}
	}
	return f
}

func TestLinks(t *testing.T) {
	root := ".."
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "build") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) < 5 {
		t.Fatalf("found only %v", files)
	}
	parsed := map[string]mdFile{}
	get := func(p string) mdFile {
		if f, ok := parsed[p]; ok {
			return f
		}
		parsed[p] = parse(t, p)
		return parsed[p]
	}
	for _, file := range files {
		for _, l := range get(file).links {
			line, url, _ := strings.Cut(l, ": ")
			if regexp.MustCompile(`^[a-z]+:`).MatchString(url) {
				continue // http:, https:, mailto:
			}
			path, frag, _ := strings.Cut(url, "#")
			target := file
			if path != "" {
				target = filepath.Join(filepath.Dir(file), path)
				if _, err := os.Stat(target); err != nil {
					t.Errorf("%s:%s: %s: no such file", file, line, url)
					continue
				}
			}
			if frag != "" && strings.HasSuffix(target, ".md") && !get(target).anchors[frag] {
				t.Errorf("%s:%s: %s: no heading #%s in %s", file, line, url, frag, target)
			}
		}
	}
}
