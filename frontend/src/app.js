import './style.css';
import './app.css';
import { sendMessage, setModel, getModel, listModels, getCurrentSession, setMessagePinned } from './api';
import { listCotModes, getCotMode, setCotMode } from './api';
import { selectDirectory, clearDirectory, getWorkDir } from './api';
import { renderSessionList, getActiveSessionId } from './sessions'
import { renderArtifactList } from './artifacts'
import { renderPlanList } from './plan'
import { escapeHtml, renderMarkdown, renderHighlighted, initMermaidIn } from './content'
import { EventsOn } from '../wailsjs/runtime/runtime'
import { renderToolsList } from './tools'
import { openToolLightbox } from './tool-views'



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

// phaseStartedAt marks the start of whatever the model/tool is currently
// generating, purely client-side (the backend doesn't send timing data).
// It's reset at three points: when a turn begins, when a "chat:status" event
// announces the start of a hidden cot pass or a tool dispatch, and right
// after each cot/tool message is rendered — so the gap between two
// consecutive resets is that step's own generation/execution time. See
// renderIncomingMessage and the chat:status handler below. Deliberately NOT
// used for the final assistant message (see turnStartedAt) — a turn that
// goes through several tool rounds resets this on every one of them, so by
// the time the assistant's reply arrives it only reflects the last round's
// duration, not the whole turn's.
let phaseStartedAt = Date.now();

// turnStartedAt marks when the current turn began and, unlike phaseStartedAt,
// is never reset mid-turn — it's what the final assistant message's elapsed
// badge is measured against, so a multi-round tool-calling turn shows the
// model's true end-to-end time rather than just the gap since the last tool
// call finished.
let turnStartedAt = Date.now();

// --- DOM refs ---
const textarea   = document.querySelector('.chat-textarea');
const sendBtn    = document.querySelector('.send-btn');
const modelSel   = document.getElementById('modelSel');
const modeSel    = document.getElementById('modeSel');
const dirBtn      = document.getElementById('dirBtn');
const dirClearBtn = document.getElementById('dirClearBtn');
const statusText = document.querySelector('.status-left span:last-child'); // "Ready" text + dot
const statusDot  = document.querySelector('.status-dot');
const statusBarModel = document.querySelector('.status-right > span:first-child'); // shows current model name
const statusBarMode  = document.getElementById('statusMode'); // shows current cot mode
const planBanner     = document.getElementById('planBanner');
const planStopBtn    = document.getElementById('planStopBtn');
const planResumeBtn  = document.getElementById('planResumeBtn');
const cogBtn          = document.getElementById('cogBtn');
const settingsOverlay = document.getElementById('settingsOverlay');
const settingsCloseBtn = document.getElementById('settingsCloseBtn');
const toolbarSummary  = document.getElementById('toolbarSummary');

// Tracks current model/mode locally so the toolbar summary stays current
// without requiring a second round-trip to the backend.
let _summaryModel = '';
let _summaryMode  = '';

function updateToolbarSummary() {
    if (!toolbarSummary) return;
    const parts = [_summaryModel, (_summaryMode && _summaryMode !== 'none') ? _summaryMode : ''].filter(Boolean);
    toolbarSummary.textContent = parts.join(' · ');
}

cogBtn?.addEventListener('click', () => {
    if (!settingsOverlay) return;
    settingsOverlay.style.display = '';
    renderToolsList();
});

settingsCloseBtn?.addEventListener('click', () => {
    if (settingsOverlay) settingsOverlay.style.display = 'none';
});

settingsOverlay?.addEventListener('click', (e) => {
    if (e.target === settingsOverlay) settingsOverlay.style.display = 'none';
});

