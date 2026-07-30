import {
    memoryStatus, memorySources, selectMemoryDirectory, ingestDirectory,
    forgetMemoryFolder, searchMemory, defaultMemoryWeights,
    rebuildMemoryEdges, enrichMemoryEntities, memoryModelInfo, provisionMemoryModel,
    memoryReady, indexExistingHistory, unindexedHistoryCount,
} from './api';
import { escapeHtml } from './content';

// --- Memory tab (left sidebar) ---
//
// Left, not right: the right pane holds session-relative things (this chat's artifacts,
// this chat's plan) while the left holds global ones. Memory spans every session — a
// note indexed once is searchable from any conversation — so it belongs with Sessions.
//
// Phase 9's exit criterion is that memory is manageable without touching the database
// by hand. Concretely that means four things have to be visible or doable here:
// whether embeddings work (and why not, when they don't), what has been indexed,
// what a query actually retrieves, and *why* it retrieved that.
//
// The last one is why every result carries its four-signal breakdown. §3.5 is explicit
// that without it, tuning the weights is guesswork — and the weights are currently
// unsettled, so the breakdown is the working surface rather than a debug extra.

const statusEl = document.getElementById('memoryStatus');
const sourcesEl = document.getElementById('memorySources');
const searchFormEl = document.getElementById('memorySearchForm');
const searchInputEl = document.getElementById('memorySearchInput');
const resultsEl = document.getElementById('memoryResults');
const tuningEl = document.getElementById('memoryTuning');

let lastProgress = null;
let weights = null;
// modelInfoCache holds the static model description; cleared when provisioning changes
// whether the model is present.
let modelInfoCache = null;

// ---------- status ----------

// renderInFlight serializes status renders. Each one awaits backend calls and then
// replaces innerHTML wholesale, so two overlapping renders can interleave — the later
// one's listeners attached to DOM the earlier one is about to discard.
let renderInFlight = null;

export function renderMemoryStatus() {
    // Coalesce rather than queue: if a render is already running, the caller wants
    // "current state shown", and the one in flight will show it.
    if (renderInFlight) return renderInFlight;
    renderInFlight = doRenderMemoryStatus().finally(() => { renderInFlight = null; });
    return renderInFlight;
}

