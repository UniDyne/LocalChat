package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"simple-cot-chat/skill"
)

// searchSkillsTool lets the model list available skills (name + description)
// without loading any skill body into context, optionally filtered by keyword.
func (a *App) searchSkillsTool() ToolDef {
	return ToolDef{
		Name:        "search_skills",
		Description: "List available skills (name and short description), optionally filtered by a keyword. Use this before load_skill to find the right skill name.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Optional keyword to filter skills by name/description"}
			}
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			metas, err := skill.Index()
			if err != nil {
				return "", err
			}

			query, _ := args["query"].(string)
			query = strings.ToLower(strings.TrimSpace(query))

			var b strings.Builder
			count := 0
			for _, m := range metas {
				if query != "" && !strings.Contains(strings.ToLower(m.Name+" "+m.Description), query) {
					continue
				}
				fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
				count++
			}
			if count == 0 {
				return "No matching skills found.", nil
			}
			return b.String(), nil
		},
	}
}

// loadSkillTool lets the model load the full body of a named skill into context.
func (a *App) loadSkillTool() ToolDef {
	return ToolDef{
		Name:        "load_skill",
		Description: "Load the full instructions for a skill by name (as returned by search_skills) so you can follow them for this task.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The skill's name, exactly as returned by search_skills"}
			},
			"required": ["name"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return "", fmt.Errorf("name is required")
			}
			return skill.Load(name)
		},
	}
}
