// Package skill discovers and loads markdown-file-backed skills: short
// frontmatter-described capabilities whose full body is only loaded into a
// chat's context on demand (via a tool call), keeping the index cheap to
// keep in context at all times.
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Meta is the frontmatter-derived summary of a skill — cheap enough to keep
// fully in context (e.g. as a tool-call result) without loading the body.
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"` // absolute path to the source .md file, for lazy body load
}

// Dir returns the path to the "skills" directory, checked next to the
// executable and in the working directory (same convention as cotDir in app.go).
func Dir() string {
	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			p := filepath.Join(dir, "skills")
			if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
				return p
			}
		}
	}
	return "./skills"
}

// Index parses the frontmatter of every markdown file in the skills directory
// and returns their metadata, sorted by name. It re-scans the directory on
// every call rather than caching, so hand-edited skill files are picked up
// immediately without an app restart (same reasoning as cotPrompt in app.go).
func Index() ([]Meta, error) {
	dir := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fields, _, err := readFrontmatter(path)
		if err != nil {
			continue // skip unreadable files rather than fail the whole index
		}
		name := fields["name"]
		if name == "" {
			name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		metas = append(metas, Meta{Name: name, Description: fields["description"], Path: path})
	}

	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas, nil
}

// Load returns the full markdown body (frontmatter stripped) for the named skill.
func Load(name string) (string, error) {
	metas, err := Index()
	if err != nil {
		return "", err
	}
	for _, m := range metas {
		if m.Name == name {
			_, body, err := readFrontmatter(m.Path)
			if err != nil {
				return "", fmt.Errorf("read skill %q: %w", name, err)
			}
			return body, nil
		}
	}
	return "", fmt.Errorf("skill %q not found", name)
}

// readFrontmatter splits a skill file into its frontmatter fields and body.
// This is a hand-rolled minimal parser, not a full YAML parser — skill files
// only need a couple of flat single-line string fields (name, description),
// so multi-line scalars and nested/list values are intentionally unsupported.
func readFrontmatter(path string) (map[string]string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	lines := strings.Split(string(data), "\n")
	fields := map[string]string{}

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		// No frontmatter block — treat the whole file as the body.
		return fields, string(data), nil
	}

	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
		if idx := strings.Index(lines[i], ":"); idx > 0 {
			key := strings.TrimSpace(lines[i][:idx])
			val := strings.TrimSpace(lines[i][idx+1:])
			fields[key] = val
		}
	}

	if closeIdx == -1 {
		// Unterminated frontmatter block — treat everything as body.
		return map[string]string{}, string(data), nil
	}

	body := strings.TrimLeft(strings.Join(lines[closeIdx+1:], "\n"), "\n")
	return fields, body, nil
}