// --- Helpers ---
function formatMessageTime(time) {
    const d = time ? new Date(time) : new Date();
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
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
        const elapsed = rec.elapsedMs != null ? ` <span class="msg-elapsed">${formatElapsed(rec.elapsedMs)}</span>` : '';
        div.innerHTML = `
            ${pinButtonHtml(rec.pinned)}
            <div class="msg-meta-summary">${label}${elapsed}</div>
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
        // rec.auto covers the live/optimistic render for this session; rec.toolName
        // covers a message reloaded from the DB (auto defaults to false there, but
        // the persisted tool_name — see SendChat's queuedByTool in app.go — survives).
        const isTask = rec.role === 'user' && (rec.auto || !!rec.toolName);
        div.className = `message msg-${rec.role}${isTask ? ' msg-task' : ''}${rec.pinned ? '' : ' msg-unpinned'}`;
        const md = renderMarkdown(rec.content);
        const metaBits = [rec.model, (rec.mode && rec.mode !== 'none') ? rec.mode : ''].filter(Boolean).join(' · ');
        const avatarLetter = isTask ? 'T' : (rec.role === 'user' ? 'U' : 'A');
        const headerLabel = isTask ? 'Task' : (rec.role === 'user' ? 'You' : 'Assistant');
        // elapsedMs is only ever set on generated (assistant) messages — a
        // user turn is typed, not timed. Absent entirely on messages reloaded
        // from a past session, since it's tracked client-side only.
        const elapsedBadge = rec.elapsedMs != null ? `<span class="msg-model-badge">${formatElapsed(rec.elapsedMs)}</span>` : '';
        div.innerHTML = `
            ${pinButtonHtml(rec.pinned)}
            <div class="msg-avatar">${avatarLetter}</div>
            <div class="msg-body">
                <div class="msg-header">
                    <span>${headerLabel}</span>
                    <span class="msg-time">${formatMessageTime(rec.time)}</span>
                    ${metaBits ? `<span class="msg-model-badge">${escapeHtml(metaBits)}</span>` : ''}
                    ${elapsedBadge}
                </div>
                <div class="msg-text markdown-body">${md}</div>
            </div>`;
        wirePinButton(div, rec);
    }

    chatLog.appendChild(div);
    initMermaidIn(div);
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
    } else if (m.role === 'assistant') {
        // The final reply's badge is the whole turn's time (see
        // turnStartedAt above), not just the gap since the last phase
        // boundary — a turn that went through several tool rounds would
        // otherwise only show the last round's duration.
        m.elapsedMs = Date.now() - turnStartedAt;
    } else {
        // cot/tool: this message is whatever was generated/executed since
        // the last phase boundary (turn start, a chat:status event, or the
        // previous such message) — see phaseStartedAt above.
        const now = Date.now();
        m.elapsedMs = now - phaseStartedAt;
        phaseStartedAt = now;
    }
    addMessage(m);
}

// Live backend events: a message is pushed here as soon as it's persisted
// (see a.persist in app.go), and an artifact/plan update as soon as it
// happens — both well before the RPC for the whole turn returns.
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
EventsOn('plan:updated', (payload) => {
    if (!payload) return;
    if (payload.sessionId !== getActiveSessionId()) return;
    renderPlanList();
});

// formatElapsed renders a millisecond duration the way both the status bar's
// live timer and each message's generation-time badge want it: sub-second
// readings are precise enough to feel live, longer ones round to whole
// seconds so they don't jitter on every tick.
function formatElapsed(ms) {
    if (ms < 1000) return `${Math.round(ms)}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
}

// statusChangedAt marks when the current status text/state was set; the
// interval below re-renders the elapsed time next to it every tick so the
// status bar visibly counts up ("Executing tool… 3.2s") instead of just
// sitting on a static label the whole time an operation is in flight.
let statusChangedAt = Date.now();
setInterval(() => {
    const el = document.querySelector('.status-elapsed');
    if (el) el.textContent = formatElapsed(Date.now() - statusChangedAt);
}, 200);

function setStatus(text, state) {
    statusChangedAt = Date.now();
    // Update the status dot and label.
    if (statusDot) {
        statusDot.className = `status-dot ${state}`;
    }
    if (statusText) {
        const elapsed = state !== 'ready' ? ' <span class="status-elapsed">0ms</span>' : '';
        statusText.innerHTML = `<span class="status-dot ${state}"></span>${text}${elapsed}`;
    }
}

