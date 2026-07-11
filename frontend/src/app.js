import './style.css';
import './app.css';
import { sendMessage, setModel, getModel, listModels, getCurrentSession, setMessagePinned } from './api';
import { listCotModes, getCotMode, setCotMode } from './api';
import { renderSessionList, getActiveSessionId } from './sessions'
import { renderArtifactList } from './artifacts'
import { escapeHtml, renderMarkdown, renderHighlighted } from './content'
import { EventsOn } from '../wailsjs/runtime/runtime'



// --- State ---
// timeline holds every rendered message record, in order: {seq, role, content,
// model, mode, pinned, toolName, toolArgs, toolResult, el}. seq is null until
// the backend round-trip returns the persisted row (see doSend).
let timeline = [];
let currentModel = '';

// seqs already rendered into the timeline (via a live "chat:message" event or
// the final batch result — whichever arrives first), so the other one is a
// no-op instead of a duplicate bubble. Cleared alongside timeline.
let renderedSeqs = new Set();

// --- DOM refs ---
const textarea   = document.querySelector('.chat-textarea');
const sendBtn    = document.querySelector('.send-btn');
const modelSel   = document.querySelector('select.dd-btn');
const modeSel    = document.querySelector('select.mode-sel');
const statusText = document.querySelector('.status-left span:last-child'); // "Ready" text + dot
const statusDot  = document.querySelector('.status-dot');
const statusBarModel = document.querySelector('.status-right > span:first-child'); // shows current model name
const statusBarMode  = document.getElementById('statusMode'); // shows current cot mode
const queueBanner    = document.getElementById('queueBanner');
const queueStopBtn   = document.getElementById('queueStopBtn');

// --- Helpers ---
function getTimestamp() {
    const d = new Date();
    return d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
}

export function resetMessages() {
    timeline = [];
    renderedSeqs = new Set();
}

// Messages sent to the backend as context on the next turn: pinned
// user/assistant turns only. Derived fresh from `timeline` each time rather
// than maintained incrementally, so pin/unpin toggles can't drift out of
// sync with what's actually rendered.
export function computeHistory() {
    return timeline
        .filter(m => m.pinned && (m.role === 'user' || m.role === 'assistant'))
        .map(m => ({ role: m.role, content: m.content }));
}

function pinButtonHtml(pinned) {
    return `<button class="msg-pin-btn" title="${pinned ? 'Unpin (exclude from context)' : 'Pin (include in context)'}">${pinned ? '📌' : '📍'}</button>`;
}

// Wires a message's pin button: toggles pinned state via the backend and
// updates the icon/dimming in place, without removing the message.
function wirePinButton(div, rec) {
    const btn = div.querySelector('.msg-pin-btn');
    if (!btn) return;
    btn.addEventListener('click', async (e) => {
        e.stopPropagation();
        if (rec.seq === null) return; // not yet persisted — brief window for the optimistic user bubble
        const next = !rec.pinned;
        try {
            const sessionId = await getCurrentSession();
            await setMessagePinned(sessionId, rec.seq, next);
            rec.pinned = next;
            btn.textContent = rec.pinned ? '📌' : '📍';
            btn.title = rec.pinned ? 'Unpin (exclude from context)' : 'Pin (include in context)';
            div.classList.toggle('msg-unpinned', !rec.pinned);
        } catch (err) { console.error('pin toggle failed', err); }
    });
}

