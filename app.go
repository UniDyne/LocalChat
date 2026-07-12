package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/ollama/ollama/api"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"simple-cot-chat/store"
)

// Reserved chain-of-thought mode names. All other modes are discovered from
// markdown files in the cot directory.
const (
	CotModeNone    = "none"
	CotModeBuiltIn = "built-in"
)

// config holds the Ollama endpoint and preferred model loaded from config.json.
type config struct {
	OllamaEndpoint string `json:"ollama_endpoint"`
	Model          string `json:"model"`
}

func loadConfig() *config {
	cfg := &config{
		OllamaEndpoint: "http://localhost:11434",
		Model:          "qwen3.5:9b",
	}

	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			path := filepath.Join(dir, "config.json")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				slog.Debug("config not found", "path", path, "error", readErr)
				continue
			}
			var loaded config
			if parseErr := json.Unmarshal(data, &loaded); parseErr != nil {
				slog.Warn("failed to parse config.json", "error", parseErr)
				continue
			}
			cfg = &loaded
			break
		}
	}

	return cfg
}

// App holds the Ollama client, active configuration, and DuckDB session store.
type App struct {
	ctx   context.Context
	cli   *api.Client
	addr  string
	model string
	mode  string
	sess  *store.Store
}

// NewApp creates a new App with defaults.
func NewApp() *App {
	cfg := loadConfig()
	return &App{addr: cfg.OllamaEndpoint, model: cfg.Model, mode: CotModeNone}
}

// startup is called when the app starts — saves context and initializes Ollama client + DB.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	addr := os.Getenv("OLLAMA_HOST")
	if addr == "" {
		addr = a.addr
	}

	u, err := url.Parse(addr)
	if err != nil {
		slog.Error("invalid Ollama address", "addr", addr, "error", err)
		return
	}

	hc := &http.Client{}
	a.cli = api.NewClient(u, hc)
	slog.Info("Ollama client ready", "addr", addr)

	ds, err := store.Open()
	if err != nil {
		slog.Error("db init failed", "error", err)
		return
	}
	a.sess = ds
	slog.Info("session store initialized")
}

// GetModel returns the currently selected model name.
func (a *App) GetModel() string {
	return a.model
}

// SetModel changes the active model for chat completions.
func (a *App) SetModel(name string) error {
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	a.model = name
	slog.Info("model changed", "model", a.model)
	return nil
}

// ListModels returns all locally available model names from Ollama.
func (a *App) ListModels() ([]string, error) {
	if a.cli == nil {
		return nil, fmt.Errorf("Ollama client not initialized")
	}

	resp, err := a.cli.List(a.ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	names := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// confDir returns the path to the "conf" directory (holding SYSTEM.md, cot/,
// and skills/), checked next to the executable and in the working directory
// (same convention as loadConfig).
func confDir() string {
	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			p := filepath.Join(dir, "conf")
			if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
				return p
			}
		}
	}
	return "./conf"
}

// cotDir returns the path to the "cot" directory under conf/.
func cotDir() string {
	return filepath.Join(confDir(), "cot")
}

// systemPromptPath returns the path to conf/SYSTEM.md.
func systemPromptPath() string {
	return filepath.Join(confDir(), "SYSTEM.md")
}