async function doRenderMemoryStatus() {
    if (!statusEl) return;
    try {
        const st = await memoryStatus();
        const c = st.corpus || {};

        // The unavailable reason is shown in full rather than summarised. It is the
        // difference between "provision the model" and "your ONNX Runtime is too old",
        // and both are actionable — but only if the user can read which one it is.
        const embedRow = st.embeddingsAvailable
            ? `<span class="mem-ok">semantic search on</span>
               <span class="mem-dim">${escapeHtml(c.embedModel || '')} · ${c.embedDims || 0}d</span>`
            : `<span class="mem-warn">semantic search off</span>
               <div class="mem-reason">${escapeHtml(st.unavailableReason || 'unknown reason')}
               <br><span class="mem-dim">Keyword, entity and character matching still work.</span></div>`;

        // The download offer only appears when the *model* is what's missing. If the
        // model is present and embeddings are still off, the problem is the ONNX
        // Runtime, and offering to re-download 133 MB would be actively misleading.
        let provision = '';
        if (!st.embeddingsAvailable) {
            try {
                // Cached: the model's identity, size and destination are fixed for the
                // session, and only provisioning changes `present` — which forces a
                // refresh below. Re-fetching it on every render was one backend call per
                // render for data that never moves.
                const info = modelInfoCache || (modelInfoCache = await memoryModelInfo());
                provision = info.present
                    ? `<div class="mem-hint">The model is present but could not be loaded — this is the ONNX Runtime, not the model. See the reason above.</div>`
                    : `<div class="mem-provision">
                         <button id="memProvision">Download the model (${Math.round(info.bytes / 1e6)} MB)</button>
                         <div class="mem-dim">${escapeHtml(info.name)} @ ${escapeHtml(info.revision.slice(0, 7))} → ${escapeHtml(info.targetDir)}</div>
                       </div>`;
            } catch (err) {
                console.error('memoryModelInfo', err);
            }
        }

        const edges = st.edgesByKind || {};
        const edgeParts = Object.keys(edges).sort().map(k => `${k} ${edges[k]}`).join(', ');
        const pass = st.entityPass || {};
        const pending = pass.pending || 0;

        const q = st.queue || {};
        const busy = q.running || (q.pending > 0 ? 'queued' : '');

        // Ingestion is event-driven, so history that predates memory is invisible to it
        // and nothing would ever replay it. Offered only when there is something to do,
        // so it disappears once the backfill has run rather than sitting there implying
        // work is outstanding.
        let backfill = '';
        try {
            const pendingHistory = await unindexedHistoryCount();
            if (pendingHistory > 0) {
                backfill = `<div class="mem-hint">${pendingHistory} earlier chat${pendingHistory === 1 ? '' : 's'} ${pendingHistory === 1 ? 'is' : 'are'} not indexed —
                    conversations are only captured from when memory started.
                    <button id="memBackfill" class="mem-inline-btn">Index them now</button></div>`;
            }
        } catch (err) {
            console.error('unindexedHistoryCount', err);
        }

        statusEl.innerHTML = `
            <div class="mem-section">
                <div class="mem-row">${embedRow}</div>
                ${busy ? `<div class="mem-busy">working: ${escapeHtml(busy)}${q.pending ? ` (+${q.pending} queued)` : ''}</div>` : ''}
                ${lastProgress ? `<div class="mem-dim mem-progress">${escapeHtml(lastProgress)}</div>` : ''}
            </div>
            <div class="mem-stats">
                <div><b>${c.sources || 0}</b><span>sources</span></div>
                <div><b>${c.chunks || 0}</b><span>chunks</span></div>
                <div><b>${c.embeddedChunks || 0}</b><span>embedded</span></div>
                <div><b>${c.entities || 0}</b><span>entities</span></div>
                <div><b>${c.edges || 0}</b><span>edges</span></div>
                <div><b>${c.terms || 0}</b><span>terms</span></div>
            </div>
            ${edgeParts ? `<div class="mem-dim mem-edges">${escapeHtml(edgeParts)}</div>` : ''}
            <div class="mem-actions">
                <button id="memAddFolder">Index a folder…</button>
                ${(c.chunks || 0) > 0 ? `<button id="memRebuildEdges" class="mem-secondary" title="Rebuild the similarity graph. Needed if the corpus was indexed before the embedding model was available.">Rebuild edges</button>` : ''}
                ${st.extractModel ? `<button id="memEnrich" class="mem-secondary" title="Run the optional LLM entity pass (${escapeHtml(st.extractModel)}). ${pending} sources pending.">Enrich (${pending})</button>` : ''}
            </div>
            ${provision}
            ${backfill}
            ${!st.embeddingsAvailable && (c.chunks || 0) > 0
                ? `<div class="mem-hint">Chunks are stored without vectors. Downloading the model embeds them automatically — no re-indexing needed.</div>`
                : ''}`;

        statusEl.querySelector('#memAddFolder')?.addEventListener('click', onAddFolder);
        statusEl.querySelector('#memRebuildEdges')?.addEventListener('click', onRebuildEdges);
        statusEl.querySelector('#memEnrich')?.addEventListener('click', onEnrich);
        statusEl.querySelector('#memProvision')?.addEventListener('click', onProvision);
        statusEl.querySelector('#memBackfill')?.addEventListener('click', onBackfillHistory);
        cancelStartupRetry();
    } catch (err) {
        console.error('renderMemoryStatus', err);
        renderUnavailable(err);
    }
}

// startupRetry polls while the backend is still building the subsystem.
let startupRetry = null;

function cancelStartupRetry() {
    if (startupRetry) {
        clearTimeout(startupRetry);
        startupRetry = null;
    }
}

