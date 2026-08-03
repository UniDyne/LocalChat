// Package skill discovers and loads markdown-file-backed skills: short
// frontmatter-described capabilities whose full body is only loaded into a
// chat's context on demand (via a tool call), keeping the index cheap to
// keep in context at all times.
//
// A skill comes in one of two shapes, both living under the skills directory:
//
//   - a plain "foo.md" file — the original form; the whole file is the skill.
//   - a "foo/" directory containing a SKILL.md file — a "rich" skill. SKILL.md
//     uses the same frontmatter (name, description) and body, but may reference
//     sibling files in its directory that the model can fetch on demand (see
//     Files and LoadFile) rather than paying to load them all up front.
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
	Path        string `json:"path"` // absolute path to the source .md / SKILL.md file, for lazy body load
	Rich        bool   `json:"rich"` // true when the skill is a directory bundle (Path is its SKILL.md)
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
		if e.IsDir() {
			// A directory is a "rich" skill only if it holds a SKILL.md.
			path := filepath.Join(dir, e.Name(), skillFile)
			if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
				continue
			}
			fields, _, err := readFrontmatter(path)
			if err != nil {
				continue
			}
			name := fields["name"]
			if name == "" {
				name = e.Name() // fall back to the directory name
			}
			metas = append(metas, Meta{Name: name, Description: fields["description"], Path: path, Rich: true})
			continue
		}
		if strings.ToLower(filepath.Ext(e.Name())) != ".md" {
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

// skillFile is the fixed name of the entry-point markdown file inside a rich
// (directory-backed) skill.
const skillFile = "SKILL.md"

// Create writes a new skill under the skills directory, deriving its filename
// (or directory name) from name. It fails if a skill with this name already
// exists — callers should use Update to revise an existing one instead of
// silently overwriting it.
//
// With no files, it writes a plain "stem.md". With one or more files, it
// instead creates a rich skill: a "stem/" directory whose SKILL.md holds the
// name/description/body and which bundles each files[relPath] = content
// alongside (see Files/LoadFile). Bundled-file paths are confined to the
// skill's directory; an invalid or escaping path fails the whole create.
func Create(name, description, body string, files map[string]string) (string, error) {
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

	plainPath := filepath.Join(dir, stem+".md")
	richDir := filepath.Join(dir, stem)
	// Guard against collisions in either shape, so a plain "foo.md" and a rich
	// "foo/" can never both claim the same name.
	if _, err := os.Stat(plainPath); err == nil {
		return "", fmt.Errorf("skill %q already exists (use update_skill to revise it)", name)
	}
	if _, err := os.Stat(richDir); err == nil {
		return "", fmt.Errorf("skill %q already exists (use update_skill to revise it)", name)
	}

	if len(files) == 0 {
		if err := writeSkillFile(plainPath, name, description, body); err != nil {
			return "", err
		}
		return name, nil
	}

	if err := os.MkdirAll(richDir, 0o755); err != nil {
		return "", fmt.Errorf("create skill dir: %w", err)
	}
	if err := writeSkillFile(filepath.Join(richDir, skillFile), name, description, body); err != nil {
		os.RemoveAll(richDir)
		return "", err
	}
	for rel, content := range files {
		if err := writeBundledFile(richDir, rel, content); err != nil {
			os.RemoveAll(richDir) // don't leave a half-written skill behind
			return "", err
		}
	}
	return name, nil
}

// Update overwrites an existing skill's body and, if description is non-empty,
// its description too (an empty description leaves the existing one as-is). It
// fails if no skill with this name exists.
//
// When files is non-empty, each files[relPath] = content is (over)written into
// the skill's bundle directory. A plain skill is promoted to a rich one in
// place for this — "stem.md" becomes "stem/SKILL.md" with the files alongside.
// Bundled files already present but not named in files are left untouched.
func Update(name, description, body string, files map[string]string) error {
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

		if len(files) == 0 {
			return writeSkillFile(m.Path, name, description, body)
		}

		var bundleDir string
		if m.Rich {
			bundleDir = filepath.Dir(m.Path)
			if err := writeSkillFile(m.Path, name, description, body); err != nil {
				return err
			}
		} else {
			// Promote the plain "stem.md" to a rich "stem/SKILL.md" bundle.
			bundleDir = strings.TrimSuffix(m.Path, filepath.Ext(m.Path))
			if _, err := os.Stat(bundleDir); err == nil {
				return fmt.Errorf("cannot make skill %q rich: %q already exists", name, bundleDir)
			}
			if err := os.MkdirAll(bundleDir, 0o755); err != nil {
				return fmt.Errorf("create skill dir: %w", err)
			}
			if err := writeSkillFile(filepath.Join(bundleDir, skillFile), name, description, body); err != nil {
				return err
			}
			if err := os.Remove(m.Path); err != nil {
				return fmt.Errorf("remove old plain skill file: %w", err)
			}
		}
		for rel, content := range files {
			if err := writeBundledFile(bundleDir, rel, content); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("skill %q not found (use create_skill to add it)", name)
}

// writeBundledFile writes one auxiliary file into a rich skill's directory,
// named by a path relative to that directory. The path is confined to the
// directory (rejecting "..", absolute paths, and the reserved SKILL.md name),
// and any intermediate subdirectories are created. This is the write-side
// mirror of LoadFile; containment here is purely lexical, since the caller
// owns the freshly created skill directory.
func writeBundledFile(bundleDir, relPath, content string) error {
	rp := strings.TrimSpace(relPath)
	if rp == "" {
		return fmt.Errorf("bundled file path is empty")
	}
	clean := filepath.Clean(filepath.FromSlash(rp))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("bundled file path %q is invalid or escapes the skill directory", relPath)
	}
	if strings.EqualFold(clean, skillFile) {
		return fmt.Errorf("bundled file %q conflicts with the skill's SKILL.md; put its content in the body instead", relPath)
	}
	full := filepath.Join(bundleDir, clean)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create dir for %q: %w", relPath, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write bundled file %q: %w", relPath, err)
	}
	return nil
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

