import { getActiveSessionId } from './sessions';
import { getSessionToolStates, setSessionToolEnabled } from './api';

// Renders the tool toggle list into #settingsToolsList inside the settings
// lightbox. Called by app.js whenever the lightbox opens.
export async function renderToolsList() {
    const container = document.getElementById('settingsToolsList');
    if (!container) return;

    container.innerHTML = '<div class="tools-panel-empty">Loading…</div>';

    const sessionId = getActiveSessionId();
    let states;
    try {
        states = await getSessionToolStates(sessionId);
    } catch (err) {
        console.error('getSessionToolStates', err);
        container.innerHTML = '<div class="tools-panel-empty">Could not load tools</div>';
        return;
    }

    container.innerHTML = '';
    if (!states || states.length === 0) {
        container.innerHTML = '<div class="tools-panel-empty">No tools available</div>';
        return;
    }

    for (const t of states) {
        const label = document.createElement('label');
        label.className = 'tools-panel-item';

        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = t.enabled;

        const name = document.createElement('span');
        name.textContent = t.name;

        label.appendChild(cb);
        label.appendChild(name);
        container.appendChild(label);

        cb.addEventListener('change', async () => {
            const prev = !cb.checked;
            try {
                await setSessionToolEnabled(sessionId, t.name, cb.checked);
            } catch (err) {
                console.error('setSessionToolEnabled', err);
                cb.checked = prev;
            }
        });
    }
}
