import { getPlan, getCurrentSession } from './api';
import { escapeHtml } from './content';

// --- Plan checklist (right sidebar, "Plan" tab) ---

const planListEl = document.getElementById('planList');

const STATUS_ICON = { pending: '○', in_progress: '◐', completed: '●', failed: '✕' };

// Render the plan checklist for the currently active session, in its own
// tab (see index.html's sidebar-right tabs) — same empty-state treatment as
// the artifact list, rather than hiding the pane itself.
export async function renderPlanList() {
    if (!planListEl) return;
    try {
        const sessionId = await getCurrentSession();
        const steps = await getPlan(sessionId);

        if (!steps || steps.length === 0) {
            planListEl.innerHTML = `
                <div class="right-empty">Plan<br><br>
                <span style="font-size:10px;color:var(--text-muted)">Multi-step plans created during this chat will appear here</span></div>`;
            return;
        }

        planListEl.innerHTML = `<div class="plan-list-title">Plan</div>` + steps.map(s => `
            <div class="plan-item plan-${s.status}">
                <span class="plan-item-icon">${STATUS_ICON[s.status] || '○'}</span>
                <span class="plan-item-content">${escapeHtml(s.content)}</span>
            </div>`).join('');
    } catch (err) {
        console.error('renderPlanList', err);
    }
}

renderPlanList();
