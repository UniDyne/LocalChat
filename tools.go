package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ollama/ollama/api"
)

// ToolDef is a single tool the model may call: a name/description/JSON-schema
// (Parameters) advertised to Ollama, plus the Go handler that executes it.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON-schema-shaped tool parameters
	Handler     func(a *App, args map[string]any) (string, error)
}

// toolRegistry returns every tool available to the model. The file tools
// (list/read/write/update) are only included once the user has selected a
// directory in the UI — see App.hasWorkDir — so the model can't see or call
// them at all while they're disabled, rather than seeing them fail.
func (a *App) toolRegistry() []ToolDef {
	tools := []ToolDef{
		a.searchSkillsTool(),
		a.loadSkillTool(),
		a.createSkillTool(),
		a.updateSkillTool(),
		a.createArtifactTool(),
		a.listArtifactsTool(),
		a.getArtifactTool(),
		a.managePlanTool(),
	}
	if a.hasWorkDir() {
		tools = append(tools, a.listFilesTool(), a.readFileTool(), a.writeFileTool(), a.updateFileTool())
	}
	return tools
}

func (a *App) findTool(name string) (ToolDef, bool) {
	for _, def := range a.toolRegistry() {
		if def.Name == name {
			return def, true
		}
	}
	return ToolDef{}, false
}

// dispatchTool looks up a requested tool call by name and invokes its handler.
// The call's arguments are round-tripped through JSON rather than assumed to
// already be map[string]any: tc.Function.Arguments' exact Go type could not be
// verified in this environment (no Go toolchain available to inspect the
// vendored ollama/api package), and this works regardless of that type's
// concrete shape as long as it's JSON-marshalable, which the API type must be.
func (a *App) dispatchTool(tc api.ToolCall) (string, error) {
	def, ok := a.findTool(tc.Function.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", tc.Function.Name)
	}

	raw, err := json.Marshal(tc.Function.Arguments)
	if err != nil {
		return "", fmt.Errorf("marshal tool arguments: %w", err)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("unmarshal tool arguments: %w", err)
	}

	return def.Handler(a, args)
}

// toAPITools converts our tool registry into Ollama's api.Tools request shape.
// Parameters is round-tripped through JSON rather than constructed as a typed
// literal, since the exact Go type of api.ToolFunction.Parameters could not be
// verified in this environment — this works regardless of that type's exact
// shape, as long as it accepts a JSON-schema-style object (type/properties/required).
func toAPITools(defs []ToolDef) api.Tools {
	tools := make(api.Tools, 0, len(defs))
	for _, d := range defs {
		var t api.Tool
		t.Type = "function"
		t.Function.Name = d.Name
		t.Function.Description = d.Description
		if len(d.Parameters) > 0 {
			if err := json.Unmarshal(d.Parameters, &t.Function.Parameters); err != nil {
				slog.Warn("tool parameters schema invalid", "tool", d.Name, "error", err)
			}
		}
		tools = append(tools, t)
	}
	return tools
}