// Opens a split-pane lightbox showing a tool call's arguments and result,
// pretty-printed and syntax-highlighted where they parse as JSON.
function openToolLightbox(rec) {
    let argsPretty = rec.toolArgs || '';
    try { argsPretty = JSON.stringify(JSON.parse(rec.toolArgs), null, 2); } catch {}

    let resultPretty = rec.toolResult || '';
    let resultLang = 'plaintext';
    try { resultPretty = JSON.stringify(JSON.parse(rec.toolResult), null, 2); resultLang = 'json'; } catch {}

    const overlay = document.createElement('div');
    overlay.className = 'artifact-preview-overlay';
    overlay.innerHTML = `
        <div class="artifact-preview-panel tool-lightbox-panel">
            <div class="artifact-preview-header">
                <span class="artifact-preview-title">🔧 ${escapeHtml(rec.toolName || 'tool')}</span>
                <div><button class="artifact-close-btn">&times;</button></div>
            </div>
            <div class="tool-lightbox-split">
                <div class="tool-lightbox-pane">
                    <div class="tool-lightbox-pane-label">Arguments</div>
                    <pre class="artifact-preview-pre"><code>${renderHighlighted(argsPretty, 'json')}</code></pre>
                </div>
                <div class="tool-lightbox-pane">
                    <div class="tool-lightbox-pane-label">Result</div>
                    <pre class="artifact-preview-pre"><code>${renderHighlighted(resultPretty, resultLang)}</code></pre>
                </div>
            </div>
        </div>`;
    overlay.querySelector('.artifact-close-btn').addEventListener('click', () => overlay.remove());
    overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// Adds one message to the chat log and the in-memory timeline, and returns
// its record (so callers can reconcile it later, e.g. once a seq is known).
// entry: {seq=null, role, content, model='', mode='', pinned=true, toolName='', toolArgs='', toolResult=''}
export function addMessage(entry) {
    const rec = { seq: null, model: '', mode: '', pinned: true, toolName: '', toolArgs: '', toolResult: '', auto: false, ...entry };
    timeline.push(rec);

    const chatLog = document.getElementById('chatLog');
    if (!chatLog) return rec;

    const div = document.createElement('div');
    rec.el = div;

    if (rec.role === 'cot' || rec.role === 'tool') {
        // Collapsed bar for internal steps — chain-of-thought notes and tool
        // calls are unpinned by default (excluded from future context) but
        // stay visible and expandable.
        div.className = `message msg-meta msg-${rec.role}${rec.pinned ? '' : ' msg-unpinned'}`;
        const label = rec.role === 'cot' ? 'Chain of thought' : `🔧 ${escapeHtml(rec.toolName || 'tool')}`;
        div.innerHTML = `
            ${pinButtonHtml(rec.pinned)}
            <div class="msg-meta-summary">${label}</div>
            <div class="msg-meta-body" style="display:none"></div>`;
        const summary = div.querySelector('.msg-meta-summary');
        const body = div.querySelector('.msg-meta-body');
        summary.addEventListener('click', () => {
            if (rec.role === 'tool') { openToolLightbox(rec); return; }
            const isOpen = body.style.display !== 'none';
            if (isOpen) { body.style.display = 'none'; return; }
            body.innerHTML = `<div class="markdown-body">${renderMarkdown(rec.content)}</div>`;
            body.style.display = '';
        });
        wirePinButton(div, rec);
    } else {
        div.className = `message msg-${rec.role}${rec.pinned ? '' : ' msg-unpinned'}`;
        const md = renderMarkdown(rec.content);
        const metaBits = [rec.model, (rec.mode && rec.mode !== 'none') ? rec.mode : ''].filter(Boolean).join(' · ');
        div.innerHTML = `
            ${pinButtonHtml(rec.pinned)}
            <div class="msg-avatar">${rec.role === 'user' ? 'U' : 'A'}</div>
            <div class="msg-body">
                <div class="msg-header">
                    <span>${rec.role === 'user' ? 'You' : 'Assistant'}</span>
                    ${rec.auto ? '<span class="msg-auto-badge">auto</span>' : ''}
                    <span class="msg-time">${getTimestamp()}</span>
                    ${metaBits ? `<span class="msg-model-badge">${escapeHtml(metaBits)}</span>` : ''}
                </div>
                <div class="msg-text markdown-body">${md}</div>
            </div>`;
        wirePinButton(div, rec);
    }

    chatLog.appendChild(div);
    chatLog.scrollTop = chatLog.scrollHeight;
    return rec;
}

// Renders one persisted message, whether it arrived via the live
// "chat:message" event (while the backend is still mid-turn) or in the final
// batch returned by sendMessage — whichever gets there first wins, the other
// call is a no-op, so tool calls/notes show up as they happen rather than
// all at once at the end.
function renderIncomingMessage(m) {
    if (m.seq != null && renderedSeqs.has(m.seq)) return;
    if (m.seq != null) renderedSeqs.add(m.seq);

    if (m.role === 'user') {
        // The optimistic bubble is already on screen — reconcile it in place
        // instead of adding a duplicate.
        const pending = timeline.find(r => r.role === 'user' && r.seq === null);
        if (pending) {
            pending.seq = m.seq;
            pending.model = m.model;
            pending.mode = m.mode;
            return;
        }
    }
    addMessage(m);
}

// Live backend events: a message is pushed here as soon as it's persisted
// (see a.persist in app.go), and an artifact as soon as it's created — both
// well before the RPC for the whole turn returns.
EventsOn('chat:message', (payload) => {
    if (!payload?.message) return;
    if (payload.sessionId !== getActiveSessionId()) return;
    renderIncomingMessage(payload.message);
});
EventsOn('artifact:created', (payload) => {
    if (!payload) return;
    if (payload.sessionId !== getActiveSessionId()) return;
    renderArtifactList();
});

function setStatus(text, state) {
    // Update the status dot and label.
    if (statusDot) {
        statusDot.className = `status-dot ${state}`;
    }
    if (statusText) {
        statusText.innerHTML = `<span class="status-dot ${state}"></span>${text}`;
    }
}

// --- Send message ---
// Core send+render path shared by the manual send button and the
// auto-continuing task queue below. `auto` marks the rendered user bubble so
// queue-driven steps are visually distinguishable from hand-typed ones.
async function sendAndRender(text, { auto = false } = {}) {
    // Snapshot context before rendering this turn's user bubble — the backend
    // appends `text` itself as the final turn, so history must not include it.
    const priorHistory = computeHistory();
    // Render optimistically for instant feedback; reconciled in place (see
    // renderIncomingMessage) with its real seq/model/mode once either the live
    // "chat:message" event or the round-trip below delivers the persisted row.
    addMessage({ role: 'user', content: text, pinned: true, auto });

    setStatus('Sending…', 'loading');

    try {
        const result = await sendMessage(text, priorHistory);
        const msgs = result?.messages || [];

        // Each of these has very likely already been rendered live via the
        // "chat:message" event as the backend produced it — renderIncomingMessage
        // dedupes by seq, so this is just a fallback for anything that wasn't
        // (e.g. an event that arrived after this promise already resolved).
        for (const m of msgs) {
            renderIncomingMessage(m);
        }

        maybeAdvanceTaskQueue(msgs);

        if (!msgs.some(m => m.role === 'assistant')) {
            // Ollama may return empty for certain models — just note it.
            setStatus('Ready', 'ready');
        }
    } catch (err) {
        console.error(err);
        addMessage({ role: 'assistant', content: `<em style="color:var(--text-muted)">Error: ${escapeHtml(String(err))}</em>`, pinned: false });
        stopTaskQueue(); // a failure mid-run halts the queue rather than silently continuing
    } finally {
        setStatus('Ready', 'ready');
        // This turn's tool loop (if any) is done — clear it and recompute the
        // banner. If maybeAdvanceTaskQueue just started a queue step above,
        // queueRunning is already true again, so the banner stays up with the
        // queue's own text instead of flickering off.
        toolLoopActive = false;
        currentToolName = '';
        updateQueueBanner();
        // Refresh sidebar so session message counts stay current.
        renderSessionList();
        // Refresh in case the assistant created any artifacts this turn.
        renderArtifactList();
    }
}

async function doSend() {
    const text = textarea.value.trim();
    if (!text) return;

    textarea.value     = '';
    textarea.style.height = 'auto';

    await sendAndRender(text);
}

// --- Self-queued task loop ---
// The queue_tasks tool (tools_queue.go) is stateless server-side and ends the
// turn immediately once dispatched (see chatWithTools in app.go) — its result
// is just a human-facing confirmation, not the task list, since that result
// is also what would otherwise be fed back into the model's own context. The
// actual list comes from the tool call's own persisted arguments (toolArgs)
// instead. All the looping happens here — after any turn resolves, if it
// included a queue_tasks call, pull the list out and keep auto-sending the
// next task until it's empty.
const MAX_QUEUE_STEPS_TOTAL = 20; // cross-turn safety cap, independent of the tool's own per-call cap
const MAX_TASKS_PER_QUEUE_CALL = 20; // mirrors maxQueuedTasks in tools_queue.go
let taskQueue = [];
let queueTotal = 0;
let queueIndex = 0;
let queueStepsRun = 0;
let queueRunning = false;

// Separate from the task queue: whether the *current* turn's tool-calling
// loop (app.go's chatWithTools) is mid-flight. A single turn can dispatch
// several tool calls in a row before replying, and that should show the same
// banner/stop affordance as a multi-turn queue — not just silently update the
// status bar text.
let toolLoopActive = false;
let currentToolName = '';

function maybeAdvanceTaskQueue(msgs) {
    const call = msgs.find(m => m.role === 'tool' && m.toolName === 'queue_tasks');
    if (call) {
        // toolArgs is the model's own call arguments (e.g. {"tasks": [...]})
        // as persisted alongside the call — not toolResult, which is now just
        // a human-facing confirmation string (see tools_queue.go).
        let newTasks = [];
        try {
            const parsed = JSON.parse(call.toolArgs);
            newTasks = Array.isArray(parsed?.tasks) ? parsed.tasks : [];
        } catch { /* malformed — ignore, nothing to queue */ }
        // Mirror the backend's own validation: trim, drop blanks, cap per call.
        newTasks = newTasks
            .filter(t => typeof t === 'string' && t.trim() !== '')
            .map(t => t.trim())
            .slice(0, MAX_TASKS_PER_QUEUE_CALL);
        if (newTasks.length > 0) {
            // A queue_tasks call mid-run just extends the remaining queue —
            // handles the model re-queuing more steps as it goes, for free.
            taskQueue.push(...newTasks);
            queueTotal += newTasks.length;
            queueRunning = true;
        }
    }

    if (!queueRunning) return;

    if (taskQueue.length === 0 || queueStepsRun >= MAX_QUEUE_STEPS_TOTAL) {
        stopTaskQueue();
        return;
    }

    const next = taskQueue.shift();
    queueIndex++;
    queueStepsRun++;
    updateQueueBanner();
    // Deliberately not awaited — a detached continuation, not a bug. This lets
    // the current turn's finally block (sidebar refresh, status reset) settle
    // immediately instead of waiting on the entire remaining queue.
    sendAndRender(next, { auto: true });
}

export function stopTaskQueue() {
    taskQueue = [];
    queueRunning = false;
    queueIndex = 0;
    queueTotal = 0;
    // Stop can't actually cancel an in-flight backend request (no cancellation
    // channel exists yet) — same limitation the queue itself already has, it
    // only prevents further auto-continuation. Clearing this at least
    // dismisses the banner rather than leaving it stuck showing "tool loop"
    // once the user has asked to stop.
    toolLoopActive = false;
    updateQueueBanner();
}

function updateQueueBanner() {
    const busy = queueRunning || toolLoopActive;
    // Disabled while busy so a manually-typed message can't interleave with
    // auto-steps/tool calls and corrupt turn ordering — stop-then-type, not concurrent.
    textarea.disabled = busy;
    sendBtn.disabled = busy;
    if (!queueBanner) return;
    queueBanner.style.display = busy ? '' : 'none';
    if (!busy) return;
    const label = queueBanner.querySelector('.queue-banner-text');
    if (!label) return;
    label.textContent = queueRunning
        ? `Running step ${queueIndex} of ${queueTotal}…`
        : `Executing ${currentToolName || 'tool'}…`;
}

queueStopBtn?.addEventListener('click', stopTaskQueue);

// --- Live status from the backend (chain-of-thought / tool execution) ---
const THINKING_LABELS = ['Thinking…', 'Cogitating…', 'Pondering…'];
EventsOn('chat:status', (payload) => {
    if (!payload) return;
    if (payload.state === 'thinking') {
        setStatus(THINKING_LABELS[Math.floor(Math.random() * THINKING_LABELS.length)], 'loading');
    } else if (payload.state === 'tool') {
        setStatus(`Executing ${payload.tool}…`, 'loading');
        toolLoopActive = true;
        currentToolName = payload.tool;
        updateQueueBanner();
    }
});


// Event delegation: copy buttons are created inside renderMarkdown via innerHTML,
// so inline addEventListener is lost. Delegate from chatLog instead.
document.getElementById('chatLog').addEventListener('click', (e) => {
    const btn = e.target.closest('.code-copy-btn');
    if (!btn) return;

    // The sibling <pre><code> holds the raw source text
    const wrapper = btn.parentElement;
    const pre = wrapper.querySelector(':scope > pre code');
    if (pre) {
        navigator.clipboard.writeText(pre.textContent).then(() => {
            btn.textContent = 'copied!';
            setTimeout(() => (btn.textContent = 'copy'), 1500);
        });
    }
});

sendBtn.addEventListener('click', doSend);

textarea.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        doSend();
    }
});

