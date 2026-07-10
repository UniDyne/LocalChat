package main

import "simple-cot-chat/skill"

// ListSkills and GetSkillBody are temporary devtools-only bindings for
// exercising skill discovery before the tool-calling loop exists. Once the
// model can reach skills via its own search/load tools (see tools_skills.go),
// these are no longer load-bearing for the app to function, but are left in
// place as a manual inspection affordance.

// ListSkills returns metadata (name + description, no body) for every skill
// found in the skills directory.
func (a *App) ListSkills() ([]skill.Meta, error) {
	return skill.Index()
}

// GetSkillBody returns the full markdown body for a named skill.
func (a *App) GetSkillBody(name string) (string, error) {
	return skill.Load(name)
}
