package skill

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withSkillsDir points Dir() at a temporary skills directory for the duration
// of a test by overriding the same executable/working-dir lookup Dir() uses.
// Dir() checks conf/skills next to the executable and in ".", so the simplest
// reliable override is to chdir into a temp root that contains conf/skills.
func withSkillsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	skills := filepath.Join(root, "conf", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	// Dir() prefers the executable's directory; the test binary lives
	// elsewhere and has no conf/skills there, so it falls through to ".".
	if got := Dir(); got != filepath.Join(".", "conf", "skills") && got != skills {
		// Accept either the relative "." form or an absolute match.
		t.Logf("Dir() resolved to %q", got)
	}
	return skills
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIndexPlainAndRich(t *testing.T) {
	skills := withSkillsDir(t)

	writeFile(t, filepath.Join(skills, "plain.md"),
		"---\nname: plain-skill\ndescription: a plain one\n---\n\nplain body\n")
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"),
		"---\nname: rich-skill\ndescription: a rich one\n---\n\nrich body referencing ref.md\n")
	writeFile(t, filepath.Join(skills, "rich", "ref.md"), "reference content\n")
	writeFile(t, filepath.Join(skills, "rich", "sub", "deep.txt"), "deep content\n")
	// A directory without SKILL.md must be ignored, not treated as a skill.
	writeFile(t, filepath.Join(skills, "notaskill", "readme.txt"), "nope\n")

	metas, err := Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(metas), metas)
	}
	// Sorted by name: plain-skill, rich-skill.
	if metas[0].Name != "plain-skill" || metas[0].Rich {
		t.Errorf("meta[0] = %+v, want plain-skill non-rich", metas[0])
	}
	if metas[1].Name != "rich-skill" || !metas[1].Rich {
		t.Errorf("meta[1] = %+v, want rich-skill rich", metas[1])
	}
	if metas[1].Description != "a rich one" {
		t.Errorf("rich description = %q", metas[1].Description)
	}
}

func TestLoadRichBody(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"),
		"---\nname: rich-skill\ndescription: d\n---\n\nrich body\n")

	body, err := Load("rich-skill")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.TrimSpace(body) != "rich body" {
		t.Errorf("body = %q, want %q", body, "rich body")
	}
}

func TestFiles(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"), "---\nname: rich-skill\ndescription: d\n---\nb\n")
	writeFile(t, filepath.Join(skills, "rich", "ref.md"), "r\n")
	writeFile(t, filepath.Join(skills, "rich", "sub", "deep.txt"), "d\n")
	writeFile(t, filepath.Join(skills, "plain.md"), "---\nname: plain\ndescription: d\n---\nb\n")

	files, err := Files("rich-skill")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := []string{"ref.md", "sub/deep.txt"} // SKILL.md excluded, sorted, forward-slashed
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Errorf("Files = %v, want %v", files, want)
	}

	plainFiles, err := Files("plain")
	if err != nil {
		t.Fatalf("Files(plain): %v", err)
	}
	if len(plainFiles) != 0 {
		t.Errorf("plain skill should have no bundled files, got %v", plainFiles)
	}
}

func TestLoadFile(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"), "---\nname: rich-skill\ndescription: d\n---\nb\n")
	writeFile(t, filepath.Join(skills, "rich", "ref.md"), "reference content\n")
	writeFile(t, filepath.Join(skills, "rich", "sub", "deep.txt"), "deep content\n")
	// A file the read must never reach.
	writeFile(t, filepath.Join(skills, "secret.md"), "secret\n")

	got, err := LoadFile("rich-skill", "ref.md")
	if err != nil {
		t.Fatalf("LoadFile ref.md: %v", err)
	}
	if strings.TrimSpace(got) != "reference content" {
		t.Errorf("got %q", got)
	}

	got, err = LoadFile("rich-skill", "sub/deep.txt")
	if err != nil {
		t.Fatalf("LoadFile sub/deep.txt: %v", err)
	}
	if strings.TrimSpace(got) != "deep content" {
		t.Errorf("got %q", got)
	}
}

func TestLoadFileRejectsTraversal(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"), "---\nname: rich-skill\ndescription: d\n---\nb\n")
	writeFile(t, filepath.Join(skills, "rich", "ref.md"), "ok\n")
	writeFile(t, filepath.Join(skills, "secret.md"), "secret\n")

	for _, p := range []string{"../secret.md", "../../etc/passwd", "/etc/passwd", "sub/../../secret.md"} {
		if _, err := LoadFile("rich-skill", p); err == nil {
			t.Errorf("LoadFile(%q) succeeded, want rejection", p)
		}
	}
}

func TestLoadFileRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is unreliable on Windows CI")
	}
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"), "---\nname: rich-skill\ndescription: d\n---\nb\n")
	writeFile(t, filepath.Join(skills, "secret.md"), "secret\n")

	link := filepath.Join(skills, "rich", "escape.md")
	if err := os.Symlink(filepath.Join(skills, "secret.md"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := LoadFile("rich-skill", "escape.md"); err == nil {
		t.Error("LoadFile followed a symlink out of the skill directory, want rejection")
	}
}

func TestLoadFileOnPlainSkillFails(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "plain.md"), "---\nname: plain\ndescription: d\n---\nb\n")
	if _, err := LoadFile("plain", "anything.md"); err == nil {
		t.Error("LoadFile on a plain skill should fail")
	}
}

func TestCreatePlain(t *testing.T) {
	skills := withSkillsDir(t)
	if _, err := Create("My Skill", "does things", "the body", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "my-skill.md")); err != nil {
		t.Fatalf("expected plain file my-skill.md: %v", err)
	}
	metas, _ := Index()
	if len(metas) != 1 || metas[0].Rich {
		t.Fatalf("expected 1 plain skill, got %+v", metas)
	}
	// A second create with the same name must fail.
	if _, err := Create("My Skill", "again", "b", nil); err == nil {
		t.Error("duplicate Create should fail")
	}
}

func TestCreateRich(t *testing.T) {
	skills := withSkillsDir(t)
	files := map[string]string{
		"ref.md":              "reference content",
		"queries/example.sql": "SELECT 1;",
	}
	if _, err := Create("data-skill", "d", "SKILL body", files); err != nil {
		t.Fatalf("Create rich: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "data-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected SKILL.md: %v", err)
	}
	got, err := LoadFile("data-skill", "queries/example.sql")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got != "SELECT 1;" {
		t.Errorf("bundled content = %q", got)
	}
	listed, _ := Files("data-skill")
	if strings.Join(listed, ",") != "queries/example.sql,ref.md" {
		t.Errorf("Files = %v", listed)
	}
	// A plain skill with the same stem must not be creatable now.
	if _, err := Create("data-skill", "d", "b", nil); err == nil {
		t.Error("Create over an existing rich skill should fail")
	}
}

func TestCreateRichRejectsBadPath(t *testing.T) {
	skills := withSkillsDir(t)
	for _, bad := range []string{"../escape.md", "/abs.md", "SKILL.md", "a/../../x"} {
		name := "s"
		if _, err := Create(name, "d", "b", map[string]string{bad: "x"}); err == nil {
			t.Errorf("Create with bad bundled path %q should fail", bad)
		}
		// The failed create must not leave a directory behind.
		if _, err := os.Stat(filepath.Join(skills, "s")); err == nil {
			t.Errorf("failed Create for %q left a skill dir behind", bad)
		}
	}
}

func TestUpdateRichWritesFiles(t *testing.T) {
	skills := withSkillsDir(t)
	writeFile(t, filepath.Join(skills, "rich", "SKILL.md"), "---\nname: rich\ndescription: old\n---\nold body\n")
	writeFile(t, filepath.Join(skills, "rich", "keep.md"), "keep me\n")

	err := Update("rich", "new desc", "new body", map[string]string{"added.md": "new file"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	body, _ := Load("rich")
	if strings.TrimSpace(body) != "new body" {
		t.Errorf("body = %q", body)
	}
	metas, _ := Index()
	if metas[0].Description != "new desc" {
		t.Errorf("description = %q", metas[0].Description)
	}
	got, err := LoadFile("rich", "added.md")
	if err != nil || got != "new file" {
		t.Errorf("added file = %q, err %v", got, err)
	}
	// A pre-existing bundled file not named in the update is left untouched.
	if kept, _ := LoadFile("rich", "keep.md"); strings.TrimSpace(kept) != "keep me" {
		t.Errorf("keep.md was disturbed: %q", kept)
	}
}

func TestUpdatePromotesPlainToRich(t *testing.T) {
	skills := withSkillsDir(t)
	if _, err := Create("promote-me", "d", "body", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Update("promote-me", "", "body v2", map[string]string{"ref.txt": "hi"}); err != nil {
		t.Fatalf("Update promote: %v", err)
	}
	// The old plain file is gone; the rich bundle exists.
	if _, err := os.Stat(filepath.Join(skills, "promote-me.md")); !os.IsNotExist(err) {
		t.Errorf("old plain file should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "promote-me", "SKILL.md")); err != nil {
		t.Errorf("expected promoted SKILL.md: %v", err)
	}
	metas, _ := Index()
	if len(metas) != 1 || !metas[0].Rich {
		t.Fatalf("expected 1 rich skill after promotion, got %+v", metas)
	}
	if got, _ := LoadFile("promote-me", "ref.txt"); got != "hi" {
		t.Errorf("bundled file = %q", got)
	}
}

func TestUpdateMissingSkillFails(t *testing.T) {
	withSkillsDir(t)
	if err := Update("nope", "d", "b", nil); err == nil {
		t.Error("Update of a missing skill should fail")
	}
}
