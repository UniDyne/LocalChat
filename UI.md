# UI — LocalChat

## Overview

Compact, dark-themed chat interface modeled after a code-editor layout
(VS Code–inspired). The DOM structure lives in `frontend/index.html`; all
styling is in `frontend/src/app.css` (+ `markdown.css` for rendered
message content) and all interactivity in `frontend/src/*.js`. No UI
framework or component library — plain DOM APIs throughout.

## Color Theme

All colors are CSS custom properties on `:root` (`app.css`):

| Variable | Value | Usage |
|----------|-------|-------|
| `--bg-primary` | `#1e1e1e` | Main chat background |
| `--bg-secondary` | `#252526` | Sidebars, input area, topbar |
| `--bg-tertiary` | `#2d2d2d` | Inputs, code blocks, badges |
| `--bg-hover` / `--bg-active` | `#2a2d2e` / `#37373d` | Hover / active row states |
| `--border` | `#404040` | All dividers |
| `--text-primary` | `#cccccc` | Main text |
| `--text-secondary` | `#969696` | Secondary labels |
| `--text-muted` | `#858585` | Timestamps, meta text |
| `--accent` / `--accent-hover` | `#7b5ea7` / `#9678c6` | Active states, send button, status bar background |
| `--radius` | `3px` | Border radius everywhere |
| `--font-ui` / `--font-mono` | system UI stack / `ui-monospace` stack | UI chrome / code blocks |
| `--font-md` / `--font-md-title` | serif / sans-serif stack | Rendered markdown body / headings (`markdown.css`) |

Message-role colors aren't variables — they're translucent overlays on top
of the theme background: green-tinted for a user message, amber-tinted for
a task (see below), a subtle dark tint for the assistant.

## Layout Structure

```
┌────────────────────────────────────────────┐ topbar (35px)
│ ☰   LocalChat                            ☰ │
├──────┬───────────────────────────┬──────────┤
│      │                           │          │
│ LEFT │   chat-log (flex:1,       │  RIGHT   │
│ (0 / │   scrolls independently)  │  (0 /    │
│ 240px│                           │  260px   │
│ when │   plan-banner (if any)    │  when    │
│ open)│   input-area              │  open)   │
├──────┴───────────────────────────┴──────────┤ statusbar (24px)
└────────────────────────────────────────────┘
```

- **Top bar** (`.topbar`, 35px): hamburger buttons toggle each sidebar's
  `.expanded` class (width 0 → 240px left / 260px right, CSS transition).
- **Left sidebar** (`#sidebarLeft`): collapsed (`width:0`) by default. Only
  the **Sessions** tab is wired up — a Files tab and a Search tab exist in
  the markup as commented-out placeholders for future use, not rendered.
- **Main area** (`.main`): chat log, an optional plan progress banner,
  and the input area, stacked vertically.
- **Right sidebar** (`#sidebarRight`): collapsed by default, 260px when
  expanded. Tabbed like the left sidebar — **Artifacts** (`#artifactList`)
  and **Plan** (`#planList`), each its own `.sidebar-content` pane, switched
  with the same `switchTab` used for the left sidebar's tabs (scoped to
  whichever `<aside>` the clicked tab lives in, so the two panes' tabs don't
  interfere with each other).
- **Status bar** (`.statusbar`, 24px, accent-purple background): connection
  status with a live elapsed-time counter, current model, cot mode, and
  version.

## UI Elements in Detail

### Top Bar (`.topbar`)
Two `☰` buttons (left/right sidebar toggles), a separator, and the app
title.

### Left Sidebar — Sessions (`sessions.js`)
- **`+ New Chat`** button creates a session and switches to it.
- Each `.session-item` shows a title (auto-derived from the user's first
  message — see `SendChat`'s `isFirstMessage` handling in `app.go` — updated
  the moment the backend renames it, via the `session:renamed` live event,
  not just when the current turn finishes) and metadata (created date,
  message count). Click switches sessions; a hover-revealed 🗑 button
  deletes (with a confirm prompt); right-click renames (via a native
  `prompt()`).
- Switching sessions stops any running plan for the old session and
  reloads the chat log (and plan checklist) from the backend for the new one.

### Chat Area (`.chat-log`)
Each entry is a `.message` with a role-specific style:

- **`.msg-user`** — green-tinted, avatar `U`, header "You".
- **`.msg-user.msg-task`** — amber-tinted, avatar `T`, header "Task" instead
  of "You". Applied to a message auto-dispatched from an active plan
  (`rec.auto` while the app is running, or the persisted `toolName` field
  after a reload — see `manage_plan` in `README.md`/`ARCHITECTURE.md`) —
  visually distinct so it's clear at a glance which turns you actually typed.
