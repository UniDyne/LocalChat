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

// createSkillTool lets the model persist a brand-new skill: a markdown file
// documenting its understanding of a system, process, or problem worked out
// during this conversation, so a future session can find and load it via
// search_skills/load_skill instead of rediscovering the same ground.
func (a *App) createSkillTool() ToolDef {
	return ToolDef{
		Name: "create_skill",
		Description: "Create a new persisted skill documenting your understanding of a system, process, or problem, " +
			"learned from this conversation — so a future session can search_skills/load_skill it instead of " +
			"rediscovering it. Fails if a skill with this name already exists; use update_skill to revise one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Short, unique skill name (e.g. \"deploy-pipeline\"); used to derive its filename"},
				"description": {"type": "string", "description": "One-line summary shown in search_skills results"},
				"body": {"type": "string", "description": "Full skill documentation/instructions, in markdown"}
			},
			"required": ["name", "description", "body"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			name, _ := args["name"].(string)
			description, _ := args["description"].(string)
			body, _ := args["body"].(string)
			if name == "" || description == "" || body == "" {
				return "", fmt.Errorf("name, description, and body are required")
			}
			created, err := skill.Create(name, description, body)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("skill created: %s", created), nil
		},
	}
}

// updateSkillTool lets the model revise an existing skill's documentation
// (and optionally its description) as its understanding improves.
func (a *App) updateSkillTool() ToolDef {
	return ToolDef{
		Name: "update_skill",
		Description: "Update an existing skill's body (and optionally its description) by name, as returned by " +
			"search_skills. Fails if the skill doesn't exist; use create_skill for a new one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The skill's name, exactly as returned by search_skills"},
				"description": {"type": "string", "description": "New one-line summary; omit or leave empty to keep the existing one"},
				"body": {"type": "string", "description": "Full replacement markdown body"}
			},
			"required": ["name", "body"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			name, _ := args["name"].(string)
			description, _ := args["description"].(string)
			body, _ := args["body"].(string)
			if name == "" || body == "" {
				return "", fmt.Errorf("name and body are required")
			}
			if err := skill.Update(name, description, body); err != nil {
				return "", err
			}
			return fmt.Sprintf("skill updated: %s", name), nil
		},
	}
}