// --- Send message ---
// Core send+render path shared by the manual send button and the
// auto-continuing plan runner below. `auto` marks the rendered user bubble so
// plan-driven steps are visually distinguishable from hand-typed ones, and
// is also sent to the backend as queuedByTool so it's persisted on the row
// (see SendChat's doc in app.go) — that's what lets a reloaded session still
// tell a plan-driven step apart from one the user actually typed. `text` is
// already the fully-composed prompt by the time it gets here — for an auto
// turn that includes the injected plan state (see planPromptFor) — this
// function itself doesn't know or care whether a plan is involved.
//
// `displayText`, when given, is shown/persisted as the turn's user message
// instead of `text` — used for a plan-driven turn, where `text` is the full
// prompt the model needs (plan state + instructions) but showing all of that
// in the chat log would just repeat the Plan tab's own checklist. Omitted
// for a hand-typed turn, where `text` itself is already what should be shown.
async function sendAndRender(text, { auto = false, displayText = '' } = {}) {
    const queuedByTool = auto ? 'manage_plan' : '';
    // The session this turn belongs to — captured now, not re-read after the
    // round-trip below, since the user can switch sessions while a multi-step
    // (cot/tool) turn is still running. The live "chat:message" handler
    // already guards against rendering a backgrounded turn's messages into
    // whatever session is currently on screen; this fallback loop needs the
    // same guard, or switching away (and possibly back) mid-turn renders
    // messages into the wrong session and/or duplicates them once
    // resetMessages() (called on every session switch) clears the "already
    // rendered" set those messages were already recorded in.
    const turnSessionId = getActiveSessionId();
    // Turn start: the first generated message's elapsed time is measured
    // from here, unless a chat:status event resets it first. turnStartedAt
    // marks the same instant but, unlike phaseStartedAt, stays fixed for the
    // rest of the turn — it's what the final assistant reply's badge uses.
    phaseStartedAt = Date.now();
    turnStartedAt = phaseStartedAt;
    // Snapshot context before rendering this turn's user bubble — the backend
    // appends `text` itself as the final turn, so history must not include it.
    const priorHistory = computeHistory();
    // Render optimistically for instant feedback; reconciled in place (see
    // renderIncomingMessage) with its real seq/model/mode once either the live
    // "chat:message" event or the round-trip below delivers the persisted row.
    addMessage({ role: 'user', content: displayText || text, pinned: true, auto, toolName: queuedByTool });

    setStatus('Sending…', 'loading');

    try {
        const result = await sendMessage(text, priorHistory, queuedByTool, displayText);
        const msgs = result?.messages || [];

        if (turnSessionId === getActiveSessionId()) {
            // Each of these has very likely already been rendered live via the
            // "chat:message" event as the backend produced it — renderIncomingMessage
            // dedupes by seq, so this is just a fallback for anything that wasn't
            // (e.g. an event that arrived after this promise already resolved).
            for (const m of msgs) {
                renderIncomingMessage(m);
            }

            advancePlan(msgs);
        }

        if (!msgs.some(m => m.role === 'assistant')) {
            // Ollama may return empty for certain models — just note it.
            setStatus('Ready', 'ready');
        }
    } catch (err) {
        console.error(err);
        addMessage({ role: 'assistant', content: `<em style="color:var(--text-muted)">Error: ${escapeHtml(String(err))}</em>`, pinned: false });
        // A turn-level error (network/API failure) means we can't know what
        // the model actually did — unlike a step explicitly reported as
        // "failed" via manage_plan, this isn't something the consecutive-
        // failure breaker can reason about, so just halt the run outright.
        stopPlanRun();
    } finally {
        setStatus('Ready', 'ready');
        // This turn's tool loop (if any) is done — clear it and recompute the
        // banner. If advancePlan just started the next step above, planRunning
        // is already true again, so the banner stays up with the plan's own
        // text instead of flickering off.
        toolLoopActive = false;
        currentToolName = '';
        updatePlanBanner();
        // Refresh sidebar so session message counts stay current.
        renderSessionList();
        // Refresh in case the assistant created any artifacts this turn.
        renderArtifactList();
        // Refresh in case the assistant created/updated the plan this turn.
        renderPlanList();
    }
}

async function doSend() {
    const text = textarea.value.trim();
    if (!text) return;

    textarea.value     = '';
    textarea.style.height = 'auto';

    await sendAndRender(text);
}

