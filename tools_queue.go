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
// multi-step work that can't be finished in a single reply. Unlike the other
// tools, its result is not a human-readable confirmation — it's the
// validated/possibly-truncated JSON task list itself, which the frontend
// parses directly to drive an automatic one-step-at-a-time continuation loop.
// No server-side queue state is kept; the frontend is the only conductor.
func (a *App) queueTasksTool() ToolDef {
	return ToolDef{
		Name: "queue_tasks",
		Description: "Queue a sequence of follow-up tasks to work through one at a time, " +
			"for multi-step work that can't be finished in a single reply. Each item should " +
			"be a self-contained instruction for that step, written as if the user were asking " +
			"it. The app automatically runs each queued task in order **after the end of this turn**, feeding " +
			"you one at a time, until the queue is empty or the user stops it.",
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

			
			/*
			echo, err := json.Marshal(tasks)
			if err != nil {
				return "", fmt.Errorf("marshal queued tasks: %w", err)
			}
			
			return string(echo), nil
			*/
			return "Tasks queued. Will execute after this turn.", nil
		},
	}
}