// loadSystemPrompt reads the configurable system prompt from conf/SYSTEM.md.
// A missing file is not an error — the system prompt is optional, and the
// file is re-read on every call (same reasoning as loadCotConfig: it may be
// hand-edited while the app is running).
func loadSystemPrompt() (string, error) {
	data, err := os.ReadFile(systemPromptPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ListCotModes returns all registered chain-of-thought modes: the reserved
// "none" and "built-in" options, plus one entry per markdown file found in
// the cot directory.
func (a *App) ListCotModes() ([]string, error) {
	modes := []string{CotModeNone, CotModeBuiltIn}

	entries, err := os.ReadDir(cotDir())
	if err != nil {
		if os.IsNotExist(err) {
			return modes, nil
		}
		return nil, fmt.Errorf("read cot dir: %w", err)
	}

	var custom []string
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".md" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if name == CotModeNone || name == CotModeBuiltIn {
			continue
		}
		custom = append(custom, name)
	}
	sort.Strings(custom)

	return append(modes, custom...), nil
}

// GetCotMode returns the currently active chain-of-thought mode.
func (a *App) GetCotMode() string {
	return a.mode
}

// SetCotMode changes the active chain-of-thought mode for subsequent chat requests.
func (a *App) SetCotMode(name string) error {
	if name == "" {
		return fmt.Errorf("mode name is empty")
	}
	a.mode = name
	slog.Info("cot mode changed", "mode", a.mode)
	return nil
}

// cotDefaultMaxTokens caps the hidden evaluation pass's response length when
// a cot mode's frontmatter omits max_tokens (or the file has no frontmatter
// at all). The eval prompt (cotEvalWrapper) already asks for a single tight
// pass, but some models still loop back and redo the analysis two or three
// times over ("wait, let me reconsider...") or need more room for a genuinely
// complex prompt — this cap is a backstop against the former, sized for the
// common case. Set max_tokens in a specific mode's frontmatter to raise it
// for that mode alone rather than raising the default for every mode.
const cotDefaultMaxTokens = 1024

// cotConfig is a custom (non-reserved) chain-of-thought mode's loaded prompt
// body plus its per-mode settings.
type cotConfig struct {
	Prompt    string
	MaxTokens int
}

// loadCotConfig loads the markdown prompt and frontmatter settings for a
// custom chain-of-thought mode. Frontmatter is optional — a file with none
// gets cotDefaultMaxTokens and its whole content as the prompt body.
func loadCotConfig(name string) (cotConfig, error) {
	data, err := os.ReadFile(filepath.Join(cotDir(), name+".md"))
	if err != nil {
		return cotConfig{}, fmt.Errorf("read cot prompt %q: %w", name, err)
	}

	fields, body := parseCotFrontmatter(string(data))
	cfg := cotConfig{Prompt: body, MaxTokens: cotDefaultMaxTokens}
	if v, ok := fields["max_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTokens = n
		} else {
			slog.Warn("invalid max_tokens in cot frontmatter, using default", "mode", name, "value", v, "default", cotDefaultMaxTokens)
		}
	}
	return cfg, nil
}

// parseCotFrontmatter splits a cot file into its frontmatter fields and body.
// Hand-rolled minimal parser, not a full YAML parser — mirrors
// skill.readFrontmatter (duplicated rather than imported, same reasoning as
// skill.Dir duplicating the cotDir/confDir convention): only flat single-line
// "key: value" pairs between a leading and closing "---" line are supported.
func parseCotFrontmatter(data string) (map[string]string, string) {
	lines := strings.Split(data, "\n")
	fields := map[string]string{}

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fields, data
	}

	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
		if idx := strings.Index(lines[i], ":"); idx > 0 {
			key := strings.TrimSpace(lines[i][:idx])
			val := strings.TrimSpace(lines[i][idx+1:])
			fields[key] = val
		}
	}

	if closeIdx == -1 {
		return map[string]string{}, data
	}

	body := strings.TrimLeft(strings.Join(lines[closeIdx+1:], "\n"), "\n")
	return fields, body
}

// cotEvalWrapper frames a custom cot prompt for the hidden evaluation pass.
// The prompt files themselves are written as reasoning frameworks (numbered
// steps, checklists) and vary in whether they explicitly tell the model not
// to answer yet — several don't. Without an explicit frame, the model reads
// the steps as instructions for the reply itself and answers the user right
// here, which defeats the point of a separate hidden pass. This wrapper is
// prepended/appended around every custom prompt so that framing never
// depends on how a given .md file happens to be worded.
const cotEvalWrapper = `You are in a hidden internal evaluation step, not the conversation itself. The user cannot see this step and has not yet received a reply. Do not write a final answer, greeting, or any text addressed to the user here — only produce internal analysis notes for the framework below. A separate pass afterward will use these notes to compose the actual reply.

Apply this framework to the user's message that follows:

---
%s
---

Produce your analysis now, in a single pass. Do not answer the user's question or request directly — stop at the analysis. Write it once and commit to it: do not restart, redo, or re-run the framework from the top to second-guess or correct an earlier step — if you notice a mistake, correct it in place and move on. Keep it tight — bullet points over prose, no repeating a point already made in an earlier step.`

