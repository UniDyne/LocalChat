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
	"strings"
	"sync"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/ollama/ollama/api"
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
// file is re-read on every call (same reasoning as cotPrompt: it may be
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

// cotPrompt loads the markdown prompt for a custom (non-reserved) chain-of-thought mode.
func cotPrompt(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(cotDir(), name+".md"))
	if err != nil {
		return "", fmt.Errorf("read cot prompt %q: %w", name, err)
	}
	return string(data), nil
}

// ChatMessage represents a message in the conversation history.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SendChat sends the user message to Ollama with optional history and returns the assistant reply.
// Both the user message and the assistant reply are persisted to the current session in DuckDB.
//
// For a custom (file-based) cot mode, this runs two model calls: a hidden evaluation
// pass using the cot prompt as a system message, then a final answer pass that folds
// the evaluation back in as a hidden note. Only the final answer is shown to the user
// or persisted — the cot prompt and the evaluation are discarded once the request
// completes, so the visible conversation and its context stay small.
func (a *App) SendChat(userMsg string, history []ChatMessage) (string, error) {
	if a.cli == nil {
		return "", fmt.Errorf("Ollama client not initialized")
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

	msgs := baseMsgs
	if a.mode != CotModeNone && a.mode != CotModeBuiltIn && a.mode != "" {
		if prompt, err := cotPrompt(a.mode); err != nil {
			slog.Warn("failed to load cot prompt", "mode", a.mode, "error", err)
		} else {
			evalParts := append(append([]string{}, systemParts...), prompt)
			evalMsgs := append([]api.Message{{Role: "system", Content: strings.Join(evalParts, "\n\n")}}, baseMsgs...)
			evaluation, err := a.chatOnce(evalMsgs, api.ThinkValue{Value: false})
			if err != nil {
				slog.Warn("cot evaluation failed", "mode", a.mode, "error", err)
			} else {
				systemParts = append(systemParts, "Internal reasoning notes — do not reveal or repeat verbatim, use only to inform your final answer:\n"+evaluation)
			}
		}
	}

	if len(systemParts) > 0 {
		msgs = append([]api.Message{{Role: "system", Content: strings.Join(systemParts, "\n\n")}}, baseMsgs...)
	}

	think := api.ThinkValue{Value: a.mode == CotModeBuiltIn}
	fullReply, err := a.chatWithTools(msgs, think)
	if err != nil {
		return "", fmt.Errorf("chat request failed: %w", err)
	}

	// Persist only the user message and the final answer — the cot evaluation pass, if any, is discarded.
	_ = a.sess.SaveMessage(sessionID, "user", userMsg)
	_ = a.sess.SaveMessage(sessionID, "assistant", fullReply)

	// Title the session from the user's opening message on their first turn.
	if isFirstMessage {
		if err := a.sess.RenameSession(sessionID, titleFromMessage(userMsg, 40)); err != nil {
			slog.Warn("failed to auto-title session", "session", sessionID, "error", err)
		}
	}

	return fullReply, nil
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
func (a *App) chatOnce(msgs []api.Message, think api.ThinkValue) (string, error) {
	resp, err := a.chatRequestOnce(msgs, think, nil)
	return resp.Content, err
}

// chatRequestOnce issues a single non-streaming chat completion request,
// optionally with a tool registry attached, and returns the full response
// message (content plus any requested tool calls).
func (a *App) chatRequestOnce(msgs []api.Message, think api.ThinkValue, tools api.Tools) (api.Message, error) {
	stream := false
	req := &api.ChatRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   &stream,
		Think:    &think,
		Tools:    tools,
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
const maxToolIterations = 6

// chatWithTools runs the tool-calling loop for a chat turn: send the request
// with the tool registry attached, execute any requested tool calls, feed
// their results back as "tool" messages, and repeat until the model responds
// with no more tool calls (or maxToolIterations is hit). Tool-call turns are
// intentionally not persisted to the session store — only the final answer
// returned here is saved by the caller, matching the existing convention of
// discarding the cot hidden evaluation pass.
func (a *App) chatWithTools(msgs []api.Message, think api.ThinkValue) (string, error) {
	tools := toAPITools(a.toolRegistry())

	for i := 0; i < maxToolIterations; i++ {
		resp, err := a.chatRequestOnce(msgs, think, tools)
		if err != nil {
			return "", err
		}
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}

		msgs = append(msgs, resp)
		for _, tc := range resp.ToolCalls {
			result, herr := a.dispatchTool(tc)
			if herr != nil {
				slog.Warn("tool call failed", "tool", tc.Function.Name, "error", herr)
				result = fmt.Sprintf("error: %v", herr)
			} else {
				slog.Info("tool call", "tool", tc.Function.Name)
			}
			msgs = append(msgs, api.Message{Role: "tool", Content: result})
		}
	}

	return "", fmt.Errorf("tool-calling exceeded %d iterations", maxToolIterations)
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

// SaveMessage persists a single message for a given session (frontend-facing).
func (a *App) SaveMessage(sessionID, role, content string) error {
	return a.sess.SaveMessage(sessionID, role, content)
}
