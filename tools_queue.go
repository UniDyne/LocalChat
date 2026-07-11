package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxQueuedTasks caps how many follow-up tasks a single queue_tasks call may
// queue at once — a safety limit, not a UX target.
const maxQueuedTasks = 20

// queueTasksTool lets the model lay out a sequence of follow-up steps for
// multi-step work that can't be finished in a single reply. Calling it ends
// the current turn immediately (see chatWithTools in app.go) — the model
// never sees this result, so it's a plain human-facing confirmation rather
// than the JSON task list itself. The frontend gets the actual list from the
// tool call's persisted arguments (ToolArgs), not this result, so this text
// is free to read as a simple status update instead of looking like a
// checklist to act on. No server-side queue state is kept; the frontend is
// the only conductor.
func (a *App) queueTasksTool() ToolDef {
	return ToolDef{
		Name: "queue_tasks",
		Description: "Queue a sequence of follow-up tasks to work through one at a time, " +
			"for multi-step work that can't be finished in a single reply. Each item should " +
			"be a self-contained instruction for that step, written as if the user were asking " +
			"it. This ends your current turn immediately — you will NOT get a chance to act on " +
			"these tasks yourself right now, so don't try. The app runs each queued task in order " +
			"on its own, feeding it back to you one at a time as a new turn, until the queue is " +
			"empty or the user stops it.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"tasks": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Ordered list of follow-up task prompts"
				}
			},
			"required": ["tasks"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			raw, _ := args["tasks"].([]any)
			tasks := make([]string, 0, len(raw))
			for _, t := range raw {
				if s, _ := t.(string); strings.TrimSpace(s) != "" {
					tasks = append(tasks, strings.TrimSpace(s))
				}
			}
			if len(tasks) == 0 {
				return "", fmt.Errorf("tasks must contain at least one non-empty string")
			}
			if len(tasks) > maxQueuedTasks {
				tasks = tasks[:maxQueuedTasks]
			}

			noun := "step"
			if len(tasks) != 1 {
				noun = "steps"
			}
			return fmt.Sprintf("Queued %d follow-up %s. They'll run automatically, one at a time, after this turn ends — do not attempt them yourself.", len(tasks), noun), nil
		},
	}
}