// cotAnswerWrapper folds the hidden evaluation back in as part of the final
// user turn, rather than the system message. Instructions placed in a system
// message ahead of the whole conversation history compete with everything
// else for the model's attention ("lost in the middle"); folding the notes
// into the last user turn puts them exactly where the model attends most
// strongly, right before it starts generating the reply.
const cotAnswerWrapper = `%s

---
The section above is the prompt to answer. Below are your own hidden internal reasoning notes on it, produced in a prior step the user never saw — use them to inform your answer, but do not mention, quote, or refer to them ("notes", "analysis", etc.) in your reply. Just answer the prompt directly, as if this were the only step.

If the notes describe something that spans multiple distinct steps, or the full deliverable would be long (e.g. a multi-file app, a long document), do not attempt to fit the whole thing into this one reply — use queue_tasks to hand the remaining steps to yourself one at a time, and create_artifact for the actual deliverable content, per your operating instructions. Only write the full thing directly here if it's genuinely short.

%s`

// ChatMessage represents a message in the conversation history.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatTurnMessage is one message persisted during a single SendChat call,
// returned to the frontend so it can render (and let the user pin/unpin) it
// without a session reload.
type ChatTurnMessage struct {
	Seq        int    `json:"seq"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	Pinned     bool   `json:"pinned"`
	ToolName   string `json:"toolName,omitempty"`
	ToolArgs   string `json:"toolArgs,omitempty"`
	ToolResult string `json:"toolResult,omitempty"`
}

// ChatTurnResult is everything SendChat persisted for one turn, in
// conversation order (user, optional cot note, optional tool calls, assistant).
type ChatTurnResult struct {
	Messages []ChatTurnMessage `json:"messages"`
}

// SendChat sends the user message to Ollama with optional history and returns
// every message persisted this turn (user, optional cot note, optional tool
// calls, assistant).
//
// For a custom (file-based) cot mode, this runs two model calls: a hidden evaluation
// pass using the cot prompt as a system message, then a final answer pass that folds
// the evaluation back in as a hidden note. The cot note and any tool calls are
// persisted as their own unpinned message rows (visible collapsed in the UI, but
// excluded from context on future turns unless the user pins them).
func (a *App) SendChat(userMsg string, history []ChatMessage) (ChatTurnResult, error) {
	if a.cli == nil {
		return ChatTurnResult{}, fmt.Errorf("Ollama client not initialized")
	}

	sessionID := a.sess.CurrentSession()

	// Checked before this turn's messages are saved below, and independent of the
	// frontend-supplied history (which already includes the current user message).
	hadMessages, err := a.sess.HasMessages(sessionID)
	if err != nil {
		slog.Warn("failed to check session message count", "session", sessionID, "error", err)
	}
	isFirstMessage := err == nil && !hadMessages

	baseMsgs := make([]api.Message, 0, len(history)+1)
	for _, h := range history {
		baseMsgs = append(baseMsgs, api.Message{Role: h.Role, Content: h.Content})
	}
	baseMsgs = append(baseMsgs, api.Message{Role: "user", Content: userMsg})

	systemPrompt, err := loadSystemPrompt()
	if err != nil {
		slog.Warn("failed to load system prompt", "error", err)
	}

	// systemParts collects every piece of system-level context for this turn
	// (the configurable conf/SYSTEM.md prompt, then the cot hidden-evaluation
	// note if applicable) into a single system message — most chat templates
	// expect exactly one system message before history/user, not several, and
	// a trailing system message after the final user turn confuses the model
	// into truncating its reply.
	var systemParts []string
	if systemPrompt != "" {
		systemParts = append(systemParts, systemPrompt)
	}

	// turnMsgs accumulates every message row this turn persists, in
	// conversation order. Each message is saved to the store as soon as it's
	// produced (not batched until the turn succeeds) so that a turn cut short
	// by an error — e.g. exceeding maxToolIterations — still leaves everything
	// that happened so far in the session.
	turnMsgs := []ChatTurnMessage{a.persist(sessionID, store.NewMessage{Role: "user", Content: userMsg, Model: a.model, Mode: a.mode, Pinned: true})}

	// Title the session from the user's opening message as soon as it's in,
	// not after the model replies — a long tool-calling/queued-task chain, or
	// one that errors out (timeout, exceeding maxToolIterations), would
	// otherwise leave the session titled "New Chat" indefinitely.
	if isFirstMessage {
		title := titleFromMessage(userMsg, 40)
		if err := a.sess.RenameSession(sessionID, title); err != nil {
			slog.Warn("failed to auto-title session", "session", sessionID, "error", err)
		} else {
			wailsruntime.EventsEmit(a.ctx, "session:renamed", map[string]any{"sessionId": sessionID, "title": title})
		}
	}

	msgs := baseMsgs
	if a.mode != CotModeNone && a.mode != CotModeBuiltIn && a.mode != "" {
		if cot, err := loadCotConfig(a.mode); err != nil {
			slog.Warn("failed to load cot prompt", "mode", a.mode, "error", err)
		} else {
			evalParts := append(append([]string{}, systemParts...), fmt.Sprintf(cotEvalWrapper, cot.Prompt))
			evalMsgs := append([]api.Message{{Role: "system", Content: strings.Join(evalParts, "\n\n")}}, baseMsgs...)

			wailsruntime.EventsEmit(a.ctx, "chat:status", map[string]any{"state": "thinking"})
			evaluation, err := a.chatOnce(evalMsgs, api.ThinkValue{Value: false}, map[string]any{"num_predict": cot.MaxTokens})
			if err != nil {
				slog.Warn("cot evaluation failed", "mode", a.mode, "error", err)
			} else {
				turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{Role: "cot", Content: evaluation, Model: a.model, Mode: a.mode, Pinned: false}))

				// Fold the evaluation into the final user turn itself (not the
				// system message, and not the persisted/history copy of the
				// turn — baseMsgs and the stored user row above keep the plain
				// prompt so future turns don't replay the CoT note back at the
				// model as if the user had written it).
				augmented := fmt.Sprintf(cotAnswerWrapper, userMsg, evaluation)
				msgs = append(append([]api.Message{}, baseMsgs[:len(baseMsgs)-1]...), api.Message{Role: "user", Content: augmented})
			}
		}
	}

	if len(systemParts) > 0 {
		msgs = append([]api.Message{{Role: "system", Content: strings.Join(systemParts, "\n\n")}}, msgs...)
	}

	think := api.ThinkValue{Value: a.mode == CotModeBuiltIn}
	fullReply, toolTurnMsgs, err := a.chatWithTools(sessionID, msgs, think)
	turnMsgs = append(turnMsgs, toolTurnMsgs...)
	if err != nil {
		// Everything up to the failure (user message, cot note, dispatched tool
		// calls) has already been persisted via a.persist above/inside
		// chatWithTools, so the caller still gets a full record of the turn.
		return ChatTurnResult{Messages: turnMsgs}, fmt.Errorf("chat request failed: %w", err)
	}
	turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{Role: "assistant", Content: fullReply, Model: a.model, Mode: a.mode, Pinned: true}))

	return ChatTurnResult{Messages: turnMsgs}, nil
}

// titleFromMessage derives a short session title by collapsing a message to a single
// line and truncating it to maxLen characters on a word boundary.
func titleFromMessage(msg string, maxLen int) string {
	title := strings.Join(strings.Fields(msg), " ")
	if len(title) <= maxLen {
		return title
	}
	cut := title[:maxLen]
	if idx := strings.LastIndex(cut, " "); idx > 0 {
		cut = cut[:idx]
	}
	return strings.TrimSpace(cut) + "…"
}

// chatOnce issues a single non-streaming chat completion request (no tools
// attached) and returns the full reply text. Used for the cot hidden
// evaluation pass, which is a throwaway internal note that shouldn't be able
// to trigger skill/artifact tool calls.
func (a *App) chatOnce(msgs []api.Message, think api.ThinkValue, options map[string]any) (string, error) {
	resp, err := a.chatRequestOnce(msgs, think, nil, options)
	return resp.Content, err
}

// chatRequestOnce issues a single non-streaming chat completion request,
// optionally with a tool registry attached, and returns the full response
// message (content plus any requested tool calls).
func (a *App) chatRequestOnce(msgs []api.Message, think api.ThinkValue, tools api.Tools, options map[string]any) (api.Message, error) {
	stream := false
	req := &api.ChatRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   &stream,
		Think:    &think,
		Tools:    tools,
		Options:  options,
	}

	var full api.Message
	var mu sync.Mutex
	err := a.cli.Chat(a.ctx, req, func(resp api.ChatResponse) error {
		mu.Lock()
		defer mu.Unlock()
		full.Role = resp.Message.Role
		full.Content += resp.Message.Content
		if len(resp.Message.ToolCalls) > 0 {
			full.ToolCalls = append(full.ToolCalls, resp.Message.ToolCalls...)
		}
		return nil
	})
	return full, err
}

// maxToolIterations caps how many rounds of tool calls a single chat turn may
// make, guarding against a model that keeps calling tools indefinitely.
const maxToolIterations = 16

// chatWithTools runs the tool-calling loop for a chat turn: send the request
// with the tool registry attached, execute any requested tool calls, feed
// their results back as "tool" messages, and repeat until the model responds
// with no more tool calls (or maxToolIterations is hit). Each dispatched tool
// call is persisted immediately (role "tool", unpinned) and also returned as
// a ChatTurnMessage so the caller can report it to the frontend — even if the
// loop later errors out (e.g. the iteration cap is hit), everything dispatched
// so far is already in the session.
//
// queue_tasks is special-cased to end the turn immediately once dispatched,
// instead of looping back for another model round: its whole point is to
// hand off remaining work to the frontend's one-at-a-time continuation, and
// giving the model another generation afterward just invites it to go ahead
// and attempt those same tasks itself before the turn ends. Ending the turn
// right there removes that possibility structurally rather than relying on
// the model to respect the tool description's wording.
func (a *App) chatWithTools(sessionID string, msgs []api.Message, think api.ThinkValue) (string, []ChatTurnMessage, error) {
	tools := toAPITools(a.toolRegistry())
	var turnMsgs []ChatTurnMessage

	for i := 0; i < maxToolIterations; i++ {
		resp, err := a.chatRequestOnce(msgs, think, tools, nil)
		if err != nil {
			return "", turnMsgs, err
		}
		if len(resp.ToolCalls) == 0 {
			return resp.Content, turnMsgs, nil
		}

		msgs = append(msgs, resp)
		queuedReply := ""
		for _, tc := range resp.ToolCalls {
			wailsruntime.EventsEmit(a.ctx, "chat:status", map[string]any{"state": "tool", "tool": tc.Function.Name})

			result, herr := a.dispatchTool(tc)
			if herr != nil {
				slog.Warn("tool call failed", "tool", tc.Function.Name, "error", herr)
				result = fmt.Sprintf("error: %v", herr)
			} else {
				slog.Info("tool call", "tool", tc.Function.Name)
				if tc.Function.Name == "queue_tasks" {
					queuedReply = result
				}
			}
			msgs = append(msgs, api.Message{Role: "tool", Content: result})

			argsJSON, jerr := json.Marshal(tc.Function.Arguments)
			if jerr != nil {
				slog.Warn("failed to marshal tool arguments for persistence", "tool", tc.Function.Name, "error", jerr)
			}
			turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{
				Role: "tool", Content: tc.Function.Name, Model: a.model, Mode: a.mode, Pinned: false,
				ToolName: tc.Function.Name, ToolArgs: string(argsJSON), ToolResult: result,
			}))
		}

		if queuedReply != "" {
			return queuedReply, turnMsgs, nil
		}
	}

	return "", turnMsgs, fmt.Errorf("tool-calling exceeded %d iterations", maxToolIterations)
}

// persist saves a single message row immediately and returns it as a
// frontend-facing ChatTurnMessage. Called as each message is produced during
// a turn (user, cot note, tool calls, assistant) rather than batching
// everything until the turn completes, so a turn that errors out partway
// through still leaves what happened so far in the session. It also emits a
// "chat:message" event immediately, so the frontend can render tool calls and
// intermediate notes live as the tool-calling loop runs, instead of waiting
// for the whole (possibly multi-iteration) turn to finish.
func (a *App) persist(sessionID string, msg store.NewMessage) ChatTurnMessage {
	seq, err := a.sess.SaveMessage(sessionID, msg)
	if err != nil {
		slog.Warn("failed to persist message", "role", msg.Role, "session", sessionID, "error", err)
	}
	turnMsg := ChatTurnMessage{
		Seq: seq, Role: msg.Role, Content: msg.Content, Model: msg.Model, Mode: msg.Mode, Pinned: msg.Pinned,
		ToolName: msg.ToolName, ToolArgs: msg.ToolArgs, ToolResult: msg.ToolResult,
	}
	wailsruntime.EventsEmit(a.ctx, "chat:message", map[string]any{"sessionId": sessionID, "message": turnMsg})
	return turnMsg
}

// --- Session management (delegated to store.Store) ---

// CreateSession starts a new chat session and returns its ID.
func (a *App) CreateSession() string {
	id, _ := a.sess.CreateSession("New Chat")
	slog.Info("created session", "id", id)
	return id
}

// GetCurrentSession returns the ID of the active session.
func (a *App) GetCurrentSession() string {
	return a.sess.CurrentSession()
}

// GetSessions returns all sessions ordered newest first.
func (a *App) GetSessions() ([]store.Session, error) {
	return a.sess.GetSessions()
}

// GetMessages returns all messages for a given session.
func (a *App) GetMessages(sessionID string) ([]store.StoredMessage, error) {
	return a.sess.GetMessages(sessionID)
}

// SwitchSession changes the active session to an existing one.
func (a *App) SwitchSession(id string) error {
	err := a.sess.SwitchSession(id)
	if err == nil {
		slog.Info("switched session", "id", id)
	}
	return err
}

// DeleteSession removes a session and its messages from the database. If it was
// the current session, a fresh one is created to replace it.
func (a *App) DeleteSession(id string) error {
	err := a.sess.DeleteSession(id)
	if err == nil {
		slog.Info("deleted session", "id", id)
	}
	return err
}

// RenameSession updates the title of an existing session.
func (a *App) RenameSession(id string, title string) error {
	return a.sess.RenameSession(id, title)
}

// SaveMessage persists a single message for a given session (frontend-facing;
// not currently called by the frontend, kept as a manual affordance like
// CreateArtifactManual). Uses the currently active model/mode and pins it by
// default.
func (a *App) SaveMessage(sessionID, role, content string) error {
	_, err := a.sess.SaveMessage(sessionID, store.NewMessage{Role: role, Content: content, Model: a.model, Mode: a.mode, Pinned: true})
	return err
}

// SetMessagePinned toggles whether a message is included in the context sent
// to the model on future turns. The message stays visible and in the
// database either way — only its inclusion in context changes.
func (a *App) SetMessagePinned(sessionID string, seq int, pinned bool) error {
	return a.sess.SetMessagePinned(sessionID, seq, pinned)
}
