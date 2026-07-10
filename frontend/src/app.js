import './style.css';
import './app.css';
import { sendMessage, setModel, getModel, listModels } from './api';
import { listCotModes, getCotMode, setCotMode } from './api';
import { renderSessionList } from './sessions'
import { renderArtifactList } from './artifacts'
import { escapeHtml, renderMarkdown } from './content'



// --- State ---
let history = []; // array of {role, content} for Ollama context
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
    history = [];
}

export function addMessage(role, content) {
    history.push({role, content});

    //let html = escapeHtml(content);

    const chatLog = document.getElementById('chatLog');
    if (!chatLog) return;

    const md = renderMarkdown(content);

    const div  = document.createElement('div');
    div.className = `message msg-${role}`;
    div.innerHTML = `
        <div class="msg-avatar">${role === 'user' ? 'U' : 'A'}</div>
        <div class="msg-body">
            <div class="msg-header"><span>${role === 'user' ? 'You' : 'Assistant'}</span><span class="msg-time">${getTimestamp()}</span></div>
            <div class="msg-text markdown-body">${md}</div>
        </div>`;
    chatLog.appendChild(div);
    chatLog.scrollTop = chatLog.scrollHeight;
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

    // Snapshot prior turns before addMessage appends this one — the backend
    // appends `text` itself as the final turn, so history must not include it.
    const priorHistory = [...history];
    addMessage('user', text);

    textarea.value     = '';
    textarea.style.height = 'auto';

    setStatus('Sending…', 'loading');

    try {
        const reply = await sendMessage(text, priorHistory);
        if (reply) {
            addMessage('assistant', reply);
        } else {
            // Ollama may return empty for certain models — just note it.
            setStatus('Ready', 'ready');
        }
    } catch (err) {
        console.error(err);
        addMessage('assistant', `<em style="color:var(--text-muted)">Error: ${escapeHtml(String(err))}</em>`);
    } finally {
        setStatus('Ready', 'ready');
        // Refresh sidebar so session message counts stay current.
        renderSessionList();
        // Refresh in case the assistant created any artifacts this turn.
        renderArtifactList();
    }
}


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