// renderUnavailable draws the failure state — and keeps the primary action available.
//
// The previous version replaced the whole panel with an error string, which had two
// consequences worth naming. The Memory tab lost its "Index a folder" button, because
// that button lived inside the status template; and a *transient* failure latched
// forever, because nothing ever re-rendered. Both showed up as "memory is broken" when
// the backend was merely still starting.
function renderUnavailable(err) {
    const msg = String(err && err.message ? err.message : err);
    // "still starting" is the retryable case the backend distinguishes for us.
    const starting = /still starting/i.test(msg);

    statusEl.innerHTML = `
        <div class="mem-section">
            <div class="mem-row">
                <span class="mem-warn">${starting ? 'starting…' : 'memory unavailable'}</span>
            </div>
            <div class="mem-reason">${escapeHtml(msg)}${starting
                ? '<br><span class="mem-dim">The subsystem loads an embedding model at startup; this can take a few seconds.</span>'
                : ''}</div>
        </div>
        <div class="mem-actions">
            <button id="memRetry" class="mem-secondary">Retry</button>
        </div>`;
    statusEl.querySelector('#memRetry')?.addEventListener('click', () => {
        cancelStartupRetry();
        renderMemoryStatus();
        renderMemorySources();
    });

    // Poll only while it is plausibly still coming up. A hard failure — a database that
    // will not open, say — is not going to fix itself, and hammering it would just hide
    // the message under a spinner.
    if (starting) {
        cancelStartupRetry();
        startupRetry = setTimeout(async () => {
            startupRetry = null;
            if (await memoryReady().catch(() => false)) {
                renderMemoryStatus();
                renderMemorySources();
            } else {
                renderMemoryStatus();
            }
        }, 750);
    }
}

async function onAddFolder() {
    try {
        const dir = await selectMemoryDirectory();
        if (dir) lastProgress = `indexing ${dir}…`;
        await renderMemoryStatus();
    } catch (err) {
        console.error('selectMemoryDirectory', err);
        lastProgress = `failed: ${err}`;
        await renderMemoryStatus();
    }
}

async function onBackfillHistory() {
    try {
        await indexExistingHistory();
        lastProgress = 'indexing earlier chats…';
    } catch (err) {
        lastProgress = `backfill refused: ${err}`;
    }
    await renderMemoryStatus();
}

async function onProvision() {
    try {
        await provisionMemoryModel();
        modelInfoCache = null;
        lastProgress = 'starting model download…';
    } catch (err) {
        lastProgress = `download refused: ${err}`;
    }
    await renderMemoryStatus();
}

async function onRebuildEdges() {
    try {
        await rebuildMemoryEdges();
        lastProgress = 'rebuilding edges…';
    } catch (err) {
        lastProgress = `edge rebuild refused: ${err}`;
    }
    await renderMemoryStatus();
}

async function onEnrich() {
    try {
        await enrichMemoryEntities(0);
        lastProgress = 'entity enrichment queued…';
    } catch (err) {
        lastProgress = `enrichment refused: ${err}`;
    }
    await renderMemoryStatus();
}

// ---------- indexed sources ----------