- **`.msg-assistant`** — dark-tinted, avatar `A`, header "Assistant". Carries
  a small badge with the model/mode used, and (for the current session
  only — this isn't persisted) how long the reply took to generate.
- **`.msg-meta` (`.msg-cot` / `.msg-tool`)** — a collapsed, dashed-border bar
  for a hidden chain-of-thought note or a tool call, showing a label
  ("Chain of thought" / `🔧 <tool name>`) plus how long that step took.
  Clicking a cot bar expands it inline (rendered markdown); clicking a tool
  bar opens the **tool-call lightbox** — a two-pane overlay showing the
  call's arguments and result side by side (reuses the artifact preview
  overlay styling, widened). Both are unpinned by default (dimmed via
  `.msg-unpinned`, excluded from future context) but stay visible.
- **Pin/unpin** — every message has a hover-revealed 📌/📍 button
  (`.msg-pin-btn`) toggling whether it's included as context on future
  turns. Unpinned messages stay visible but dimmed (`opacity:.55`), never
  hidden.
- Rendered message bodies go through `content.js`: `marked` (+ `math-marked`
  for LaTeX) for markdown, `highlight.js` for code blocks, with a copy
  button on each code block.

### Plan Banner (`.plan-banner`)
Hidden by default; shown while `manage_plan` is auto-continuing
("Working on step N of M: …") or a single turn's own tool-calling loop is
mid-flight ("Executing `<tool>`…"). A **Stop** button halts the plan after
the current step. If a step fails 3 times in a row, the run pauses instead of
continuing or aborting the rest of the plan, and a **Resume** button appears
to pick it back up.

### Right Sidebar Tabs — Artifacts / Plan
Separate tabs (`plan.js` / `artifacts.js`), same pattern as the left
sidebar's tabs. **Plan** (`#planList`) shows each step with a status icon
(`○` pending, `◐` in progress, `●` completed, `✕` failed), backed by
`GetPlan` so it survives a reload; an empty-state message is shown instead
of an empty pane when the session has no plan yet, matching the Artifacts
tab's own empty state.

### Input Area (`.input-area`)
- **Toolbar**: a model `<select>` and a cot-mode `<select>` (see
  "CoT modes" in `README.md`), both backed by real backend calls — not
  placeholders.
- **Input row**: auto-resizing textarea (`.chat-textarea`, up to 150px) and
  a send button. Enter sends, Shift+Enter inserts a newline. Both are
  disabled while a queue is auto-running or a turn's tool loop is active.

### Right Sidebar — Artifacts (`artifacts.js`)
Lists artifacts created via the `create_artifact` tool for the current
session (`.artifact-item`, title + type badge + date), refreshed on the
`artifact:created` live event. Clicking one opens a preview overlay with
rendered content, a download button, and a close button.

### Status Bar
- **Left**: a status dot (green = ready, yellow pulsing = loading) plus a
  label (`Ready`, `Thinking…`, `Executing <tool>…`, etc.) and a live elapsed
  counter that ticks up every 200ms since the status last changed — e.g.
  `Executing create_artifact… 1.6s`.
- **Right**: current model name, cot mode (`⏱ <mode>`), app version.

## Live/Dynamic Behavior

| Feature | Implementation | Notes |
|---------|---------------|-------|
| Toggle left/right sidebar | `toggleLeft`/`toggleRight` (inline in `index.html`) | Toggles `.expanded`, CSS-transitioned width |
| Tab switching (left sidebar) | `switchTab` (inline) | Only the Sessions tab is currently populated |
| Send message → live turn rendering | `app.js` (`sendAndRender`, `renderIncomingMessage`) via `api.js` → `SendChat` | Cot notes and tool calls appear as they're produced (`chat:message` event), not all at once at the end |
| Status bar live text + elapsed counter | `app.js` (`setStatus`, `chat:status` event) | Ticks up client-side; resets whenever the status changes |
| Per-message generation time | `app.js` (`phaseStartedAt` bookkeeping) | Client-side only, not persisted — absent after a session reload |
| Auto-resize textarea | input listener in `index.html` | Caps at 150px |
| Task queue auto-continuation | `app.js` (`maybeAdvanceTaskQueue`, `stopTaskQueue`) | Cross-session-safe — a background turn's messages are dropped rather than rendered into whichever session is currently on screen |
| Session list + immediate rename | `sessions.js` (`renderSessionList`, `session:renamed` listener) | Title updates as soon as the backend sets it, not after the whole turn finishes |
| Pin/unpin a message | `app.js` (`wirePinButton`) → `SetMessagePinned` | Affects context on future turns only; never deletes |
| Artifact list + preview | `artifacts.js` | Refreshed on `artifact:created` |
| Tool-call lightbox | `app.js` (`openToolLightbox`) | Reuses the artifact preview overlay markup/styles |

## Future UI Considerations

- Streaming responses (progressive token-by-token rendering) — turns
  currently resolve as a batch per model call; cot/tool/assistant messages
  each appear as a whole once produced, not word-by-word.
- Files and Search tabs in the left sidebar (markup exists, commented out;
  no backing functionality).
- Dark/light theme toggle (the CSS variables make this mechanically easy,
  but only the dark palette exists today).
