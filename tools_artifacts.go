package main

import (
	"encoding/json"
	"fmt"
	"strings"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// createArtifactTool lets the model persist a text/markdown/code artifact
// under the current session, surfaced in the app's artifacts sidebar.
func (a *App) createArtifactTool() ToolDef {
	return ToolDef{
		Name:        "create_artifact",
		Description: "Create a persisted artifact (e.g. a document, code file, or note) visible to the user in the artifacts panel. Use for substantial content the user may want to revisit or download, not for short inline answers.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Short human-readable title"},
				"content": {"type": "string", "description": "Full artifact content"},
				"content_type": {"type": "string", "description": "e.g. markdown, python, go, javascript, text", "default": "text"}
			},
			"required": ["title", "content"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			title, _ := args["title"].(string)
			content, _ := args["content"].(string)
			contentType, _ := args["content_type"].(string)
			if title == "" || content == "" {
				return "", fmt.Errorf("title and content are required")
			}
			if contentType == "" {
				contentType = "text"
			}
			sessionID := a.sess.CurrentSession()
			id, err := a.sess.CreateArtifact(sessionID, title, content, contentType)
			if err != nil {
				return "", err
			}
			// Emitted immediately so the sidebar can pick up the new artifact
			// while the model is still mid-turn, rather than only refreshing
			// once the whole turn (and its finally-block refresh) completes.
			wailsruntime.EventsEmit(a.ctx, "artifact:created", map[string]any{
				"sessionId": sessionID, "id": id, "title": title, "contentType": contentType,
			})
			return fmt.Sprintf("artifact created: %s (%s)", id, title), nil
		},
	}
}

// listArtifactsTool lets the model see what artifacts already exist in the
// current session, so it can avoid duplicating one or reference it by id.
func (a *App) listArtifactsTool() ToolDef {
	return ToolDef{
		Name:        "list_artifacts",
		Description: "List artifacts (id, title, content type) already created in the current session.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			metas, err := a.sess.GetArtifactsForSession(a.sess.CurrentSession())
			if err != nil {
				return "", err
			}
			if len(metas) == 0 {
				return "No artifacts yet in this session.", nil
			}
			var b strings.Builder
			for _, m := range metas {
				fmt.Fprintf(&b, "- %s: %s (%s)\n", m.ID, m.Title, m.ContentType)
			}
			return b.String(), nil
		},
	}
}

// getArtifactTool lets the model fetch the full content of a previously
// created artifact by id.
func (a *App) getArtifactTool() ToolDef {
	return ToolDef{
		Name:        "get_artifact",
		Description: "Fetch the full content of an artifact by id (as returned by list_artifacts or create_artifact).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The artifact's id"}
			},
			"required": ["id"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			id, _ := args["id"].(string)
			if id == "" {
				return "", fmt.Errorf("id is required")
			}
			artifact, err := a.sess.GetArtifact(id)
			if err != nil {
				return "", err
			}
			return artifact.Content, nil
		},
	}
}
