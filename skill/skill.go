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

// Dir returns the path to the "skills" directory under conf/, checked next to
// the executable and in the working directory (same convention as cotDir/
// confDir in app.go — duplicated here rather than imported, since main can't
// be imported by this package without an import cycle).
func Dir() string {
	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			p := filepath.Join(dir, "conf", "skills")
			if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
				return p
			}
		}
	}
	return "./conf/skills"
}

// Index parses the frontmatter of every markdown file in the skills directory
// and returns their metadata, sorted by name. It re-scans the directory on
// every call rather than caching, so hand-edited skill files are picked up
// immediately without an app restart (same reasoning as loadCotConfig in app.go).
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

// Create writes a new skill file under the skills directory, deriving its
// filename from name. It fails if a skill with this name already exists —
// callers should use Update to revise an existing one instead of silently
// overwriting it.
func Create(name, description, body string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	stem := sanitizeName(name)
	if stem == "" {
		return "", fmt.Errorf("name %q has no usable characters for a filename", name)
	}

	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create skills dir: %w", err)
	}

	path := filepath.Join(dir, stem+".md")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("skill %q already exists (use update_skill to revise it)", name)
	}

	if err := writeSkillFile(path, name, description, body); err != nil {
		return "", err
	}
	return name, nil
}

// Update overwrites an existing skill's body and, if description is
// non-empty, its description too (an empty description leaves the existing
// one as-is). It fails if no skill with this name exists.
func Update(name, description, body string) error {
	metas, err := Index()
	if err != nil {
		return err
	}
	for _, m := range metas {
		if m.Name != name {
			continue
		}
		if description == "" {
			description = m.Description
		}
		return writeSkillFile(m.Path, name, description, body)
	}
	return fmt.Errorf("skill %q not found (use create_skill to add it)", name)
}

// sanitizeName converts a skill name into a safe filename stem: lowercase
// alphanumerics separated by single hyphens.
func sanitizeName(name string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// writeSkillFile renders name/description as frontmatter followed by body
// and writes it to path, overwriting any existing content.
func writeSkillFile(path, name, description, body string) error {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: %s\n", description)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
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