// --- Plan runner ---
// The manage_plan tool (tools_plan.go) ends the turn immediately once
// dispatched (see chatWithTools in app.go), full-replace: every call carries
// the complete ordered plan, not a delta. All the looping happens here —
// after any turn resolves, if it included a manage_plan call, read the plan
// out of the call's own persisted arguments (toolArgs — not toolResult,
// which is just a human-facing echo) and keep auto-sending the current step
// until the plan is empty of pending/in_progress steps.
//
// A model can also reply to an auto-advanced step with plain prose and no
// tool call at all (e.g. "OK, I'll work on step 2 and..."), which is not
// progress — nothing was reported, so the plan would otherwise just stall
// silently forever with no further turns firing. advancePlan treats that the
// same as an explicit failure: retry (with an escalating reminder) up to
// MAX_CONSECUTIVE_PLAN_FAILURES times before pausing, rather than treating a
// tool-call-free reply as if the loop were simply done.
const MAX_CONSECUTIVE_PLAN_FAILURES = 3; // pause-and-surface, not abandon-the-plan

// A stronger reminder appended to the step prompt on retry after the model
// replied without calling manage_plan at all (e.g. "OK, I'll work on step
// 2..." with no tool call) — see planNoToolCallCount below. Plain prose is
// not progress: without this, the plan would silently stop advancing since
// nothing here would ever notice and re-prompt.
const PLAN_NO_PROGRESS_NUDGE = `

Reminder: your last reply didn't call manage_plan, so nothing was recorded — no step can be considered done until you do. Don't just describe what you're about to do; actually do the work, then call manage_plan (with the full plan) to report this step's outcome before ending your turn.`;

let planRunning = false;
// Distinguishes "stopped" from "paused after repeated failures" so the
// banner can offer Resume instead of just Stop.
let planPaused = false;
let planConsecutiveFailures = 0;
// Consecutive auto-advanced turns where the model replied without calling
// manage_plan at all — separate from planConsecutiveFailures (an explicit
// "failed" status IS the model engaging with the tool, just reporting it
// couldn't finish the step; this counts turns where it didn't engage at
// all). Reset whenever manage_plan is called, whatever the outcome.
let planNoToolCallCount = 0;
// The full plan and the step index we just sent to the model, remembered so
// the *next* turn's result can be compared against it to tell whether that
// specific step resolved to completed/failed — used to drive the
// consecutive-failure breaker and to reconstruct the prompt on Resume.
let lastPlanSteps = [];
let pendingPlanStep = null;

// Separate from the plan runner: whether the *current* turn's tool-calling
// loop (app.go's chatWithTools) is mid-flight. A single turn can dispatch
// several tool calls in a row before replying, and that should show the same
// banner/stop affordance as an auto-continuing plan — not just silently
// update the status bar text.
let toolLoopActive = false;
let currentToolName = '';

// Parses a manage_plan call's persisted arguments into a plain steps array.
// Malformed/empty entries are dropped rather than surfaced — nothing useful
// for the runner to do with a step it can't display or target.
function parsePlanSteps(call) {
    try {
        const parsed = JSON.parse(call.toolArgs);
        const raw = Array.isArray(parsed?.steps) ? parsed.steps : [];
        return raw
            .filter(s => s && typeof s.content === 'string' && s.content.trim() !== '')
            .map((s, i) => ({ seq: i + 1, content: s.content.trim(), status: s.status || 'pending' }));
    } catch { return []; }
}

// The step the runner should work on next: the one already marked
// in_progress (the manage_plan contract allows at most one), or otherwise
// the first pending one — the model isn't required to set in_progress
// itself, the runner is happy to just pick up the next pending step.
function pickCurrentStep(steps) {
    return steps.find(s => s.status === 'in_progress') || steps.find(s => s.status === 'pending') || null;
}

// Builds the prompt for an auto-advanced turn: the live plan state plus which
// step to work on. Only ever sent on plan-driven turns, never on a message
// the user typed themselves — see sendAndRender's `auto` flag.
function planPromptFor(steps, current) {
    const lines = steps
        .map(s => `${s.seq}. [${s.status}] ${s.content}${s.seq === current.seq ? '  ← you are here' : ''}`)
        .join('\n');
    return `Here is the current plan:\n${lines}\n\nContinue working on step ${current.seq}: "${current.content}". ` +
        `Call manage_plan (with the full plan) to mark it completed or failed before moving on — don't skip ahead to later steps in this same reply.`;
}

// Short human-facing label for a plan step — used both for the plan banner
// and as the auto-advanced turn's displayed "Task" line (see sendAndRender's
// displayText), instead of showing the full planPromptFor text (with the
// whole plan folded in) in the chat log, which just repeats what the Plan
// tab already shows.
function planTaskLabel(step, total) {
    return `Working on step ${step?.seq ?? '?'} of ${total}: ${step?.content ?? ''}`;
}

