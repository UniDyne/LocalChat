import { getActiveSessionId } from './sessions';
import { getSessionToolStates, setSessionToolEnabled } from './api';

const btn = document.getElementById('toolsBtn');

btn?.addEventListener('click', async (e) => {
    e.stopPropagation();
    const existing = document.getElementById('toolsPanel');
    if (existing) { existing.remove(); return; }
    await showToolsPanel();
});

document.addEventListener('click', () => {
    document.getElementById('toolsPanel')?.remove();
});

async function showToolsPanel() {
    const sessionId = getActiveSessionId();
    let states;
    try {
        states = await getSessionToolStates(sessionId);
    } catch (err) {
        console.error('getSessionToolStates', err);
        return;
    }

    const panel = document.createElement('div');
    panel.id = 'toolsPanel';
    panel.className = 'tools-panel';
    panel.addEventListener('click', (e) => e.stopPropagation());

    if (!states || states.length === 0) {
        panel.innerHTML = '<div class="tools-panel-empty">No tools available</div>';
    } else {
        for (const t of states) {
            const label = document.createElement('label');
            label.className = 'tools-panel-item';

            const cb = document.createElement('input');
            cb.type = 'checkbox';
            cb.checked = t.enabled;

            const name = document.createElement('span');
            name.textContent = t.name.replace(/_/g, '​_'); // allow wrap at underscores

            label.appendChild(cb);
            label.appendChild(name);
            panel.appendChild(label);

            cb.addEventListener('change', async () => {
                const prev = !cb.checked;
                try {
                    await setSessionToolEnabled(sessionId, t.name, cb.checked);
                } catch (err) {
                    console.error('setSessionToolEnabled', err);
                    cb.checked = prev; // revert on error
                }
            });
        }
    }

    document.body.appendChild(panel);

    // Position below the button, left-aligned with it.
    const rect = btn.getBoundingClientRect();
    panel.style.top = (rect.bottom + 4) + 'px';
    panel.style.left = rect.left + 'px';
}