export async function renderMemorySources() {
    if (!sourcesEl) return;
    try {
        const sources = await memorySources();
        if (!sources || sources.length === 0) {
            sourcesEl.innerHTML = `<div class="mem-dim">Nothing indexed yet. Index a folder of Markdown notes, and conversations will be added as you chat.</div>`;
            return;
        }

        // Folders are shown as one row per folder rather than one per note: a vault is
        // thousands of files, and a scrolling wall of them is not manageability. Re-scan
        // and forget act on the folder, which is the unit a user thinks in.
        const byDir = new Map();
        const others = [];
        for (const s of sources) {
            if (s.sourceType === 'directory') {
                const dir = folderOf(s.path, s.sourceRef);
                const e = byDir.get(dir) || { dir, notes: 0, tokens: 0 };
                e.notes++;
                e.tokens += s.tokenCount || 0;
                byDir.set(dir, e);
            } else {
                others.push(s);
            }
        }

        const folderRows = [...byDir.values()].sort((a, b) => a.dir.localeCompare(b.dir)).map(f => `
            <div class="mem-source" data-dir="${escapeHtml(f.dir)}">
                <div class="mem-source-main">
                    <div class="mem-source-title">${escapeHtml(f.dir || '(unknown folder)')}</div>
                    <div class="mem-dim">${f.notes} note${f.notes === 1 ? '' : 's'} · ${f.tokens.toLocaleString()} tokens</div>
                </div>
                <button class="mem-rescan" title="Re-scan for changes. Unchanged files are skipped without being read.">↻</button>
                <button class="mem-forget" title="Forget every note indexed from this folder. The files on disk are untouched.">✕</button>
            </div>`).join('');

        const otherCounts = others.reduce((m, s) => (m[s.sourceType] = (m[s.sourceType] || 0) + 1, m), {});
        const otherRows = Object.keys(otherCounts).sort().map(k => `
            <div class="mem-source">
                <div class="mem-source-main">
                    <div class="mem-source-title">${escapeHtml(k)}</div>
                    <div class="mem-dim">${otherCounts[k]} indexed automatically</div>
                </div>
            </div>`).join('');

        sourcesEl.innerHTML = folderRows + otherRows;
        sourcesEl.querySelectorAll('.mem-rescan').forEach(btn => {
            btn.addEventListener('click', async (e) => {
                const dir = e.target.closest('.mem-source').dataset.dir;
                try {
                    await ingestDirectory(dir);
                    lastProgress = `re-scanning ${dir}…`;
                } catch (err) {
                    lastProgress = `re-scan refused: ${err}`;
                }
                await renderMemoryStatus();
            });
        });
        sourcesEl.querySelectorAll('.mem-forget').forEach(btn => {
            btn.addEventListener('click', async (e) => {
                const dir = e.target.closest('.mem-source').dataset.dir;
                // Confirmed because it is destructive and not undoable without
                // re-indexing. The wording says what is and is not deleted, since
                // "forget" could plausibly mean either.
                if (!window.confirm(`Forget everything indexed from ${dir}?\n\nThe files on disk are not touched.`)) return;
                try {
                    const n = await forgetMemoryFolder(dir);
                    lastProgress = `forgot ${n} note${n === 1 ? '' : 's'} from ${dir}`;
                } catch (err) {
                    lastProgress = `forget refused: ${err}`;
                }
                await renderMemoryStatus();
                await renderMemorySources();
            });
        });
    } catch (err) {
        console.error('renderMemorySources', err);
        // Blank would read as "nothing indexed", which is a different and more alarming
        // claim than "could not read the list".
        sourcesEl.innerHTML = `<div class="mem-dim">Could not read the index: ${escapeHtml(String(err && err.message ? err.message : err))}</div>`;
    }
}

// folderOf recovers the indexed folder from a note's absolute path by stripping the
// vault-relative part, which is what source_ref holds.
function folderOf(absPath, relRef) {
    if (!absPath) return '';
    if (relRef && absPath.endsWith(relRef)) {
        return absPath.slice(0, absPath.length - relRef.length).replace(/[\\/]+$/, '');
    }
    const i = Math.max(absPath.lastIndexOf('/'), absPath.lastIndexOf('\\'));
    return i > 0 ? absPath.slice(0, i) : absPath;
}

// ---------- search, with the score breakdown ----------

const SIGNALS = ['bm25', 'vector', 'entity', 'ngram'];

async function renderTuning() {
    if (!tuningEl) return;
    if (!weights) {
        try {
            weights = await defaultMemoryWeights();
        } catch {
            weights = { bm25: 0.2, vector: 0.6, entity: 0.1, ngram: 0.1, mode: 'weighted' };
        }
    }
    tuningEl.innerHTML = `
        <details class="mem-tuning">
            <summary>Tuning</summary>
            <div class="mem-dim mem-tuning-note">These weights are not settled — they were tuned against an
            entity signal that has since changed. Adjust and compare against the breakdown on each result.</div>
            ${SIGNALS.map(k => `
                <label class="mem-weight">
                    <span>${k}</span>
                    <input type="range" min="0" max="1" step="0.05" value="${weights[k]}" data-signal="${k}">
                    <output>${Number(weights[k]).toFixed(2)}</output>
                </label>`).join('')}
            <label class="mem-toggle"><input type="checkbox" id="memRRF" ${weights.mode === 'rrf' ? 'checked' : ''}> rank fusion (RRF) instead of weighted sum</label>
            <label class="mem-toggle"><input type="checkbox" id="memExpand" checked> follow graph connections</label>
            <button id="memResetWeights" class="mem-secondary">Reset to defaults</button>
        </details>`;

    tuningEl.querySelectorAll('input[type=range]').forEach(r => {
        r.addEventListener('input', () => {
            weights[r.dataset.signal] = parseFloat(r.value);
            r.nextElementSibling.textContent = Number(r.value).toFixed(2);
        });
    });
    tuningEl.querySelector('#memRRF').addEventListener('change', (e) => {
        weights.mode = e.target.checked ? 'rrf' : 'weighted';
    });
    tuningEl.querySelector('#memResetWeights').addEventListener('click', async () => {
        weights = null;
        await renderTuning();
    });
}