function advancePlan(msgs) {
    const call = msgs.find(m => m.role === 'tool' && m.toolName === 'manage_plan');
    // A plan-driven turn's own (unpinned) user row is tagged with toolName
    // 'manage_plan' — see queuedByTool in sendAndRender. If that's what this
    // turn was, but no manage_plan call came back, the model replied with
    // plain prose ("OK, I'll work on step 2...") instead of actually acting —
    // that's the specific failure mode this whole branch exists to catch.
    const wasAutoStep = msgs.some(m => m.role === 'user' && m.toolName === 'manage_plan');

    if (!call) {
        if (!wasAutoStep) return; // an ordinary turn — nothing plan-related happened
        planNoToolCallCount++;
    } else {
        planNoToolCallCount = 0;

        const steps = parsePlanSteps(call);
        if (steps.length === 0) return;
        lastPlanSteps = steps;

        // Track whether the step we just sent resolved to completed/failed,
        // to drive the consecutive-failure breaker below — matched by seq
        // rather than content, since the model may reword a step when it
        // resubmits.
        if (pendingPlanStep) {
            const resolved = steps.find(s => s.seq === pendingPlanStep.seq);
            if (resolved?.status === 'failed') {
                planConsecutiveFailures++;
            } else if (resolved?.status === 'completed') {
                planConsecutiveFailures = 0;
            }
        }

        const next = pickCurrentStep(steps);
        if (!next) {
            // Nothing left pending/in_progress — the plan is done.
            planRunning = false;
            planPaused = false;
            pendingPlanStep = null;
            updatePlanBanner();
            return;
        }
        pendingPlanStep = next;
    }

    if (planConsecutiveFailures >= MAX_CONSECUTIVE_PLAN_FAILURES || planNoToolCallCount >= MAX_CONSECUTIVE_PLAN_FAILURES) {
        // Pause rather than abandon: the plan and the current step stay
        // exactly where they are so Resume can pick the run back up instead
        // of forcing the user to re-explain the whole plan from scratch.
        planRunning = false;
        planPaused = true;
        updatePlanBanner();
        return;
    }

    planRunning = true;
    planPaused = false;
    updatePlanBanner();
    // A retry after the model skipped calling the tool gets a sharper
    // reminder appended — the same prompt alone already didn't work once.
    const prompt = planPromptFor(lastPlanSteps, pendingPlanStep) + (planNoToolCallCount > 0 ? PLAN_NO_PROGRESS_NUDGE : '');
    // Deliberately not awaited — a detached continuation, not a bug. This lets
    // the current turn's finally block (sidebar refresh, status reset) settle
    // immediately instead of waiting on the entire remaining plan.
    sendAndRender(prompt, { auto: true, displayText: planTaskLabel(pendingPlanStep, lastPlanSteps.length) });
}

export function stopPlanRun() {
    planRunning = false;
    planPaused = false;
    planConsecutiveFailures = 0;
    planNoToolCallCount = 0;
    pendingPlanStep = null;
    // Stop can't actually cancel an in-flight backend request (no cancellation
    // channel exists yet) — it only prevents further auto-continuation.
    // Clearing this at least dismisses the banner rather than leaving it
    // stuck showing "tool loop" once the user has asked to stop.
    toolLoopActive = false;
    updatePlanBanner();
}

// Manually resumes an auto-continuing plan run after it paused on repeated
// failures (or repeated no-tool-call replies) — resets both counters and
// re-sends the same step that was paused on, so the model gets another shot
// at it, nudge included if that's why it paused.
function resumePlanRun() {
    if (!pendingPlanStep) return;
    const hadNoToolCallStall = planNoToolCallCount > 0;
    planConsecutiveFailures = 0;
    planNoToolCallCount = 0;
    planPaused = false;
    planRunning = true;
    updatePlanBanner();
    const prompt = planPromptFor(lastPlanSteps, pendingPlanStep) + (hadNoToolCallStall ? PLAN_NO_PROGRESS_NUDGE : '');
    sendAndRender(prompt, {
        auto: true,
        displayText: planTaskLabel(pendingPlanStep, lastPlanSteps.length),
    });
}

