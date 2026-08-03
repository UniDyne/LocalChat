package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"simple-cot-chat/store"
)

func readConfigMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return m
}

func TestPersistConfigMergesAndPreservesUnmanagedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// A pre-existing file with keys the lightbox doesn't own.
	seed := `{
  "ollama_endpoint": "http://example:11434",
  "extract_model": "small",
  "old_ollama_endpoint": "http://legacy:11434"
}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &App{configPath: path, model: "qwen", mode: "deep", disabledTools: []string{"web_search"}}
	app.workDir = "/tmp/project"
	app.persistConfig()

	m := readConfigMap(t, path)
	// Managed keys written:
	if m["model"] != "qwen" || m["mode"] != "deep" || m["work_dir"] != "/tmp/project" {
		t.Errorf("managed keys wrong: %+v", m)
	}
	dt, _ := m["disabled_tools"].([]any)
	if len(dt) != 1 || dt[0] != "web_search" {
		t.Errorf("disabled_tools = %v", m["disabled_tools"])
	}
	// Unmanaged keys preserved untouched:
	if m["ollama_endpoint"] != "http://example:11434" {
		t.Errorf("ollama_endpoint clobbered: %v", m["ollama_endpoint"])
	}
	if m["extract_model"] != "small" {
		t.Errorf("extract_model clobbered: %v", m["extract_model"])
	}
	if m["old_ollama_endpoint"] != "http://legacy:11434" {
		t.Errorf("unknown key old_ollama_endpoint dropped: %v", m["old_ollama_endpoint"])
	}
}

func TestPersistConfigNilToolsWritesEmptyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	app := &App{configPath: path, model: "m"}
	app.persistConfig()
	m := readConfigMap(t, path)
	dt, ok := m["disabled_tools"].([]any)
	if !ok || len(dt) != 0 {
		t.Errorf("disabled_tools should be [], got %#v", m["disabled_tools"])
	}
}

func TestSetGlobalToolDefaultAddRemoveDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	app := &App{configPath: path}

	app.setGlobalToolDefault("web_search", false) // disable -> add
	app.setGlobalToolDefault("web_search", false) // again -> no dup
	if len(app.disabledTools) != 1 || app.disabledTools[0] != "web_search" {
		t.Fatalf("after disabling: %v", app.disabledTools)
	}
	app.setGlobalToolDefault("read_file", false)
	if len(app.disabledTools) != 2 {
		t.Fatalf("expected 2 disabled, got %v", app.disabledTools)
	}
	app.setGlobalToolDefault("web_search", true) // enable -> remove
	if len(app.disabledTools) != 1 || app.disabledTools[0] != "read_file" {
		t.Fatalf("after enabling web_search: %v", app.disabledTools)
	}
	// Persisted to disk too.
	m := readConfigMap(t, path)
	dt, _ := m["disabled_tools"].([]any)
	if len(dt) != 1 || dt[0] != "read_file" {
		t.Errorf("persisted disabled_tools = %v", m["disabled_tools"])
	}
}

func TestSeedSessionToolsAndSetSessionToolEnabled(t *testing.T) {
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	app := &App{sess: st, configPath: path, disabledTools: []string{"web_search", "read_file"}}

	// A newly created session inherits the global default disabled set.
	id := app.CreateSession()
	got, err := st.GetDisabledTools(id)
	if err != nil {
		t.Fatalf("GetDisabledTools: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("seeded disabled tools = %v, want 2", got)
	}

	// Toggling in the lightbox updates both this session and the global default.
	if err := app.SetSessionToolEnabled(id, "web_search", true); err != nil {
		t.Fatalf("SetSessionToolEnabled: %v", err)
	}
	sess, _ := st.GetDisabledTools(id)
	if len(sess) != 1 || sess[0] != "read_file" {
		t.Errorf("session disabled after enable = %v", sess)
	}
	// Global default also lost web_search.
	m := readConfigMap(t, path)
	dt, _ := m["disabled_tools"].([]any)
	if len(dt) != 1 || dt[0] != "read_file" {
		t.Errorf("global default after enable = %v", m["disabled_tools"])
	}
}