// Files returns the relative paths of the auxiliary files bundled with a rich
// skill — every file in its directory tree except the SKILL.md entry point —
// so the model can see what it may fetch via LoadFile. Paths are relative to
// the skill's directory and use forward slashes. A plain (non-rich) skill has
// no bundled files, so this returns nil for one.
func Files(name string) ([]string, error) {
	metas, err := Index()
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Name != name {
			continue
		}
		if !m.Rich {
			return nil, nil
		}
		root := filepath.Dir(m.Path)
		var files []string
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel == skillFile {
				return nil // the entry point is loaded via Load, not listed as an attachment
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("list files for skill %q: %w", name, walkErr)
		}
		sort.Strings(files)
		return files, nil
	}
	return nil, fmt.Errorf("skill %q not found", name)
}

// LoadFile returns the contents of a single auxiliary file bundled with a rich
// skill, named by a path relative to the skill's directory (as returned by
// Files). The relative path is confined to the skill's directory: any attempt
// to escape it (via "..", an absolute path, or a symlink pointing outside) is
// rejected, so a skill can only ever hand back its own files.
func LoadFile(name, relPath string) (string, error) {
	metas, err := Index()
	if err != nil {
		return "", err
	}
	for _, m := range metas {
		if m.Name != name {
			continue
		}
		if !m.Rich {
			return "", fmt.Errorf("skill %q is a plain skill and has no bundled files", name)
		}
		root := filepath.Dir(m.Path)

		clean := filepath.Clean("/" + filepath.FromSlash(relPath)) // anchor at root so ".." can't climb above it
		full := filepath.Join(root, clean)

		// Resolve symlinks before the containment check so a link inside the
		// directory can't point the read at a file outside it.
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			return "", fmt.Errorf("read skill file %q: %w", relPath, err)
		}
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", fmt.Errorf("resolve skill %q: %w", name, err)
		}
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q is outside skill %q", relPath, name)
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			return "", fmt.Errorf("read skill file %q: %w", relPath, err)
		}
		return string(data), nil
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
