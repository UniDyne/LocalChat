import { getArtifacts, getArtifactContent, getCurrentSession, saveArtifact, importArtifact } from './api';
import { escapeHtml, renderMarkdown, renderHighlighted } from './content';

// --- Artifact list (right sidebar) ---

const artifactListEl = document.getElementById('artifactList');

// Render the artifact list for the currently active session.
export async function renderArtifactList() {
    if (!artifactListEl) return;
    try {
        const sessionId = await getCurrentSession();
        const artifacts = await getArtifacts(sessionId);
        artifactListEl.innerHTML = '';

        if (!artifacts || artifacts.length === 0) {
            artifactListEl.innerHTML = `
                <div class="right-empty">Artifacts<br><br>
                <span style="font-size:10px;color:var(--text-muted)">Artifacts created during this chat will appear here</span></div>`;
            return;
        }

        for (const a of artifacts) {
            const div = document.createElement('div');
            div.className = 'artifact-item';
            const dateStr = a.createdAt ? new Date(a.createdAt).toLocaleString() : '';
            div.innerHTML = `
                <div class="artifact-title">${escapeHtml(a.title || 'Untitled')}</div>
                <div class="artifact-meta">
                    <span class="artifact-type-badge">${escapeHtml(a.contentType || 'text')}</span>
                    <span>${escapeHtml(dateStr)}</span>
                </div>`;
            div.addEventListener('click', () => openArtifactPreview(a.id));
            artifactListEl.appendChild(div);
        }
    } catch (err) {
        console.error('renderArtifactList', err);
    }
}

// --- Preview overlay ---

async function openArtifactPreview(id) {
    let artifact;
    try {
        artifact = await getArtifactContent(id);
    } catch (err) {
        console.error('getArtifactContent', err);
        return;
    }

    const isMarkdown = artifact.contentType === 'markdown';
    // contentType (python, go, javascript, json, yaml, html, css, sql, text, ...)
    // is passed straight through as the hljs language name — renderHighlighted
    // falls back to plain escaped text for "text" or anything unregistered.
    const bodyHtml = isMarkdown
        ? renderMarkdown(artifact.content)
        : `<pre class="artifact-preview-pre"><code>${renderHighlighted(artifact.content, artifact.contentType)}</code></pre>`;

    const overlay = document.createElement('div');
    overlay.className = 'artifact-preview-overlay';
    overlay.innerHTML = `
        <div class="artifact-preview-panel">
            <div class="artifact-preview-header">
                <span class="artifact-preview-title">${escapeHtml(artifact.title || 'Untitled')}</span>
                <div>
                    <button class="artifact-download-btn">Download</button>
                    <button class="artifact-close-btn">&times;</button>
                </div>
            </div>
            <div class="artifact-preview-body markdown-body">${bodyHtml}</div>
        </div>`;

    overlay.querySelector('.artifact-close-btn').addEventListener('click', () => overlay.remove());
    overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.remove(); });
    overlay.querySelector('.artifact-download-btn').addEventListener('click', () => downloadArtifact(artifact.id));

    document.body.appendChild(overlay);
}

// Opens a native save dialog (via the backend's SaveArtifact, which also
// picks the default filename from the artifact's own title/content type) and
// writes the artifact to wherever the user chooses. A blob/<a download> URL
// doesn't reliably trigger a save inside Wails' embedded webview the way it
// would in a full browser, so this goes through the backend instead.
async function downloadArtifact(id) {
    try {
        await saveArtifact(id);
    } catch (err) {
        console.error('saveArtifact', err);
    }
}

document.getElementById('artifactImportBtn')?.addEventListener('click', async () => {
    try {
        const sessionId = await getCurrentSession();
        const meta = await importArtifact(sessionId);
        if (meta?.id) await renderArtifactList();
    } catch (err) {
        console.error('importArtifact', err);
    }
});

renderArtifactList();