async function onSearch(e) {
    e.preventDefault();
    const q = searchInputEl.value.trim();
    if (!q) return;
    resultsEl.innerHTML = `<div class="mem-dim">Searching…</div>`;

    const expand = tuningEl?.querySelector('#memExpand')?.checked !== false;
    try {
        const resp = await searchMemory(q, 8, { ...(weights || {}), expand });
        renderResults(resp.results || [], resp.report || {});
    } catch (err) {
        console.error('searchMemory', err);
        resultsEl.innerHTML = `<div class="mem-reason">Search failed: ${escapeHtml(String(err))}</div>`;
    }
}

function renderResults(results, report) {
    // The report goes first, and says what happened even when nothing matched. A bare
    // "no results" cannot distinguish an empty corpus from an unprovisioned model from
    // a query whose terms appear nowhere — three different things to do next.
    const arms = `${report.fromBm25 || 0} keyword · ${report.fromVector || 0} semantic · ${report.fromEntity || 0} entity`;
    const walk = report.walk
        ? ` · walk ${report.walk.expanded || 0} from ${report.walk.seeds || 0} seeds`
        : '';
    let head = `<div class="mem-dim mem-report">${report.candidates || 0} candidates (${arms})${walk}`;
    if (report.expandedReturned) {
        head += ` · ${report.expandedReturned} shown via connections`;
    }
    if (report.dedupedResults) {
        head += ` · ${report.dedupedResults} near-duplicate${report.dedupedResults === 1 ? '' : 's'} dropped`;
    }
    head += `</div>`;
    if (report.vectorSkipped) {
        head += `<div class="mem-reason">Semantic search skipped: ${escapeHtml(report.vectorSkipped)}</div>`;
    }
    if (report.queryEntities && report.queryEntities.length) {
        head += `<div class="mem-dim">recognised: ${report.queryEntities.map(escapeHtml).join(', ')}</div>`;
    }

    if (!results.length) {
        resultsEl.innerHTML = head + `<div class="mem-dim">No matches.</div>`;
        return;
    }

    resultsEl.innerHTML = head + results.map((r, i) => {
        const raw = r.raw || {};
        const norm = r.normalized || {};
        // Both raw and normalized are shown: normalized is what the weights multiply,
        // raw is what the signal actually measured. Seeing only one makes a zero
        // ambiguous between "signal absent" and "signal constant across candidates".
        const bars = SIGNALS.map(k => {
            const n = Math.max(0, Math.min(1, norm[k] || 0));
            return `<div class="mem-sig" title="${k}: normalized ${(norm[k] || 0).toFixed(3)}, raw ${(raw[k] || 0).toFixed(3)}">
                        <span class="mem-sig-label">${k[0]}</span>
                        <span class="mem-sig-bar"><i style="width:${(n * 100).toFixed(0)}%"></i></span>
                    </div>`;
        }).join('');

        const via = r.expanded
            ? `<span class="mem-via" title="Found by following a ${escapeHtml(r.via)} connection, ${r.depth} hop(s) out — this excerpt need not mention your query at all.">via ${escapeHtml(r.via)} ×${r.depth}</span>`
            : '';

        return `
            <div class="mem-result">
                <div class="mem-result-head">
                    <span class="mem-result-rank">${i + 1}</span>
                    <span class="mem-result-title">${escapeHtml(r.title || r.sourceRef)}</span>
                    <span class="mem-result-score">${(r.score || 0).toFixed(3)}</span>
                </div>
                <div class="mem-result-meta">
                    <span class="mem-badge">${escapeHtml(r.sourceType)}</span>
                    ${r.headingPath ? `<span class="mem-dim">${escapeHtml(r.headingPath)}</span>` : ''}
                    ${via}
                </div>
                <div class="mem-signals">${bars}</div>
                <div class="mem-result-text">${escapeHtml(r.text || '')}</div>
            </div>`;
    }).join('');
}

// updateProgressLine rewrites just the progress text, creating it if the current render
// did not include one. Deliberately DOM-only: no backend call, no re-render.
function updateProgressLine() {
    if (!statusEl) return;
    let el = statusEl.querySelector('.mem-progress');
    if (!el) {
        const section = statusEl.querySelector('.mem-section');
        if (!section) return;
        el = document.createElement('div');
        el.className = 'mem-dim mem-progress';
        section.appendChild(el);
    }
    el.textContent = lastProgress || '';
}

