package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

// The invocation-rate measurement (§3.6, Phase 9).
//
// This exists because two failures look identical from the outside: the model never
// asked memory, and memory answered badly. The whole subsystem's usefulness rests on
// the first not happening — a model-invoked tool that the model does not recognize as
// applicable means a perfectly good index is never queried, and nothing errors. The
// tool description is the only lever against that, so the rate has to be a measured
// number rather than an assumption.
//
// Requires a reachable Ollama, since it is a question about a real model's behaviour
// and no stub can answer it. Set OLLAMA_HOST (or rely on config.json) and run:
//
//	go test -buildvcs=false -run TestSearchMemoryInvocationRate -v .
//
// It skips rather than fails when no endpoint answers, so the suite stays green on a
// machine without one — which is the machine this was written on, so the number below
// is **unmeasured**.

// invocationCase is a question plus whether a memory search is the right response.
//
// The negatives matter as much as the positives. A model that calls search_memory on
// everything is not well-calibrated, it is just noisy — it spends tokens and latency on
// questions its own knowledge answers, and §3.6's concern is calibration, not
// enthusiasm.
type invocationCase struct {
	question string
	// wantSearch is true when the question is about the user's own material.
	wantSearch bool
}

var invocationCases = []invocationCase{
	// Should search: explicitly about the user's own material.
	{"What did I write about the retrieval pipeline?", true},
	{"Check my notes on the chunking strategy.", true},
	{"What did we decide about the fusion weights?", true},
	{"Remind me how the ingestion queue works in my project.", true},
	{"What's in my notes about DuckDB?", true},
	// Should search: implicitly about it — no possessive, but unanswerable otherwise.
	{"Why was the Leiden chunker not made the default?", true},
	{"What did the eval set measure for graph expansion?", true},
	{"Which phases of the memory plan are finished?", true},
	// Should not search: general knowledge the model has.
	{"What is the capital of France?", false},
	{"Write a haiku about autumn.", false},
	{"What does the SQL LEFT JOIN keyword do?", false},
	{"Convert 72 degrees Fahrenheit to Celsius.", false},
}

// TestSearchMemoryInvocationRate measures how often the model calls search_memory when
// it should, and how often it calls it when it should not.
func TestSearchMemoryInvocationRate(t *testing.T) {
	if testing.Short() {
		t.Skip("invocation measurement talks to a model; skipped with -short")
	}
	cfg := loadConfig()
	addr := cfg.OllamaEndpoint
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		addr = v
	}
	cli, model := dialOllama(t, addr, cfg.Model)

	// A minimal App: only the fields the tool registry and dispatch touch. Building the
	// whole startup path would drag in the real database and a Wails context, neither of
	// which this measurement needs.
	app := &App{cli: cli, model: model, mode: CotModeNone, ctx: context.Background()}
	tools := api.Tools{toolSpec(t, app.searchMemoryTool())}

	var truePos, falseNeg, falsePos, trueNeg int
	var misses, spurious []string

	var failed []string
	for _, c := range invocationCases {
		called, reply, err := askOnce(cli, model, tools, c.question)
		if err != nil {
			// Recorded, not fatal. A cold model load or one slow response should not
			// throw away the other eleven data points — and "3 of 12 requests failed"
			// is itself a more useful report than a single abort.
			failed = append(failed, fmt.Sprintf("%q: %v", c.question, err))
			continue
		}
		switch {
		case c.wantSearch && called:
			truePos++
		case c.wantSearch && !called:
			falseNeg++
			misses = append(misses, fmt.Sprintf("%q -> %s", c.question, firstLine(reply)))
		case !c.wantSearch && called:
			falsePos++
			spurious = append(spurious, c.question)
		default:
			trueNeg++
		}
	}

	wantTotal := truePos + falseNeg
	notWantTotal := falsePos + trueNeg
	recall := ratio(truePos, wantTotal)
	specificity := ratio(trueNeg, notWantTotal)

	fmt.Printf("\n=== search_memory invocation rate (%s) ===\n", model)
	fmt.Printf("called when it should:      %d/%d  (%.0f%%)\n", truePos, wantTotal, 100*recall)
	fmt.Printf("correctly stayed quiet:    %d/%d  (%.0f%%)\n", trueNeg, notWantTotal, 100*specificity)
	for _, m := range misses {
		fmt.Printf("  MISSED: %s\n", m)
	}
	for _, s := range spurious {
		fmt.Printf("  SPURIOUS: %q\n", s)
	}
	for _, f := range failed {
		fmt.Printf("  REQUEST FAILED: %s\n", f)
	}
	fmt.Println()

	// Too few answers and the percentages mean nothing — say so rather than reporting
	// a rate derived from three questions.
	if wantTotal+notWantTotal < len(invocationCases)/2 {
		t.Skipf("only %d of %d questions got a response (%d failed); not enough to "+
			"measure an invocation rate", wantTotal+notWantTotal, len(invocationCases), len(failed))
	}

	// The assertion is deliberately loose. This measures a model's judgement, which
	// varies by model and by prompt wording, so a tight threshold would turn a model
	// swap into a test failure and teach everyone to ignore it. Under half is not a
	// wording problem though — it means the description is not doing its job.
	if wantSearch := wantTotal; wantSearch > 0 && recall < 0.5 {
		t.Errorf("search_memory was invoked on only %.0f%% of memory-answerable questions. "+
			"The tool description is the retrieval trigger (§3.6); at this rate a working "+
			"index is going unqueried and the failure is invisible", 100*recall)
	}
	if notWantTotal > 0 && specificity < 0.25 {
		t.Errorf("search_memory fired on %d/%d questions that did not need it — that is "+
			"tokens and latency spent on general knowledge", falsePos, notWantTotal)
	}
}

