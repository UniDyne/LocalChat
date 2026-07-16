package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxReadableFileSize caps how much of a file read_file will return, so a
// single large file (log, binary, dataset) can't blow out the model's context
// window in one tool call.
const maxReadableFileSize = 256 * 1024

// resolveInWorkDir resolves a path relative to the currently selected
// directory (see App.workDir) and confirms it doesn't escape it via "..".
// Callers already know the file tools are registered only when a directory is
// selected (see toolRegistry), but this is re-checked here since a directory
// could in principle be cleared between the model requesting the tool and the
// handler running.
func (a *App) resolveInWorkDir(rel string) (string, error) {
	root := a.workDirSnapshot()
	if root == "" {
		return "", fmt.Errorf("no directory selected — file tools are disabled")
	}
	if rel == "" {
		rel = "."
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve selected directory: %w", err)
	}
	absTarget, err := filepath.Abs(filepath.Join(absRoot, rel))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	relCheck, err := filepath.Rel(absRoot, absTarget)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the selected directory", rel)
	}
	return absTarget, nil
}

// listFilesTool lets the model list the contents of a directory (defaulting
// to the selected directory's root) one level deep.
func (a *App) listFilesTool() ToolDef {
	return ToolDef{
		Name:        "list_files",
		Description: "List files and subdirectories at a path inside the selected directory (one level deep). Omit path to list the root.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the selected directory; omit or use \".\" for the root"}
			}
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			rel, _ := args["path"].(string)
			abs, err := a.resolveInWorkDir(rel)
			if err != nil {
				return "", err
			}

			entries, err := os.ReadDir(abs)
			if err != nil {
				return "", fmt.Errorf("list %q: %w", rel, err)
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

			if len(entries) == 0 {
				return "(empty directory)", nil
			}
			var b strings.Builder
			for _, e := range entries {
				if e.IsDir() {
					fmt.Fprintf(&b, "%s/\n", e.Name())
				} else {
					info, err := e.Info()
					if err != nil {
						fmt.Fprintf(&b, "%s\n", e.Name())
						continue
					}
					fmt.Fprintf(&b, "%s (%d bytes)\n", e.Name(), info.Size())
				}
			}
			return b.String(), nil
		},
	}
}

// readFileTool lets the model read a file's full contents.
func (a *App) readFileTool() ToolDef {
	return ToolDef{
		Name:        "read_file",
		Description: "Read the contents of a file inside the selected directory.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path to the file, relative to the selected directory"}
			},
			"required": ["path"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			rel, _ := args["path"].(string)
			if rel == "" {
				return "", fmt.Errorf("path is required")
			}
			abs, err := a.resolveInWorkDir(rel)
			if err != nil {
				return "", err
			}

			info, err := os.Stat(abs)
			if err != nil {
				return "", fmt.Errorf("read %q: %w", rel, err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("%q is a directory, not a file", rel)
			}
			if info.Size() > maxReadableFileSize {
				return "", fmt.Errorf("%q is %d bytes, exceeding the %d byte limit for read_file", rel, info.Size(), maxReadableFileSize)
			}

			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("read %q: %w", rel, err)
			}
			return string(data), nil
		},
	}
}

// writeFileTool lets the model create a new file or overwrite an existing
// one's full contents, creating any missing parent directories.
func (a *App) writeFileTool() ToolDef {
	return ToolDef{
		Name:        "write_file",
		Description: "Create or overwrite a file inside the selected directory with the given content. Creates parent directories as needed. Use update_file instead if you only want to change part of an existing file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path to the file, relative to the selected directory"},
				"content": {"type": "string", "description": "Full content to write"}
			},
			"required": ["path", "content"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			rel, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if rel == "" {
				return "", fmt.Errorf("path is required")
			}
			abs, err := a.resolveInWorkDir(rel)
			if err != nil {
				return "", err
			}

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return "", fmt.Errorf("create parent directories for %q: %w", rel, err)
			}
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("write %q: %w", rel, err)
			}
			return fmt.Sprintf("wrote %q (%d bytes)", rel, len(content)), nil
		},
	}
}

// updateFileTool lets the model make a targeted edit to an existing file by
// replacing one exact, unique occurrence of a substring — mirroring a
// find-and-replace editor operation rather than requiring the model to
// rewrite the whole file via write_file for a small change.
func (a *App) updateFileTool() ToolDef {
	return ToolDef{
		Name: "update_file",
		Description: "Replace one exact occurrence of old_text with new_text in an existing file. Fails if old_text " +
			"isn't found, or is found more than once — include enough surrounding context in old_text to make it unique. " +
			"Use write_file instead to create a new file or replace one's entire contents.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path to the file, relative to the selected directory"},
				"old_text": {"type": "string", "description": "Exact existing text to replace; must match exactly once in the file"},
				"new_text": {"type": "string", "description": "Text to replace it with"}
			},
			"required": ["path", "old_text", "new_text"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			rel, _ := args["path"].(string)
			oldText, _ := args["old_text"].(string)
			newText, _ := args["new_text"].(string)
			if rel == "" || oldText == "" {
				return "", fmt.Errorf("path and old_text are required")
			}
			abs, err := a.resolveInWorkDir(rel)
			if err != nil {
				return "", err
			}

			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("read %q: %w", rel, err)
			}

			content := string(data)
			count := strings.Count(content, oldText)
			switch count {
			case 0:
				return "", fmt.Errorf("old_text not found in %q", rel)
			case 1:
				// fine — proceed below
			default:
				return "", fmt.Errorf("old_text found %d times in %q; include more context so it matches exactly once", count, rel)
			}

			updated := strings.Replace(content, oldText, newText, 1)
			if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
				return "", fmt.Errorf("write %q: %w", rel, err)
			}
			return fmt.Sprintf("updated %q", rel), nil
		},
	}
}