// Auto-resize textarea.
textarea.addEventListener('input', function() {
    this.style.height = 'auto';
    this.style.height = Math.min(this.scrollHeight, 150) + 'px';
});

// --- Model selector ---
async function loadModels() {
    if (!modelSel) return;
    try {
        const models = await listModels();
        // Keep first option as placeholder, append real options.
        modelSel.innerHTML = '';
        for (const m of models) {
            const opt = document.createElement('option');
            opt.textContent = m;
            modelSel.appendChild(opt);
        }

        // Sync UI with the backend's active model.
        const active = await getModel();
        if (active && [...modelSel.options].some(o => o.value === active)) {
            currentModel = active;
            modelSel.value = active;
            updateStatusBarModel(active);
        } else if (models.length > 0) {
            // Default to first available.
            const chosen = models[0];
            await setModel(chosen);
            currentModel = chosen;
            modelSel.value = chosen;
            updateStatusBarModel(chosen);
        }
    } catch (err) {
        console.warn('Could not load Ollama models:', err.message);
        // Placeholder option so the selector isn't empty.
        modelSel.innerHTML = '<option disabled>— no Ollama —</option>';
    }
}

// Update the status bar to show the current model name.
function updateStatusBarModel(name) {
    if (statusBarModel) statusBarModel.textContent = name;
}

