package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"simple-cot-chat/store"
)

// GetArtifacts returns metadata for all artifacts belonging to a session, for
// display in the right sidebar.
func (a *App) GetArtifacts(sessionID string) ([]store.ArtifactMeta, error) {
	return a.sess.GetArtifactsForSession(sessionID)
}

// GetArtifactContent returns the full artifact record (including content), for
// preview or download.
func (a *App) GetArtifactContent(id string) (store.Artifact, error) {
	return a.sess.GetArtifact(id)
}

// CreateArtifactManual persists a new artifact under the current session.
// This is the manual/dev-facing counterpart to the create-artifact tool the
// model will use once tool-calling is wired in.
func (a *App) CreateArtifactManual(title, content, contentType string) (string, error) {
	id, err := a.sess.CreateArtifact(a.sess.CurrentSession(), title, content, contentType)
	if err != nil {
		return "", err
	}
	a.enqueueArtifactMemory(id)
	return id, nil
}

// enqueueArtifactMemory queues an artifact for ingestion into memory.
//
// Non-blocking and failure-tolerant for the same reason as enqueueTurnMemory: the
// artifact is already saved, and memory not indexing it is a degraded search, not a
// failed operation. Both creation paths call this so an artifact the user made by
// hand is as searchable as one the model produced.
func (a *App) enqueueArtifactMemory(id string) {
	if a.mem == nil || id == "" {
		return
	}
	if _, err := a.mem.EnqueueArtifactIngest(func() (store.Artifact, error) {
		return a.sess.GetArtifact(id)
	}, id); err != nil {
		slog.Warn("could not queue artifact for memory", "artifact", id, "error", err)
	}
}

// artifactExtByType maps an artifact's content_type to a file extension for
// the save dialog's suggested filename. Mirrors EXT_BY_TYPE in artifacts.js.
var artifactExtByType = map[string]string{
	"markdown": "md", "python": "py", "go": "go", "javascript": "js", "typescript": "ts",
	"json": "json", "yaml": "yaml", "html": "html", "css": "css", "sql": "sql", "text": "txt",
}

func extForContentType(contentType string) string {
	if ext, ok := artifactExtByType[contentType]; ok {
		return ext
	}
	return "txt"
}

// invalidFilenameChars strips characters that aren't safe in a filename on
// any of Windows/macOS/Linux, so an artifact title with e.g. a "/" in it
// doesn't produce an invalid default filename for the save dialog.
var invalidFilenameChars = regexp.MustCompile(`[\\/:*?"<>|]+`)

func sanitizeArtifactFilename(title string) string {
	title = invalidFilenameChars.ReplaceAllString(strings.TrimSpace(title), "_")
	if title == "" {
		return "artifact"
	}
	return title
}

// SaveArtifact opens a native save dialog — defaulting to the artifact's own
// title (plus an extension inferred from its content type) as the suggested
// filename — and writes the artifact's content to wherever the user chooses.
// Returns the saved path, or "" if the user cancelled the dialog without
// choosing one.
func (a *App) SaveArtifact(id string) (string, error) {
	artifact, err := a.sess.GetArtifact(id)
	if err != nil {
		return "", err
	}

	path, err := wailsruntime.SaveFileDialog(a.ctx, wailsruntime.SaveDialogOptions{
		Title:           "Save artifact",
		DefaultFilename: sanitizeArtifactFilename(artifact.Title) + "." + extForContentType(artifact.ContentType),
	})
	if err != nil {
		return "", fmt.Errorf("open save dialog: %w", err)
	}
	if path == "" {
		return "", nil
	}

	if err := os.WriteFile(path, []byte(artifact.Content), 0o644); err != nil {
		return "", fmt.Errorf("write artifact file: %w", err)
	}
	return path, nil
}
