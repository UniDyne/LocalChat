import { createArtifactManual, saveArtifact } from './api';
import { renderArtifactList } from './artifacts';
import { renderMarkdown, renderHighlighted, escapeHtml } from './content';

// Registry maps tool names to custom renderer functions (rec) => void.
// A renderer is responsible for building and mounting its own overlay.
const registry = new Map();

// --- Shared helpers ---

// Wire an "Add Artifact" button: creates an artifact, refreshes the sidebar,
// and mutates the button text on success.
function wireAddArtifact(btn, title, content, contentType) {
    btn.addEventListener('click', async () => {
        btn.disabled = true;
        try {
            await createArtifactManual(title, content, contentType);
            await renderArtifactList();
            btn.textContent = 'Added';
        } catch (err) {
            console.error('tool-views: addArtifact', err);
            btn.disabled = false;
        }
    });
}

// Wire a "Download" button: creates an artifact (so the backend can open a
// native save dialog with a proper filename), refreshes the sidebar, and
// triggers the save dialog.
function wireDownload(btn, title, content, contentType) {
    btn.addEventListener('click', async () => {
        btn.disabled = true;
        try {
            const id = await createArtifactManual(title, content, contentType);
            await renderArtifactList();
            await saveArtifact(id);
            btn.textContent = 'Saved';
        } catch (err) {
            console.error('tool-views: download', err);
            btn.disabled = false;
        }
    });
}

// Build and mount the outer overlay div.
function createOverlay() {
    const overlay = document.createElement('div');
    overlay.className = 'artifact-preview-overlay';
    overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
    return overlay;
}

// --- Generic fallback (replicates the original two-pane JSON lightbox) ---

function openGenericLightbox(rec) {
    let argsPretty = rec.toolArgs || '';
    try { argsPretty = JSON.stringify(JSON.parse(rec.toolArgs), null, 2); } catch {}

    let resultPretty = rec.toolResult || '';
    let resultLang = 'plaintext';
    try { resultPretty = JSON.stringify(JSON.parse(rec.toolResult), null, 2); resultLang = 'json'; } catch {}

    const overlay = createOverlay();
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
}

// --- web_fetch view ---
// Result shape: {url, title, content, cached}

registry.set('web_fetch', (rec) => {
    let result;
    try { result = JSON.parse(rec.toolResult); } catch {}
    if (!result || typeof result.content !== 'string') {
        openGenericLightbox(rec);
        return;
    }

    const { url, title, content, cached } = result;
    const displayTitle = title || url || 'Fetched Page';

    const overlay = createOverlay();
    overlay.innerHTML = `
        <div class="artifact-preview-panel tool-lightbox-panel">
            <div class="artifact-preview-header">
                <span class="artifact-preview-title">🌐 ${escapeHtml(displayTitle)}</span>
                <div class="tool-view-header-actions">
                    ${cached ? '<span class="tool-view-cached-badge">cached</span>' : ''}
                    <button class="artifact-download-btn js-download">Download</button>
                    <button class="artifact-download-btn js-add-artifact">Add Artifact</button>
                    <button class="artifact-close-btn">&times;</button>
                </div>
            </div>
            <div class="tool-view-url-bar">${escapeHtml(url || '')}</div>
            <div class="artifact-preview-body tool-view-body markdown-body"></div>
        </div>`;

    overlay.querySelector('.tool-view-body').innerHTML = renderMarkdown(content);
    overlay.querySelector('.artifact-close-btn').addEventListener('click', () => overlay.remove());
    wireDownload(overlay.querySelector('.js-download'), displayTitle, content, 'markdown');
    wireAddArtifact(overlay.querySelector('.js-add-artifact'), displayTitle, content, 'markdown');
});

// --- web_search view ---
// Result shape: [{title, url, snippet}, ...]

registry.set('web_search', (rec) => {
    let results;
    try { results = JSON.parse(rec.toolResult); } catch {}
    if (!Array.isArray(results)) {
        openGenericLightbox(rec);
        return;
    }

    let args;
    try { args = JSON.parse(rec.toolArgs); } catch { args = {}; }
    const query = args.query || '';

    const artifactContent = [
        `# Search: ${query}`,
        '',
        ...results.map((r, i) => [
            `### ${i + 1}. ${r.title || 'Result'}`,
            '',
            `**${r.url}**`,
            '',
            r.snippet || '',
        ].join('\n')),
    ].join('\n\n');

    const resultsHtml = results.map((r, i) => `
        <div class="tool-view-search-result">
            <div class="tool-view-result-index">${i + 1}</div>
            <div class="tool-view-result-content">
                <div class="tool-view-result-title">${escapeHtml(r.title || 'Untitled')}</div>
                <div class="tool-view-result-url">${escapeHtml(r.url || '')}</div>
                ${r.snippet ? `<div class="tool-view-result-snippet">${escapeHtml(r.snippet)}</div>` : ''}
            </div>
        </div>`).join('');

    const overlay = createOverlay();
    overlay.innerHTML = `
        <div class="artifact-preview-panel tool-lightbox-panel">
            <div class="artifact-preview-header">
                <span class="artifact-preview-title">🔍 Web Search</span>
                <div class="tool-view-header-actions">
                    <button class="artifact-download-btn js-download">Download</button>
                    <button class="artifact-download-btn js-add-artifact">Add Artifact</button>
                    <button class="artifact-close-btn">&times;</button>
                </div>
            </div>
            <div class="tool-view-query-bar">${escapeHtml(query)}</div>
            <div class="tool-view-results-count">${results.length} result${results.length !== 1 ? 's' : ''}</div>
            <div class="artifact-preview-body tool-view-body">
                <div class="tool-view-search-results">${resultsHtml}</div>
            </div>
        </div>`;

    overlay.querySelector('.artifact-close-btn').addEventListener('click', () => overlay.remove());

    const artifactTitle = query ? `Search: ${query}` : 'Search Results';
    wireDownload(overlay.querySelector('.js-download'), artifactTitle, artifactContent, 'markdown');
    wireAddArtifact(overlay.querySelector('.js-add-artifact'), artifactTitle, artifactContent, 'markdown');
});

// --- Main entry point ---

export function openToolLightbox(rec) {
    const renderer = registry.get(rec.toolName);
    if (renderer) {
        renderer(rec);
    } else {
        openGenericLightbox(rec);
    }
}