// ---------- wiring ----------

if (searchFormEl) searchFormEl.addEventListener('submit', onSearch);

// Live progress from the ingestion queue's single worker. Kept as a one-line summary
// rather than a log: the interesting state is "is it busy and roughly where", and the
// reports that matter (what was skipped, what was deleted) are logged backend-side.
if (window.runtime?.EventsOn) {
    // Emitted once startup finishes building the subsystem. This is what turns the
    // startup race from a permanent error into a brief "starting…" — the UI that
    // rendered too early gets told when to look again.
    window.runtime.EventsOn('memory:ready', () => {
        cancelStartupRetry();
        renderMemoryStatus();
        renderMemorySources();
    });
    window.runtime.EventsOn('memory:progress', (p) => {
        if (!p) return;
        lastProgress = p.running
            ? `${p.running}${p.pending ? ` (+${p.pending} queued)` : ''}`
            : (p.pending ? `${p.pending} queued` : 'idle');
        renderMemoryStatus();
    });
    window.runtime.EventsOn('memory:ingested', (rep) => {
        if (!rep) return;
        const parts = [`${rep.filesIngested || 0} indexed`];
        if (rep.filesUnchanged) parts.push(`${rep.filesUnchanged} unchanged`);
        if (rep.filesDeleted) parts.push(`${rep.filesDeleted} removed`);
        if (rep.filesTooLarge) parts.push(`${rep.filesTooLarge} too large`);
        if (rep.filesUnreadable) parts.push(`${rep.filesUnreadable} unreadable`);
        if (rep.linksUnresolved) parts.push(`${rep.linksUnresolved} unresolved links`);
        lastProgress = parts.join(', ');
        renderMemoryStatus();
        renderMemorySources();
    });
    window.runtime.EventsOn('memory:provision', (p) => {
        if (!p) return;
        if (p.error) {
            lastProgress = `model download failed: ${p.error}`;
        } else if (p.complete) {
            lastProgress = 'model ready — embedding indexed notes…';
        } else if (p.verifying) {
            lastProgress = `verifying ${p.file}…`;
        } else {
            const mb = (n) => (n / 1e6).toFixed(0);
            lastProgress = `${p.file} ${mb(p.done)}/${mb(p.total)} MB (${p.percent.toFixed(0)}%)${p.resumed ? ' — resumed' : ''}`;
        }
        // Update the progress line in place. A full renderMemoryStatus() here would fire
        // two backend calls *per progress event*, which over a 133 MB download is
        // thousands of round-trips through the UI bridge for the sake of a percentage —
        // and a stream of overlapping renders, since none of them are serialized.
        // Progress text is the only thing that changed; only it is rewritten.
        updateProgressLine();
        // A finished or failed download does change the rest of the panel — that is the
        // one case worth a full refresh, and it happens once.
        if (p.complete || p.error) {
            renderMemoryStatus();
            renderMemorySources();
        }
    });
    window.runtime.EventsOn('memory:backfilled', (r) => {
        if (!r) return;
        lastProgress = `indexed ${r.turns || 0} earlier turn${r.turns === 1 ? '' : 's'} and ${r.artifacts || 0} artifact${r.artifacts === 1 ? '' : 's'}`;
        renderMemoryStatus();
        renderMemorySources();
    });
    window.runtime.EventsOn('memory:enriched', (rep) => {
        if (!rep) return;
        const g = rep.guards || {};
        lastProgress = `enriched ${rep.sourcesDone || 0} sources, ${g.entitiesAccepted || 0}/${g.entitiesProposed || 0} entities kept`;
        renderMemoryStatus();
    });
}

// Sequential, not three concurrent calls.
//
// Firing them together spawned three simultaneous backend goroutines, each hitting the
// embedded database through cgo — the first concurrent database access this app ever had,
// since the other tabs load one after another. There is no reason for a sidebar panel to
// race itself at startup, and serializing costs nothing perceptible.
(async () => {
    try {
        await renderMemoryStatus();
        await renderMemorySources();
        await renderTuning();
    } catch (err) {
        console.error('memory tab init', err);
    }
})();
