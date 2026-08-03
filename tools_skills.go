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
			body, err := skill.Load(name)
			if err != nil {
				return "", err
			}
			// A rich (directory-backed) skill may bundle extra files it refers
			// to. List them so the model knows what it can pull in on demand,
			// without paying to load them all now.
			files, err := skill.Files(name)
			if err != nil {
				return "", err
			}
			if len(files) > 0 {
				var b strings.Builder
				b.WriteString(body)
				b.WriteString("\n\n---\n\nThis skill bundles the following files, which you can fetch with read_skill_file when the instructions above refer to them:\n")
				for _, f := range files {
					fmt.Fprintf(&b, "- %s\n", f)
				}
				return b.String(), nil
			}
			return body, nil
		},
	}
}

// readSkillFileTool lets the model fetch one or more files bundled with a rich
// (directory-backed) skill — the files SKILL.md refers to — on demand, rather
// than having load_skill pull them all into context up front.
func (a *App) readSkillFileTool() ToolDef {
	return ToolDef{
		Name: "read_skill_file",
		Description: "Fetch one or more files bundled with a rich skill (as listed at the end of that skill's " +
			"load_skill output). Use this when a loaded skill's instructions refer to a bundled file you need.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The skill's name, exactly as returned by search_skills"},
				"paths": {
					"type": "array",
					"items": {"type": "string"},
					"description": "One or more file paths, relative to the skill, exactly as listed in the skill's load_skill output"
				}
			},
			"required": ["name", "paths"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return "", fmt.Errorf("name is required")
			}
			rawPaths, _ := args["paths"].([]any)
			var paths []string
			for _, p := range rawPaths {
				if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
					paths = append(paths, s)
				}
			}
			if len(paths) == 0 {
				return "", fmt.Errorf("at least one path is required")
			}

			var b strings.Builder
			for i, p := range paths {
				if i > 0 {
					b.WriteString("\n")
				}
				content, err := skill.LoadFile(name, p)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&b, "===== %s =====\n%s", p, content)
				if !strings.HasSuffix(content, "\n") {
					b.WriteString("\n")
				}
			}
			return b.String(), nil
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
			"rediscovering it. Fails if a skill with this name already exists; use update_skill to revise one. " +
			"Pass files (and/or artifact_files) to make a rich skill: the body becomes SKILL.md and each file is " +
			"bundled alongside it, fetchable later with read_skill_file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Short, unique skill name (e.g. \"deploy-pipeline\"); used to derive its filename"},
				"description": {"type": "string", "description": "One-line summary shown in search_skills results"},
				"body": {"type": "string", "description": "Full skill documentation/instructions, in markdown (becomes SKILL.md for a rich skill)"},
				"files": {
					"type": "object",
					"additionalProperties": {"type": "string"},
					"description": "Optional bundled files as a {relative-path: content} map (e.g. {\"queries/example.sql\": \"SELECT ...\"}). Any file makes this a rich skill. Paths must stay within the skill; \"SKILL.md\" is reserved for the body."
				},
				"artifact_files": {
					"type": "object",
					"additionalProperties": {"type": "string"},
					"description": "Optional bundled files whose content is an existing artifact, as a {relative-path: artifact-id} map (ids from list_artifacts/create_artifact). Same path rules as files; a path may not appear in both."
				}
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
			files, err := a.collectSkillFiles(args["files"], args["artifact_files"])
			if err != nil {
				return "", err
			}
			created, err := skill.Create(name, description, body, files)
			if err != nil {
				return "", err
			}
			if len(files) > 0 {
				return fmt.Sprintf("rich skill created: %s (%d bundled file(s))", created, len(files)), nil
			}
			return fmt.Sprintf("skill created: %s", created), nil
		},
	}
}

// parseSkillFiles converts the tool argument for bundled skill files — a JSON
// object of {relative-path: content} — into a Go map, rejecting any non-string
// value so a malformed argument fails loudly rather than silently dropping a
// file. A missing/empty argument yields a nil map (a plain skill).
func parseSkillFiles(arg any) (map[string]string, error) {
	if arg == nil {
		return nil, nil
	}
	raw, ok := arg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("files must be an object mapping relative paths to file contents")
	}
	if len(raw) == 0 {
		return nil, nil
	}
	files := make(map[string]string, len(raw))
	for path, v := range raw {
		content, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("content for bundled file %q must be a string", path)
		}
		files[path] = content
	}
	return files, nil
}

// collectSkillFiles merges the two ways a rich skill's bundled files can be
// sourced into the single {relative-path: content} map that skill.Create and
// skill.Update expect: filesArg carries inline content, while artifactFilesArg
// carries {relative-path: artifact-id} references whose content is pulled from
// the session's stored artifacts. A path may appear in only one of the two —
// listing it in both is treated as a mistake rather than silently resolved.
func (a *App) collectSkillFiles(filesArg, artifactFilesArg any) (map[string]string, error) {
	files, err := parseSkillFiles(filesArg)
	if err != nil {
		return nil, err
	}

	if artifactFilesArg == nil {
		return files, nil
	}
	refs, ok := artifactFilesArg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("artifact_files must be an object mapping relative paths to artifact ids")
	}
	if len(refs) == 0 {
		return files, nil
	}

	if files == nil {
		files = make(map[string]string, len(refs))
	}
	for path, v := range refs {
		id, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("artifact id for bundled file %q must be a string", path)
		}
		if id == "" {
			return nil, fmt.Errorf("artifact id for bundled file %q is empty", path)
		}
		if _, dup := files[path]; dup {
			return nil, fmt.Errorf("bundled file %q is given both inline content and an artifact id; use one", path)
		}
		art, err := a.sess.GetArtifact(id)
		if err != nil {
			return nil, fmt.Errorf("bundled file %q: %w", path, err)
		}
		files[path] = art.Content
	}
	return files, nil
}

// updateSkillTool lets the model revise an existing skill's documentation
// (and optionally its description) as its understanding improves.
func (a *App) updateSkillTool() ToolDef {
	return ToolDef{
		Name: "update_skill",
		Description: "Update an existing skill's body (and optionally its description) by name, as returned by " +
			"search_skills. Fails if the skill doesn't exist; use create_skill for a new one. Pass files and/or " +
			"artifact_files to add or overwrite bundled files (fetchable with read_skill_file); doing so promotes a " +
			"plain skill to a rich one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The skill's name, exactly as returned by search_skills"},
				"description": {"type": "string", "description": "New one-line summary; omit or leave empty to keep the existing one"},
				"body": {"type": "string", "description": "Full replacement markdown body (SKILL.md for a rich skill)"},
				"files": {
					"type": "object",
					"additionalProperties": {"type": "string"},
					"description": "Optional bundled files as a {relative-path: content} map to add or overwrite. Bundled files not listed here are left untouched. Paths must stay within the skill; \"SKILL.md\" is reserved for the body."
				},
				"artifact_files": {
					"type": "object",
					"additionalProperties": {"type": "string"},
					"description": "Optional bundled files whose content is an existing artifact, as a {relative-path: artifact-id} map (ids from list_artifacts/create_artifact). Same path rules as files; a path may not appear in both."
				}
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
			files, err := a.collectSkillFiles(args["files"], args["artifact_files"])
			if err != nil {
				return "", err
			}
			if err := skill.Update(name, description, body, files); err != nil {
				return "", err
			}
			if len(files) > 0 {
				return fmt.Sprintf("skill updated: %s (%d bundled file(s) written)", name, len(files)), nil
			}
			return fmt.Sprintf("skill updated: %s", name), nil
		},
	}
}