function updatePlanBanner() {
    const busy = planRunning || planPaused || toolLoopActive;
    // Disabled while actively running (not just paused) so a manually-typed
    // message can't interleave with auto-steps/tool calls and corrupt turn
    // ordering — stop-then-type, not concurrent. Paused leaves input enabled
    // since nothing is in flight.
    const running = planRunning || toolLoopActive;
    textarea.disabled = running;
    sendBtn.disabled = running;
    if (!planBanner) return;
    planBanner.style.display = busy ? '' : 'none';
    if (planResumeBtn) planResumeBtn.style.display = planPaused ? '' : 'none';
    if (!busy) return;
    const label = planBanner.querySelector('.plan-banner-text');
    if (!label) return;
    if (planPaused) {
        const reason = planNoToolCallCount >= MAX_CONSECUTIVE_PLAN_FAILURES
            ? `${MAX_CONSECUTIVE_PLAN_FAILURES} replies without calling manage_plan`
            : `${MAX_CONSECUTIVE_PLAN_FAILURES} failed attempts`;
        label.textContent = `Paused after ${reason} on step ${pendingPlanStep?.seq ?? '?'}: ${pendingPlanStep?.content ?? ''}`;
    } else if (planRunning) {
        label.textContent = planTaskLabel(pendingPlanStep, lastPlanSteps.length);
    } else {
        label.textContent = `Executing ${currentToolName || 'tool'}…`;
    }
}

planStopBtn?.addEventListener('click', stopPlanRun);
planResumeBtn?.addEventListener('click', resumePlanRun);

// --- Live status from the backend (chain-of-thought / tool execution) ---
const THINKING_LABELS = ['Thinking…', 'Cogitating…', 'Pondering…'];
EventsOn('chat:status', (payload) => {
    if (!payload) return;
    // Marks the start of a new phase (hidden cot pass or tool dispatch) — the
    // message it produces will measure its elapsed time from here.
    phaseStartedAt = Date.now();
    if (payload.state === 'thinking') {
        setStatus(THINKING_LABELS[Math.floor(Math.random() * THINKING_LABELS.length)], 'loading');
    } else if (payload.state === 'tool') {
        setStatus(`Executing ${payload.tool}…`, 'loading');
        toolLoopActive = true;
        currentToolName = payload.tool;
        updatePlanBanner();
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

// Update the status bar and toolbar summary to show the current model name.
function updateStatusBarModel(name) {
    if (statusBarModel) statusBarModel.textContent = name;
    _summaryModel = name;
    updateToolbarSummary();
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

// Update the status bar and toolbar summary to show the current cot mode.
function updateStatusBarMode(name) {
    if (statusBarMode) statusBarMode.textContent = `⏱ ${name}`;
    _summaryMode = name;
    updateToolbarSummary();
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

// --- Directory selector (enables/disables the file tools) ---
// Shows the last path segment as a label; the button itself doubles as the
// "select" affordance whether or not a directory is already active.
function updateDirButton(dir) {
    if (!dirBtn) return;
    const base = dir ? dir.replace(/[\\/]+$/, '').split(/[\\/]/).pop() : '';
    dirBtn.textContent = dir ? base : 'None';
    dirBtn.title = dir ? `File tools enabled on: ${dir}` : 'Select a directory to enable file tools';
    dirBtn.classList.toggle('dir-active', !!dir);
    if (dirClearBtn) dirClearBtn.style.display = dir ? '' : 'none';
}

async function loadWorkDir() {
    if (!dirBtn) return;
    try {
        updateDirButton(await getWorkDir());
    } catch (err) {
        console.warn('Could not load file tool directory:', err.message);
    }
}

dirBtn?.addEventListener('click', async () => {
    try {
        const dir = await selectDirectory();
        updateDirButton(dir);
    } catch (err) {
        console.error('Failed to select directory:', err);
    }
});

dirClearBtn?.addEventListener('click', async (e) => {
    e.stopPropagation();
    try {
        await clearDirectory();
        updateDirButton('');
    } catch (err) {
        console.error('Failed to clear directory:', err);
    }
});

// Re-populate on startup and whenever the selector loses focus (covers reconnects).
loadModels();
loadCotModes();
loadWorkDir();

// Clear demo data 
if (chatLog.querySelector('.message')) {
    chatLog.innerHTML = '';
}