modelSel?.addEventListener('change', async () => {
    const chosen = modelSel.value;
    setStatus(`Switching to ${chosen}…`, 'loading');
    try {
        await setModel(chosen);
        currentModel = chosen;
        updateStatusBarModel(chosen);
        // Reset conversation history when switching models so the new context is clean.
        //resetMessages();
    } catch (err) {
        console.error('Failed to switch model:', err);
    } finally {
        setStatus('Ready', 'ready');
    }
});

// --- Mode selector (chain-of-thought) ---
async function loadCotModes() {
    if (!modeSel) return;
    try {
        const modes = await listCotModes();
        modeSel.innerHTML = '';
        for (const m of modes) {
            const opt = document.createElement('option');
            opt.textContent = m;
            modeSel.appendChild(opt);
        }

        // Sync UI with the backend's active mode.
        const active = await getCotMode();
        if (active && [...modeSel.options].some(o => o.value === active)) {
            modeSel.value = active;
            updateStatusBarMode(active);
        } else if (modes.length > 0) {
            // Default to first available (always "none").
            const chosen = modes[0];
            await setCotMode(chosen);
            modeSel.value = chosen;
            updateStatusBarMode(chosen);
        }
    } catch (err) {
        console.warn('Could not load CoT modes:', err.message);
        modeSel.innerHTML = '<option value="none">none</option>';
    }
}

// Update the status bar to show the current cot mode.
function updateStatusBarMode(name) {
    if (statusBarMode) statusBarMode.textContent = `⏱ ${name}`;
}

modeSel?.addEventListener('change', async () => {
    const chosen = modeSel.value;
    try {
        await setCotMode(chosen);
        updateStatusBarMode(chosen);
    } catch (err) {
        console.error('Failed to switch cot mode:', err);
    }
});

// Re-populate on startup and whenever the selector loses focus (covers reconnects).
loadModels();
loadCotModes();

// Clear demo data 
if (chatLog.querySelector('.message')) {
    chatLog.innerHTML = '';
}
