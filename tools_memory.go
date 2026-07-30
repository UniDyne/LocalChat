package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"simple-cot-chat/memory"
)

// maxMemoryResults caps what one call may return, independently of the token budget.
const maxMemoryResults = 10

// searchMemoryTool lets the model search the user's memory: their notes, past
// conversations, and artifacts.
//
// The description carries explicit when-to-use AND when-not-to-use guidance, in the
// style manage_plan already uses to steer behavior. That matters more here than for
// most tools: the model decides when to search, so if it fails to recognize a
// question as memory-answerable then a perfectly good index is never queried and the
// failure is invisible — no error, just a worse answer. Under-invocation is the
// principal risk with a model-invoked design, and the description is the main lever
// against it.
func (a *App) searchMemoryTool() ToolDef {
	return ToolDef{
		Name: "search_memory",
		Description: "Search the user's own memory — their Markdown notes, earlier conversations, " +
			"and saved artifacts — and get back the most relevant excerpts with their sources. " +
			"USE THIS when the user refers to their own material or to anything from before this " +
			"conversation: \"what did I write about X\", \"what did we decide\", \"check my notes\", " +
			"questions about their projects, decisions, or documents, or any time you would " +
			"otherwise have to say you don't know something they plausibly recorded. Prefer " +
			"searching over guessing. DO NOT use it for general knowledge, for the contents of a " +
			"file you can read directly with the file tools, or for something already stated " +
			"earlier in this conversation. If the first search misses, try again with different " +
			"wording before concluding nothing is there.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "What to look for. A natural-language question or phrase works best; keep the user's own distinctive words, since exact terms and identifiers are matched too."
				},
				"limit": {
					"type": "integer",
					"description": "Maximum excerpts to return (1-10, default 6)."
				},
				"source_types": {
					"type": "array",
					"items": {"type": "string", "enum": ["directory", "conversation", "artifact"]},
					"description": "Restrict to particular kinds of source. Omit to search everything."
				},
				"since": {
					"type": "string",
					"description": "Only material ingested on or after this date (YYYY-MM-DD)."
				},
				"until": {
					"type": "string",
					"description": "Only material ingested on or before this date (YYYY-MM-DD)."
				},
				"expand": {
					"type": "boolean",
					"description": "Follow links and related-note connections outward from the best matches, which finds material that does not itself mention the search words. Default true. Set false for a narrow lookup where only exact matches are wanted."
				}
			},
			"required": ["query"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			if a.mem == nil {
				return "", fmt.Errorf("memory is not initialized")
			}
			query, _ := args["query"].(string)
			query = strings.TrimSpace(query)
			if query == "" {
				return "", fmt.Errorf("query is required and must not be empty")
			}

			// Expansion defaults on. It can only add candidates — an expanded chunk
			// scores strictly below the chunk that led to it — so the downside is a
			// weaker result at the tail rather than a displaced good one, while the
			// upside is finding notes that never mention the query's words at all.
			opts := memory.SearchOptions{Limit: 6, Expand: true}
			if v, ok := args["expand"].(bool); ok {
				opts.Expand = v
			}
			// The current conversation is already in the model's context, and since
			// turns ingest automatically it is also in memory. Returning it would spend
			// the token budget restating what the model can already see, and displace a
			// result that would have been new.
			opts.ExcludeSessionID = a.sess.CurrentSession()
			if v, ok := args["limit"].(float64); ok && v > 0 {
				opts.Limit = int(v)
				if opts.Limit > maxMemoryResults {
					opts.Limit = maxMemoryResults
				}
			}
			if raw, ok := args["source_types"].([]any); ok {
				for _, item := range raw {
					if s, ok := item.(string); ok && s != "" {
						opts.SourceTypes = append(opts.SourceTypes, s)
					}
				}
			}
			opts.Since, _ = args["since"].(string)
			opts.Until, _ = args["until"].(string)

			results, rep, err := a.mem.Search(context.Background(), query, opts)
			if err != nil {
				return "", err
			}
			return formatMemoryResults(query, results, rep), nil
		},
	}
}

// edgeLabel renders an edge kind in words the model can reason about. The stored
// kinds are schema identifiers, not English.
func edgeLabel(kind string) string {
	switch kind {
	case "link":
		return "link the user wrote"
	case "inferred_link":
		return "likely cross-reference"
	case "next", "prev":
		return "same-document adjacency"
	case "similar":
		return "topical similarity"
	case "entity":
		return "shared name or term"
	default:
		return kind
	}
}

// formatMemoryResults renders results for the model.
//
// Output is load-bearing because the model consumes it directly: each block carries
// the source and heading path so a claim can be attributed, and a miss explains
// itself rather than returning a bare "no results" that invites the model to assume
// the corpus is empty.
func formatMemoryResults(query string, results []memory.Result, rep memory.SearchReport) string {
	var b strings.Builder

	if len(results) == 0 {
		fmt.Fprintf(&b, "No memory matched %q.\n", query)
		if rep.Candidates == 0 {
			b.WriteString("Nothing in memory shares any term with that query. ")
			b.WriteString("Consider rephrasing with the user's own wording, or the memory may not cover this topic.\n")
		} else {
			fmt.Fprintf(&b, "%d candidates were considered but none passed the filters.\n", rep.Candidates)
		}
		if rep.VectorSkipped != "" {
			fmt.Fprintf(&b, "Note: semantic search was unavailable (%s), so only keyword, "+
				"entity and character matching were used.\n", rep.VectorSkipped)
		}
		return b.String()
	}

	fmt.Fprintf(&b, "%d memory excerpt(s) for %q:\n", len(results), query)
	if rep.VectorSkipped != "" {
		fmt.Fprintf(&b, "(semantic search unavailable: %s — keyword matching only)\n", rep.VectorSkipped)
	}

	for i, r := range results {
		b.WriteString("\n---\n")
		label := r.SourceRef
		if r.Title != "" && r.Title != r.SourceRef {
			label = fmt.Sprintf("%s (%s)", r.Title, r.SourceRef)
		}
		fmt.Fprintf(&b, "[%d] %s — %s, relevance %.2f\n", i+1, label, r.SourceType, r.Score)
		if r.HeadingPath != "" {
			fmt.Fprintf(&b, "Section: %s\n", r.HeadingPath)
		}
		// Expanded results are labelled because they are a different kind of evidence:
		// this excerpt does not necessarily mention the query at all, it is connected
		// to something that does. Saying so lets the model weigh it accordingly instead
		// of treating a two-hop neighbour as a direct answer.
		if r.Expanded {
			fmt.Fprintf(&b, "Found by following a %s connection (%d hop(s)) rather than by "+
				"matching the query directly.\n", edgeLabel(r.Via), r.Depth)
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(r.Text))
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	b.WriteString("Cite the source when you use an excerpt. If none of these answer the " +
		"question, say so rather than inferring beyond what they state.\n")
	return b.String()
}
