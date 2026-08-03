package main

import (
	"path/filepath"
	"testing"

	"simple-cot-chat/store"
)

// newArtifactTestApp opens a fresh temp-backed store with one artifact and
// returns an App wired to it plus that artifact's id.
func newArtifactTestApp(t *testing.T, content string) (*App, string) {
	t.Helper()
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "skills-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sid, err := st.CreateSession("test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	id, err := st.CreateArtifact(sid, "notes", content, "markdown")
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	return &App{sess: st}, id
}

func TestCollectSkillFilesResolvesArtifacts(t *testing.T) {
	app, artID := newArtifactTestApp(t, "ARTIFACT BODY")

	files, err := app.collectSkillFiles(
		map[string]any{"inline.md": "inline content"},
		map[string]any{"ref/from-artifact.md": artID},
	)
	if err != nil {
		t.Fatalf("collectSkillFiles: %v", err)
	}
	if files["inline.md"] != "inline content" {
		t.Errorf("inline.md = %q", files["inline.md"])
	}
	if files["ref/from-artifact.md"] != "ARTIFACT BODY" {
		t.Errorf("artifact-backed file = %q, want ARTIFACT BODY", files["ref/from-artifact.md"])
	}
}

func TestCollectSkillFilesArtifactsOnly(t *testing.T) {
	app, artID := newArtifactTestApp(t, "X")
	files, err := app.collectSkillFiles(nil, map[string]any{"a.md": artID})
	if err != nil {
		t.Fatalf("collectSkillFiles: %v", err)
	}
	if len(files) != 1 || files["a.md"] != "X" {
		t.Errorf("files = %v", files)
	}
}

func TestCollectSkillFilesRejectsConflict(t *testing.T) {
	app, artID := newArtifactTestApp(t, "X")
	_, err := app.collectSkillFiles(
		map[string]any{"dup.md": "inline"},
		map[string]any{"dup.md": artID},
	)
	if err == nil {
		t.Error("a path given both inline and as an artifact should fail")
	}
}

func TestCollectSkillFilesUnknownArtifact(t *testing.T) {
	app, _ := newArtifactTestApp(t, "X")
	if _, err := app.collectSkillFiles(nil, map[string]any{"a.md": "no-such-id"}); err == nil {
		t.Error("referencing a missing artifact should fail")
	}
}

func TestCollectSkillFilesEmptyArtifactID(t *testing.T) {
	app, _ := newArtifactTestApp(t, "X")
	if _, err := app.collectSkillFiles(nil, map[string]any{"a.md": ""}); err == nil {
		t.Error("empty artifact id should fail")
	}
}

func TestCollectSkillFilesNilArgs(t *testing.T) {
	app, _ := newArtifactTestApp(t, "X")
	files, err := app.collectSkillFiles(nil, nil)
	if err != nil {
		t.Fatalf("collectSkillFiles: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil map for no files, got %v", files)
	}
}
