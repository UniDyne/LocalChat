import { createSession, loadSessions, getCurrentSession, loadMessages, renameSession, switchSession, deleteSession } from './api';
import { escapeHtml } from './content'
import { addMessage, resetMessages } from './app'
import { renderArtifactList } from './artifacts'

// --- Session Management ---
let activeSessionId = null;

const sessionListEl = document.getElementById('sessionList');

async function initSessions() {
    try {
        activeSessionId = await getCurrentSession();
    } catch (e) { console.warn('could not get current session', e); }
    await renderSessionList();
    // Load messages for the active session into chat log.
    await loadSessionMessages(activeSessionId);
}


export async function renderSessionList() {
    if (!sessionListEl) return;
    try {
        const sessions = await loadSessions();
        sessionListEl.innerHTML = '';

        for (const s of sessions) {
            // Build a compact "New Chat" / + New Chat button that only appears when current session has messages.
            const div = document.createElement('div');
            div.className = 'session-item' + (s.id === activeSessionId ? ' active' : '');

            const title = s.title || 'New Chat';
            const dateStr = s.createdAt ? new Date(s.createdAt).toLocaleDateString() : '';
            div.innerHTML = `
                <div class="session-title">${escapeHtml(title)}</div>
                <div class="session-meta">${dateStr}${s.messageCount ? ' · ' + s.messageCount : ''}</div>
                <button class="session-delete-btn" title="Delete chat">&#128465;</button>`;

            // Click → switch session.
            div.addEventListener('click', async () => {
                try {
                    await doSwitchSession(s.id);
                } catch (e) { console.error(e); }
            });

            // Trash icon → confirm, then delete. Stop propagation so it doesn't also switch sessions.
            div.querySelector('.session-delete-btn').addEventListener('click', async (e) => {
                e.stopPropagation();
                if (!confirm(`Delete "${title}"? This cannot be undone.`)) return;
                try {
                    await doDeleteSession(s.id);
                } catch (err) { console.error('delete failed', err); }
            });

            // Right-click context: rename.
            div.addEventListener('contextmenu', async (e) => {
                e.preventDefault();
                const newTitle = prompt('New title:', title);
                if (newTitle) {
                    try {
                        await renameSessionInList(s.id, newTitle);
                    } catch (err) { console.error('rename failed', err); }
                }
            });

            sessionListEl.appendChild(div);
        }

    } catch (err) { console.error('renderSessionList', err); }
}

async function loadSessionMessages(sessionId) {
    if (!sessionId || !chatLog) return;
    chatLog.innerHTML = '';
    resetMessages()
    try {
        const msgs = await loadMessages(sessionId);
        for (const m of msgs) {
            addMessage(m.role, escapeHtml(m.content));
        }
    } catch (err) { console.error('loadSessionMessages', err); }
}


async function doSwitchSession(id) {
    if (!id || id === activeSessionId) return;
    try {
        await switchSession(id);
        activeSessionId = id;
        await loadSessionMessages(id);
        await renderSessionList(); // update highlight
        await renderArtifactList();
    } catch (err) { console.error('switch session', err); }
}

async function renameSessionInList(id, title) {
    await renameSession(id, title);
    await renderSessionList();
}

async function doDeleteSession(id) {
    await deleteSession(id);
    // If the deleted session was active, the backend auto-creates a replacement.
    activeSessionId = await getCurrentSession();
    chatLog.innerHTML = '';
    resetMessages();
    await renderSessionList();
    await loadSessionMessages(activeSessionId);
    await renderArtifactList();
}

// New session button handler (exposed to global for onclick)
window.doNewSession = async function () {
    try {
        const newId = await createSession();
        activeSessionId = newId;
        chatLog.innerHTML = '';
        resetMessages();
        await renderSessionList();
        await renderArtifactList();
    } catch (err) { console.error('new session', err); }
};

// Wire the "New Chat" button visibility: show it when current session has messages.
const observer = new MutationObserver(() => {
    const btn = document.querySelector('.new-session-btn');
    if (!btn) return;
    // Show only when there are messages in the current session.
    btn.style.display = chatLog.children.length > 0 ? '' : 'none';
});

// Observe chatLog for new messages to toggle button visibility.
observer.observe(chatLog, { childList: true });

initSessions();
