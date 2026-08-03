# LocalChat: Directed CoT Harness

A local-first desktop chat client for [Ollama](https://ollama.com), built with
[Wails](https://wails.io) (Go backend + a plain HTML/CSS/JS frontend, no
framework). It looks like a fairly ordinary LLM chat app. Sessions, a model
picker, tool use, persisted artifacts... However, its reason for existing is a
different way of getting a local model to reason before it answers.


![Screenshot](docs/assets/screenshot.png)



## How is this different?

Many local models expose a native "thinking" mode (a `<think>` block the
model fills in before its real answer). It works, but you're stuck with
whatever reasoning style the model defaults to, and (especially on smaller
models) it's common for the model to draft (or even leak) its actual answer
inside the thinking block instead of using it to *plan* the answer.

LocalChat replaces that with a **directed chain-of-thought pass**: instead of
letting the model free-associate, you hand it an explicit reasoning framework
(a checklist, a set of questions, a persona) and force it through that
framework *before* it's allowed to answer. The framework is just a markdown
file, swappable per conversation, and easy to write or tune yourself.

Thinking normally persists in the chat history, bloating the context window.
Here, the CoT is discarded after the current turn. Also, queued tasks do not
use CoT or thinking at all since the path should already be well-defined. The
theory goes that the CoT persists "hologaphically" through the answer itself.

## How the CoT feature works

Each chat turn optionally runs through two model calls instead of one:

1. **Hidden evaluation pass.** The active CoT mode's markdown file (see
   `conf/cot/`) is wrapped in framing that makes its role explicit ("this is
   a hidden internal step, the user hasn't seen a reply yet, produce analysis
   only, don't answer the question yet") and sent as a system message ahead
   of the conversation history. This framing is applied uniformly by the app
   itself, so a mode file doesn't have to spell out "don't answer yet" on its
   own to be effective. The model's response to this call (the "chain of
   thought") is never shown to the user directly, but is persisted and
   visible as a collapsible note in the chat log.
2. **Final answer pass.** Rather than burying that reasoning in a system
   message ahead of the whole conversation, where it competes with
   everything else for the model's attention, it's folded into the *final
   user turn itself*, immediately before the model generates its reply:

   ```
   <original prompt>

   ---
   The section above is the prompt to answer. Below are your own hidden
   internal reasoning notes on it, produced in a prior step the user never
   saw — use them to inform your answer, but do not mention, quote, or refer
   to them in your reply. Just answer the prompt directly...

   <hidden reasoning notes>
   ```

   This puts the reasoning exactly where the model attends to it most
   strongly (right before it starts generating), instead of the start of a
   possibly-long system prompt. The conversation history stored for future
   turns keeps the plain original prompt. The augmented version is only
   used for this one generation call, so the model doesn't see its own past
   reasoning notes replayed back as if the user had written them.

Every CoT `.md` file can optionally set a `max_tokens` cap on the hidden pass
via frontmatter, defaulting to `1024` if omitted:

```markdown
---
max_tokens: 2048
---
You are a Senior Code Architect. Analyze the request. Do NOT write code yet...
```

This caps the evaluation pass's response length. That's useful because a model
will occasionally loop back and redo its own analysis two or three times
("wait, let me reconsider...") if left unbounded, which burns tokens for
no benefit. More complex reasoning frameworks can set their own limit higher
(see the included `claude-code.md` example).


### CoT modes

The mode selector in the toolbar lists:

- **`none`**: no extra reasoning pass, straight to the answer.
- **`built-in`**: defers to the model's own native thinking mode
  (Ollama's `think` request field), if it has one.
- Everything else: One entry per `.md` file in `conf/cot/`, named after the
  file. The repo ships with a variety of ready-made frameworks (debugging,
  math, research, business analysis, editing, code architecture, Socratic
  question-splitting, and more). Add your own by simply dropping in a new
  `.md` file, no code changes required. Each is picked up live; edits to
  an existing file take effect on the next turn without restarting the app.
  The CoT is per-prompt not per-session. You can switch it between prompts
  or turn it off entirely.

Queued follow-up tasks (see below) always run with CoT forced to `none`,
regardless of what's selected in the UI. They're instructions the model
already wrote for itself, not a fresh question, so there's nothing left to
evaluate. (Besides, it would confuse itself if left on.)

**NOTE:** The included `claude-*` CoT mode examples use steps that were written in
an attempt to emulate the thinking modes of Claude.

## Other features

- **Tool use.** The model has access to:
  - `manage_plan`: lay out multi-step work as an ordered, statused plan the
    app feeds back one step at a time (with the plan's current state), instead
    of trying to cram everything into a single reply. Tracked in a sidebar
    checklist.
  - `create_artifact` / `list_artifacts` / `get_artifact`: persist
    substantial content (documents, code, notes) outside the chat log,
    browsable in the artifacts sidebar. You can also import a local file as an
    artifact via the **Import** button in the Artifacts sidebar.
  - `search_skills` / `load_skill` / `read_skill_file` / `create_skill` /
    `update_skill`: a lightweight skill system under `conf/skills/` the model
    can discover by name/description, load on demand, and write new ones to
    when it works out something worth remembering for next time. A skill is
    either a plain `foo.md` file or a **rich** skill — a `foo/` directory with
    a `SKILL.md` entry point (same `name`/`description` frontmatter) that can
    bundle extra files; `load_skill` lists those files and `read_skill_file`
    fetches them on demand. `create_skill`/`update_skill` author both shapes:
    pass bundled `files` (inline content) and/or `artifact_files` (an existing
    artifact's content, by id) to write — or promote to — a rich skill.
  - `search_memory`: search your own material — Markdown notes you've indexed,
    earlier conversations, and saved artifacts. See **Memory** below.
  - `web_search`: query DuckDuckGo and get back ranked title/URL/snippet
    results. Results are cached in-process for 10 minutes.
  - `web_fetch`: retrieve any URL and return the page as clean Markdown via
    Mozilla Readability. Ideal for reading documentation, articles, and
    reference pages. Cached per URL for 30 minutes.
  - `list_files` / `read_file` / `write_file` / `update_file`: sandboxed
    read/write access to a directory you select. Only available after you
    choose a directory via the **Dir** control in the Settings panel.
- **Per-session tool toggles.** Enable or disable individual tools for the
  current session from the Settings panel (⚙). Disabled tools are excluded
  from the tool registry sent to the model on that session's turns.
- **Memory.** A local retrieval system over your own material, in the Memory
  tab of the left sidebar. Point it at a folder of Markdown notes (an Obsidian
  vault works well — it understands wikilinks, aliases, tags, frontmatter and
  daily notes) and it indexes them; your conversations and artifacts are added
  automatically as you go. Search combines keyword matching, semantic
  similarity, entity overlap and character n-grams, then follows links and
  related-note connections outward from the best matches, so it can surface a
  note that never mentions your search words.

  Everything runs on your machine. Semantic search needs a one-time 133 MB
  model download; without it the other three signals still work and the tab
  tells you what's missing. Re-scanning a folder is incremental — unchanged
  files aren't even opened — and notes you delete on disk are dropped from the
  index.

  The tab shows what's indexed, what the queue is doing, and for every search
  result the four signal scores behind its rank, so you can see *why*
  something ranked where it did and adjust the weights if you disagree.
- **Session persistence.** Every session and message (including hidden cot
  notes and tool calls) is stored in a local DuckDB file (`sessions.db`) next
  to the executable. Messages can be individually pinned/unpinned to control
  what's included as context on future turns.
- **Task provenance.** A message auto-dispatched from a queued task list is
  visually distinct ("Task") and persists that way. A reloaded session can
  still tell a queued step apart from something you actually typed.
- **Memory dies with its session.** Deleting a session deletes the memory
  derived from it — its conversation turns and its artifacts. Notes indexed
  from a folder are untouched: they have no session to belong to, and they're
  the primary corpus.
- **Live timing.** The status bar counts up while a request is in flight, and
  every generated message (cot note, tool call, reply) shows how long it took,
  tracked client-side, not persisted, so it's only visible for the current
  session.

## Project layout

```
LocalChat/
├── main.go                # entry point — Wails app setup, --db flag
├── app.go                 # App struct, config loading, SendChat turn orchestration
├── tools.go               # ToolDef type, availableTools(), toolRegistry()
├── tools_search.go        # web_search (DuckDuckGo scraper) and web_fetch (Readability)
├── tools_artifacts.go     # create_artifact / list_artifacts / get_artifact
├── tools_files.go         # list_files / read_file / write_file / update_file
├── tools_memory.go        # search_memory
├── tools_plan.go          # manage_plan
├── tools_skills.go        # search_skills / load_skill / read_skill_file / create_skill / update_skill
├── artifacts.go           # artifact RPC methods (including import)
├── skills.go              # skill RPC wrappers
├── store/                 # DuckDB-backed session/message/artifact/plan/tool store
├── memory/                # semantic memory subsystem (embeddings, BM25, entity graph)
├── skill/                 # skill file discovery (frontmatter parsing, CRUD)
├── conf/
│   ├── SYSTEM.md          # base system prompt
│   ├── cot/*.md           # one file per custom CoT mode
│   └── skills/            # skills: plain *.md files, or rich foo/SKILL.md bundles
└── frontend/
    ├── index.html         # UI shell
    └── src/
        ├── app.js         # send/receive loop, settings lightbox, status bar
        ├── api.js         # backend abstraction — all Wails binding calls
        ├── sessions.js    # session list, switching
        ├── artifacts.js   # artifacts sidebar, import button
        ├── tools.js       # tool toggle checkboxes (inside Settings panel)
        ├── plan.js        # plan tab checklist
        ├── memory.js      # memory tab UI
        └── content.js     # Markdown + math rendering, syntax highlighting, Mermaid
```

## Getting started

### Prerequisites

- [Go](https://go.dev) 1.26+
- [Node.js](https://nodejs.org) 18+ (for the Vite-built frontend)
- The [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation):
  ```
  go install github.com/wailsapp/wails/v2/cmd/wails@latest
  ```
- [Ollama](https://ollama.com) running and reachable, with at least one model
  pulled (`ollama pull <model>`)
- A C toolchain (`gcc`/`clang`). **cgo is mandatory** — the DuckDB driver
  static-links prebuilt DuckDB libraries, so `CGO_ENABLED=0` will not build.

Run `wails doctor` to check your platform's WebView/build dependencies are in
place.

#### On the DuckDB dependency

Persistence uses [`github.com/duckdb/duckdb-go/v2`](https://github.com/duckdb/duckdb-go)
(the canonical successor to `marcboeker/go-duckdb`, which is deprecated). Two
things to expect:

- **The first build is slow and large.** The driver pulls per-platform static
  libraries — a few hundred MB into the module cache — and linking them takes a
  while. Subsequent builds are normal. `go build` fetches everything from
  `go.mod`, so no explicit `go get` is needed for a fresh clone; if you're
  updating the dependency deliberately:
  ```
  go get github.com/duckdb/duckdb-go/v2
  ```
- **The database file's format is tied to the DuckDB version.** DuckDB reads
  older files forward, so an upgrade opens an existing `sessions.db` fine, but
  once the newer library has written to it the file won't reopen under an older
  one. If you're changing DuckDB versions, copy `sessions.db` aside first.

### CLI flags

| Flag | Description |
|------|-------------|
| `--db <path>` | Override the default `sessions.db` location |

Pass flags to the app under `wails dev` with `-appargs`:

```
wails dev -appargs "--db /path/to/custom.db"
```

### Configuration

On startup the app reads `config.json` in this order: the OS config
directory (e.g. `~/.config/localchat/`), the directory containing the
executable, then the current working directory. Falls back to built-in
defaults if not found.

```json
{
  "ollama_endpoint": "http://localhost:11434",
  "model": "qwen3.5:9b",
  "extract_model": "",
  "db_path": "",
  "search_engine": "ddgs",
  "searxng_endpoint": ""
}
```

| Key | Default | Description |
|-----|---------|-------------|
| `ollama_endpoint` | `http://localhost:11434` | Ollama server URL |
| `model` | `qwen3.5:9b` | Default chat model |
| `extract_model` | *(empty)* | Model for memory entity extraction; empty disables it |
| `db_path` | *(empty)* | Override the default `sessions.db` location |
| `search_engine` | `ddgs` | Web search backend: `ddgs` (DuckDuckGo) or `searxng` |
| `searxng_endpoint` | *(empty)* | SearXNG instance URL (required when `search_engine` is `searxng`) |

The Ollama endpoint can also be overridden with the `OLLAMA_HOST` environment
variable, which takes precedence over `config.json`.

#### Semantic search: the embedding model and ONNX Runtime

Memory's keyword, entity and character matching work out of the box. *Semantic*
search needs two things on disk, and the Memory tab tells you which one is
missing:

1. **The embedding model** (`bge-small-en-v1.5`, 133 MB). Click **Download the
   model** in the Memory tab. It's fetched from a pinned Hugging Face revision,
   verified against a known SHA-256, resumable if interrupted, and written to
   `~/.cache/localchat/models/bge-small-en-v1.5/`. Nothing is downloaded unless
   you ask — no outbound request happens on launch. Notes you already indexed
   are embedded automatically afterwards, with no re-indexing.

   To place it by hand instead, put `model.onnx` and `tokenizer.json` in that
   directory, or point `LOCALCHAT_MODEL_DIR` at wherever you keep them.

2. **ONNX Runtime ≥ 1.21** (`libonnxruntime.so` / `.dylib` / `onnxruntime.dll`),
   searched in this order: `$ONNXRUNTIME_LIB`, then `lib/` beside the binary,
   then `~/.cache/localchat/lib/`, then the system library paths.

   On Debian 13 the distro package is enough:
   ```
   sudo apt install libonnxruntime1.21
   ```
   Anything newer also works — a newer runtime happily serves the older API
   version this build requests. If your platform has no package, download a
   release from
   [onnxruntime](https://github.com/microsoft/onnxruntime/releases) and drop the
   shared library into `~/.cache/localchat/lib/`.

   The 1.21 floor is set by the Go binding, which compiles in a required API
   version and cannot ask for less. If it's ever raised, semantic search stops
   working on distro packages — `TestORTAPIVersionMatchesPinnedBinding` guards
   against that happening by accident.

   One cosmetic wart with Debian's package specifically: it links the system
   `libonnx`, so the ONNX operator schemas get registered twice and the library
   prints ~570 `Schema error: Trying to register schema ... already registered`
   lines to stderr the first time it loads. Harmless — embeddings are correct and
   byte-for-byte comparable with other builds — but noisy. Upstream's
   self-contained build doesn't do it, if the noise bothers you more than the
   extra download does.

`extract_model` selects the model used by the memory subsystem's optional entity
extraction pass, which reads each ingested note once and names the people,
organisations, paths and identifiers in it.

**Empty is the default and the recommendation.** Memory works without it, using
pattern-based extraction; the LLM pass is built and tested but its measured
contribution to retrieval so far is one query in fifty, which is inside the
noise. It also costs one model call per note, so on a large vault it is hours of
background work. Turn it on only if you want to experiment — and if you do, use a
small model rather than the chat model, since extraction is a much easier task
than reasoning.

### Running in development

```
wails dev
```

This starts the app with hot-reload for frontend changes (via Vite) and also
serves a dev endpoint at `http://localhost:34115` if you want to open the UI
in a regular browser with devtools access to the Go bindings.

### Building a release binary

```
wails build
```

Produces a native, self-contained binary under `build/bin/`, with the built
frontend embedded directly into it.

### Manual frontend-only build

If you just need to rebuild `frontend/dist` (e.g. before invoking `go build`
directly rather than through `wails build`):

```
cd frontend
npm install
npm run build
```