// askOnce sends one question with the tool attached and reports whether the model
// chose to call it. It does not execute the call: the question here is invocation, and
// running the search would need a populated corpus and would change what is measured.
func askOnce(cli *api.Client, model string, tools api.Tools, question string) (bool, string, error) {
	systemPrompt, _ := loadSystemPrompt()
	msgs := []api.Message{}
	if systemPrompt != "" {
		msgs = append(msgs, api.Message{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, api.Message{Role: "user", Content: question})

	// Generous: the first request to a large model on a remote host pays a cold load,
	// which measured well past two minutes for a 27B. Overriding via env keeps a slow
	// host from being untestable.
	timeout := 300 * time.Second
	if v := os.Getenv("OLLAMA_TEST_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stream := false
	think := api.ThinkValue{Value: false}
	var reply strings.Builder
	var called bool
	err := cli.Chat(ctx, &api.ChatRequest{
		Model: model, Messages: msgs, Stream: &stream, Think: &think, Tools: tools,
	}, func(resp api.ChatResponse) error {
		reply.WriteString(resp.Message.Content)
		for _, tc := range resp.Message.ToolCalls {
			if tc.Function.Name == "search_memory" {
				called = true
			}
		}
		return nil
	})
	return called, reply.String(), err
}

// toolSpec converts a ToolDef into the api.Tool shape, mirroring what the chat loop
// does, so the measurement sees exactly the description the model normally sees.
func toolSpec(t *testing.T, def ToolDef) api.Tool {
	t.Helper()
	var params api.ToolFunctionParameters
	if err := json.Unmarshal(def.Parameters, &params); err != nil {
		t.Fatalf("tool %s parameters: %v", def.Name, err)
	}
	return api.Tool{
		Type: "function",
		Function: api.ToolFunction{
			Name: def.Name, Description: def.Description, Parameters: params,
		},
	}
}

// dialOllama returns a client, or skips when nothing answers.
func dialOllama(t *testing.T, addr, model string) (*api.Client, string) {
	t.Helper()
	if addr == "" {
		t.Skip("no Ollama endpoint configured")
	}
	u, err := url.Parse(addr)
	if err != nil {
		t.Skipf("bad Ollama endpoint %q: %v", addr, err)
	}
	// A real http.Client, not nil: api.NewClient stores whatever it is given and
	// dereferences it on the first request, so nil segfaults rather than defaulting.
	cli := api.NewClient(u, &http.Client{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cli.Heartbeat(ctx); err != nil {
		t.Skipf("Ollama unreachable at %s: %v — this measurement needs a real model", addr, err)
	}
	if model == "" {
		t.Skip("no model configured")
	}

	// A reachable endpoint is not enough — the configured model has to be pulled.
	// Skipping here rather than failing is the point: an endpoint that is up with a
	// different set of models is a normal state for a shared Ollama host, and turning
	// that into a red suite trains people to ignore it. (Learned the hard way: this
	// test failed the whole package because config.json names qwen3.5:9b and the host
	// has qwen3.5:latest.)
	//
	// OLLAMA_TEST_MODEL overrides, so the measurement can be pointed at whatever is
	// actually available without editing config.json.
	if v := os.Getenv("OLLAMA_TEST_MODEL"); v != "" {
		model = v
	}
	list, err := cli.List(ctx)
	if err != nil {
		t.Skipf("cannot list models at %s: %v", addr, err)
	}
	var names []string
	for _, m := range list.Models {
		if m.Name == model {
			return cli, model
		}
		names = append(names, m.Name)
	}
	sort.Strings(names)
	if len(names) > 6 {
		names = names[:6]
	}
	t.Skipf("model %q is not pulled on %s. Set OLLAMA_TEST_MODEL to one that is, e.g. %s",
		model, addr, strings.Join(names, ", "))
	return nil, ""
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	if s == "" {
		return "(empty reply)"
	}
	return s
}
