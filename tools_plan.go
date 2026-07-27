package main

import (
	"encoding/json"
	"fmt"
	"strings"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"simple-cot-chat/store"
)

// maxPlanSteps is a sanity cap on a single manage_plan call, not a UX target:
// full-replace semantics mean a legitimately long plan resends every step on
// every call, so this only guards against a degenerate/runaway call.
const maxPlanSteps = 200

// planStatuses are the only statuses manage_plan accepts.
var planStatuses = map[string]bool{
	"pending": true, "in_progress": true, "completed": true, "failed": true,
}

// managePlanTool lets the model create or update its working plan for
// multi-step work: a full ordered list of steps, each with a status. Calling
// it ends the current turn immediately (see chatWithTools in app.go) — the
// frontend is the conductor that decides what happens next from the plan
// state this call just persisted, not the model itself.
//
// Unlike the old queue_tasks tool it replaces, this call's result is not a
// throwaway confirmation: it echoes the full plan back, and the frontend
// re-injects that state into every auto-advanced turn (see sendAndRender/
// advancePlan in app.js) — so the model stays aware of the plan across turns
// instead of starting each step blind.
func (a *App) managePlanTool() ToolDef {
	return ToolDef{
		Name: "manage_plan",
		Description: "Create or update your working plan for multi-step work that can't be finished in a single " +
			"reply: the full ordered list of steps, each with a status. Call this to lay out the plan initially, " +
			"and again any time a step's status changes. Always send the COMPLETE list, not just what changed — " +
			"this replaces the whole plan. At most one step may be \"in_progress\" at a time. Ending your turn " +
			"after calling this hands the current in_progress step back to you as a fresh turn — you will not " +
			"get a chance to act on further steps in this same reply, so don't try.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"steps": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"content": {"type": "string", "description": "Self-contained description of this step"},
							"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "failed"]}
						},
						"required": ["content", "status"]
					},
					"description": "The complete ordered plan, every time"
				}
			},
			"required": ["steps"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			raw, _ := args["steps"].([]any)
			steps := make([]store.PlanStep, 0, len(raw))
			inProgress := 0
			for i, item := range raw {
				m, _ := item.(map[string]any)
				content, _ := m["content"].(string)
				status, _ := m["status"].(string)
				content = strings.TrimSpace(content)
				if content == "" {
					continue
				}
				if !planStatuses[status] {
					return "", fmt.Errorf("invalid status %q for step %d — must be one of pending, in_progress, completed, failed", status, i+1)
				}
				if status == "in_progress" {
					inProgress++
				}
				steps = append(steps, store.PlanStep{Seq: len(steps) + 1, Content: content, Status: status})
			}
			if len(steps) == 0 {
				return "", fmt.Errorf("steps must contain at least one entry with non-empty content")
			}
			if inProgress > 1 {
				return "", fmt.Errorf("only one step may be \"in_progress\" at a time, got %d", inProgress)
			}
			if len(steps) > maxPlanSteps {
				return "", fmt.Errorf("plan has %d steps, exceeding the %d step limit", len(steps), maxPlanSteps)
			}

			sessionID := a.sess.CurrentSession()
			if err := a.sess.SetPlan(sessionID, steps); err != nil {
				return "", err
			}
			// Emitted immediately so the sidebar checklist can pick up the
			// update while the model is still mid-turn — same reasoning as
			// create_artifact's artifact:created event.
			wailsruntime.EventsEmit(a.ctx, "plan:updated", map[string]any{"sessionId": sessionID})

			var b strings.Builder
			fmt.Fprintf(&b, "Plan updated (%d steps):\n", len(steps))
			for _, s := range steps {
				fmt.Fprintf(&b, "%d. [%s] %s\n", s.Seq, s.Status, s.Content)
			}
			return b.String(), nil
		},
	}
}
