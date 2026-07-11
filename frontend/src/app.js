import './style.css';
import './app.css';
import { sendMessage, setModel, getModel, listModels, getCurrentSession, setMessagePinned } from './api';
import { listCotModes, getCotMode, setCotMode } from './api';
import { renderSessionList } from './sessions'
import { renderArtifactList } from './artifacts'
import { escapeHtml, renderMarkdown, renderHighlighted } from './content'
import { EventsOn } from '../wailsjs/runtime/runtime'



// --- State ---
// timeline holds every rendered message record, in order: {seq, role, content,
// model, mode, pinned, toolName, toolArgs, toolResult, el}. seq is null until
// the backend round-trip returns the persisted row (see doSend).
let timeline = [];
let currentModel = '';

// --- DOM refs ---
const textarea   = document.querySelector('.chat-textarea');
const sendBtn    = document.querySelector('.send-btn');
const modelSel   = document.querySelector('select.dd-btn');
const modeSel    = document.querySelector('select.mode-sel');
const statusText = document.querySelector('.status-left span:last-child'); // "Ready" text + dot
const statusDot  = document.querySelector('.status-dot');
const statusBarModel = document.querySelector('.status-right > span:first-child'); // shows current model name
const statusBarMode  = document.getElementById('statusMode'); // shows current cot mode

// --- Helpers ---
function getTimestamp() {
    const d = new Date();
    return d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
}

export function resetMessages() {
    timeline = [];
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
    const rec = { seq: null, model: '', mode: '', pinned: true, toolName: '', toolArgs: '', toolResult: '', ...entry };
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
async function doSend() {
    const text = textarea.value.trim();
    if (!text) return;

    // Snapshot context before rendering this turn's user bubble — the backend
    // appends `text` itself as the final turn, so history must not include it.
    const priorHistory = computeHistory();
    // Render optimistically for instant feedback; reconciled with its real
    // seq/model/mode once the round-trip below returns.
    const pendingUser = addMessage({ role: 'user', content: text, pinned: true });

    textarea.value     = '';
    textarea.style.height = 'auto';

    setStatus('Sending…', 'loading');

    try {
        const result = await sendMessage(text, priorHistory);
        const msgs = result?.messages || [];

        const userTurn = msgs.find(m => m.role === 'user');
        if (userTurn) {
            pendingUser.seq = userTurn.seq;
            pendingUser.model = userTurn.model;
            pendingUser.mode = userTurn.mode;
        }

        for (const m of msgs) {
            if (m.role === 'user') continue; // already rendered optimistically above
            addMessage(m);
        }

        if (!msgs.some(m => m.role === 'assistant')) {
            // Ollama may return empty for certain models — just note it.
            setStatus('Ready', 'ready');
        }
    } catch (err) {
        console.error(err);
        addMessage({ role: 'assistant', content: `<em style="color:var(--text-muted)">Error: ${escapeHtml(String(err))}</em>`, pinned: false });
    } finally {
        setStatus('Ready', 'ready');
        // Refresh sidebar so session message counts stay current.
        renderSessionList();
        // Refresh in case the assistant created any artifacts this turn.
        renderArtifactList();
    }
}

// --- Live status from the backend (chain-of-thought / tool execution) ---
const THINKING_LABELS = ['Thinking…', 'Cogitating…', 'Pondering…'];
EventsOn('chat:status', (payload) => {
    if (!payload) return;
    if (payload.state === 'thinking') {
        setStatus(THINKING_LABELS[Math.floor(Math.random() * THINKING_LABELS.length)], 'loading');
    } else if (payload.state === 'tool') {
        setStatus(`Executing ${payload.tool}…`, 'loading');
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
