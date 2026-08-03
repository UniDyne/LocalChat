import { ListModels, SendChat, GetModel, SetModel, ModelContextLength } from '../wailsjs/go/main/App';
import { ListCotModes, GetCotMode, SetCotMode } from '../wailsjs/go/main/App';
import { CreateSession, GetCurrentSession, GetSessions, SwitchSession, DeleteSession, RenameSession, GetMessages } from '../wailsjs/go/main/App';
import { GetArtifacts, GetArtifactContent, CreateArtifactManual, SaveArtifact, ImportArtifact } from '../wailsjs/go/main/App';
import { GetSessionToolStates, SetSessionToolEnabled } from '../wailsjs/go/main/App';
import { SetMessagePinned } from '../wailsjs/go/main/App';
import { SelectDirectory, ClearDirectory, GetWorkDir } from '../wailsjs/go/main/App';
import { GetPlan } from '../wailsjs/go/main/App';
import { MemoryStatus, MemorySources, SelectMemoryDirectory, IngestDirectory, ForgetMemorySource, ForgetMemoryFolder, MemoryReady, IndexExistingHistory, UnindexedHistoryCount } from '../wailsjs/go/main/App';
import { SearchMemoryTuned, DefaultMemoryWeights, RebuildMemoryEdges, EnrichMemoryEntities, RollbackMemoryEntityEnrichment } from '../wailsjs/go/main/App';
import { ProvisionMemoryModel, GetMemoryModelInfo } from '../wailsjs/go/main/App';

/**
 * Return an array of available model names from the configured Ollama server.
 */
export async function listModels() {
    const models = await ListModels();
    return models;
}

/**
 * Return the current active model name.
 */
export async function getModel() {
    return await GetModel();
}

/**
 * Set a new model by name. Throws if Ollama rejects it.
 */
export async function setModel(name) {
    await SetModel(name);
}

/**
 * Return a model's trained maximum context length in tokens (0 if unknown).
 * An empty name means the currently selected model. Never throws — resolves to
 * 0 when Ollama can't report it, so the caller can just hide the indicator.
 */
export async function getModelContextLength(name = '') {
    try {
        return await ModelContextLength(name);
    } catch (err) {
        console.warn('Could not read model context length:', err.message);
        return 0;
    }
}

/**
 * Return an array of registered chain-of-thought mode names ("none", "built-in",
 * plus one entry per markdown file in the cot directory).
 */
export async function listCotModes() {
    return await ListCotModes();
}

/**
 * Return the current active chain-of-thought mode.
 */
export async function getCotMode() {
    return await GetCotMode();
}

/**
 * Set the active chain-of-thought mode by name.
 */
export async function setCotMode(name) {
    await SetCotMode(name);
}

// --- File tools directory ---

/**
 * Open a native directory picker and, if the user chooses one, make it the
 * sandbox root for the file tools (list/read/write/update), enabling them.
 * Returns the resulting selected directory ("" if none is selected) — if the
 * user cancels the dialog, this is whatever was already selected before.
 */
export async function selectDirectory() {
    return await SelectDirectory();
}

/**
 * Deselect the current directory, disabling the file tools.
 */
export async function clearDirectory() {
    await ClearDirectory();
}

/**
 * Return the currently selected directory, or "" if none (file tools disabled).
 */
export async function getWorkDir() {
    return await GetWorkDir();
}

/**
 * Send a user message (optionally with history) and return every message
 * persisted this turn, in order: the user message, an optional "cot" note,
 * zero or more "tool" calls, and the final "assistant" reply.
 * @param {string} text - The user's message (what the model actually sees).
 * @param {{role:string,content:string}[]} [history=[]] - Prior pinned messages.
 * @param {string} [queuedByTool=''] - Name of the tool that queued this message
 *   (currently always 'manage_plan'), or '' if the user typed it themselves.
 *   Forces cot mode to 'none' for this turn and is persisted on the row.
 * @param {string} [displayText=''] - What's persisted/shown as this turn's
 *   user message in the chat log instead of `text` — used for a plan-driven
 *   turn, where `text` is the full prompt (current plan state folded in) but
 *   showing that in the log would just repeat the Plan tab's own checklist.
 *   Empty for a hand-typed turn, where there's nothing to shorten.
 * @returns {{messages: {seq:number,role:string,content:string,model:string,mode:string,pinned:boolean,toolName?:string,toolArgs?:string,toolResult?:string}[]}}
 */
export async function sendMessage(text, history = [], queuedByTool = '', displayText = '') {
    if (!text || !text.trim()) throw new Error('Empty message');
    return await SendChat(text, history, queuedByTool, displayText);
}

// --- Session management ---

/**
 * Create a new session on the backend and return its ID.
 */
export async function createSession() {
    return await CreateSession();
}

/**
 * Get the current active session ID from the backend.
 */
export async function getCurrentSession() {
    return await GetCurrentSession();
}

/**
 * Load all sessions ordered newest first.
 */
export async function loadSessions() {
    return await GetSessions();
}

/**
 * Switch to a different session by ID.
 */
export async function switchSession(id) {
    await SwitchSession(id);
}

/**
 * Delete a session and its messages. If it was the active session, a new one is created.
 */
export async function deleteSession(id) {
    await DeleteSession(id);
}

/**
 * Rename a session on the backend.
 */
export async function renameSession(id, title) {
    await RenameSession(id, title);
}

/**
 * Persist a single message to the database for the given session.
 * @param {string} sessionId - Session ID.
 * @param {string} role - 'user' or 'assistant'.
 * @param {string} content - Raw text content.
 */
export async function saveMessage(sessionId, role, content) {
    await SaveMessage(sessionId, role, content);
}

/**
 * Load all messages for a session from the database (for switching sessions).
 * @param {string} sessionId
 * @returns {{seq:number,role:string,content:string,model:string,mode:string,pinned:boolean,toolName:string,toolArgs:string,toolResult:string,time:string}[]}
 */
export async function loadMessages(sessionId) {
    return await GetMessages(sessionId);
}

/**
 * Toggle whether a message is included in the context sent to the model on
 * future turns. The message itself is never deleted — it stays visible and
 * in the database either way.
 * @param {string} sessionId
 * @param {number} seq
 * @param {boolean} pinned
 */
export async function setMessagePinned(sessionId, seq, pinned) {
    await SetMessagePinned(sessionId, seq, pinned);
}

// --- Plan ---

/**
 * Load the current plan for a session (see manage_plan), empty if none created yet.
 * @param {string} sessionId
 * @returns {{seq:number,content:string,status:string,updatedAt:string}[]}
 */
export async function getPlan(sessionId) {
    return await GetPlan(sessionId);
}

// --- Artifacts ---

/**
 * Load artifact metadata (no content) for a session, newest first.
 * @param {string} sessionId
 * @returns {{id:string,title:string,contentType:string,createdAt:string}[]}
 */
export async function getArtifacts(sessionId) {
    return await GetArtifacts(sessionId);
}

/**
 * Fetch the full content of a single artifact by ID.
 * @param {string} id
 * @returns {{id:string,sessionId:string,title:string,content:string,contentType:string,createdAt:string}}
 */
export async function getArtifactContent(id) {
    return await GetArtifactContent(id);
}

/**
 * Manually create an artifact under the current session (dev/manual affordance;
 * the model creates artifacts via its own create-artifact tool once wired in).
 * @param {string} title
 * @param {string} content
 * @param {string} contentType
 */
export async function createArtifactManual(title, content, contentType) {
    return await CreateArtifactManual(title, content, contentType);
}

/**
 * Open a native save dialog (defaulting to the artifact's own title as the
 * filename) and write its content to wherever the user chooses.
 * @param {string} id
 * @returns {string} The saved path, or "" if the user cancelled the dialog.
 */
export async function saveArtifact(id) {
    return await SaveArtifact(id);
}

/**
 * Open a native file picker and import the chosen file as a new artifact in
 * the given session. Content type is inferred from the file extension.
 * @param {string} sessionId
 * @returns {{id:string,title:string,contentType:string,createdAt:string}|null}
 *   The new artifact's metadata, or null if the user cancelled.
 */
export async function importArtifact(sessionId) {
    return await ImportArtifact(sessionId);
}

/**
 * Return the enabled/disabled state of every available tool for a session.
 * @param {string} sessionId
 * @returns {{name:string,enabled:boolean}[]}
 */
export async function getSessionToolStates(sessionId) {
    return await GetSessionToolStates(sessionId);
}

/**
 * Enable or disable a specific tool for a session.
 * @param {string} sessionId
 * @param {string} toolName
 * @param {boolean} enabled
 */
export async function setSessionToolEnabled(sessionId, toolName, enabled) {
    await SetSessionToolEnabled(sessionId, toolName, enabled);
}

// --- Memory ---

/**
 * Snapshot of the memory subsystem: whether embeddings are available (and why not,
 * when they aren't), the active token counter and extraction model, queue state,
 * corpus counts, edges per kind, and enrichment progress per state.
 * @returns {{embeddingsAvailable:boolean,unavailableReason:string,tokenCounter:string,
 *   extractModel:string,queue:{pending:number,running:string,completed:number,failed:number},
 *   corpus:{sources:number,chunks:number,embeddedChunks:number,terms:number,entities:number,
 *   edges:number,pendingEntities:number,avgDocLength:number,embedModel:string,embedDims:number},
 *   edgesByKind:Object<string,number>,entityPass:Object<string,number>}}
 */
export async function memoryStatus() {
    return await MemoryStatus();
}

/**
 * What the embedding model is, how big, where it goes, and whether it's already
 * there — so the UI can state the cost before asking for a 133 MB download.
 * @returns {{name:string,revision:string,bytes:number,targetDir:string,present:boolean}}
 */
export async function memoryModelInfo() {
    return await GetMemoryModelInfo();
}

/**
 * Download and verify the embedding model, then enable semantic search and embed
 * anything already indexed. Progress arrives on the `memory:provision` event.
 * User-initiated by design: nothing is fetched unless asked.
 */
export async function provisionMemoryModel() {
    return await ProvisionMemoryModel();
}

/**
 * Whether the memory subsystem has finished starting.
 *
 * Wails serves the UI while OnStartup is still running and building it — which loads a
 * 133 MB model — so a memory call made on page load can legitimately arrive too early.
 * This is the cheap check that distinguishes "not yet" from "broken".
 */
export async function memoryReady() {
    return await MemoryReady();
}

/**
 * Index the sessions and artifacts that already existed before memory did.
 *
 * Ingestion is event-driven — a turn completing, an artifact being created — so a
 * database with prior history has none of it indexed and nothing would ever replay it.
 * Idempotent: everything is content-hashed, so re-running skips what is unchanged.
 */
export async function indexExistingHistory() {
    return await IndexExistingHistory();
}

/**
 * How many non-empty sessions have no memory source yet, so the offer to backfill can
 * be hidden when there is nothing to backfill.
 */
export async function unindexedHistoryCount() {
    return await UnindexedHistoryCount();
}

/**
 * List everything indexed, newest first.
 * @returns {{id:string,sourceType:string,sourceRef:string,sessionId:string,title:string,
 *   path:string,contentHash:string,mtime:string,fileSize:number,ingestedAt:string,
 *   tokenCount:number,entityPass:string}[]}
 */
export async function memorySources() {
    return await MemorySources();
}

/**
 * Open a native picker and queue the chosen folder of Markdown notes for indexing.
 * Distinct from selectDirectory(), which sets the file tools' sandbox — indexing a
 * folder must not silently grant write access to it.
 * @returns {string} The chosen directory, or "" if cancelled.
 */
export async function selectMemoryDirectory() {
    return await SelectMemoryDirectory();
}

/**
 * Re-scan an already-indexed folder. Incremental: unchanged files are skipped from
 * their (mtime, size) without being read, and notes deleted on disk are removed.
 * @param {string} path
 */
export async function ingestDirectory(path) {
    return await IngestDirectory(path);
}

/**
 * Delete one indexed source and everything derived from it.
 * @param {string} sourceId
 */
export async function forgetMemorySource(sourceId) {
    await ForgetMemorySource(sourceId);
}

/**
 * Forget every indexed note under a folder. The unit the UI works in — a vault is
 * thousands of sources, so per-source deletion is not a usable undo.
 * @param {string} path
 * @returns {number} Sources removed.
 */
export async function forgetMemoryFolder(path) {
    return await ForgetMemoryFolder(path);
}

/**
 * Search memory, returning results plus the report explaining how they were found.
 *
 * tuning is optional; omit it for the defaults. Any nonzero weight means the caller
 * is driving all four. The report is what distinguishes "nothing matched" from
 * "embeddings unavailable", which a bare result list cannot.
 * @param {string} query
 * @param {number} [limit=8]
 * @param {{bm25?:number,vector?:number,entity?:number,ngram?:number,mode?:string,
 *   expand?:boolean|null,sourceTypes?:string[]}} [tuning={}]
 */
export async function searchMemory(query, limit = 8, tuning = {}) {
    const t = {
        bm25: tuning.bm25 || 0, vector: tuning.vector || 0,
        entity: tuning.entity || 0, ngram: tuning.ngram || 0,
        mode: tuning.mode || '',
        expand: tuning.expand === undefined ? null : tuning.expand,
        sourceTypes: tuning.sourceTypes || [],
    };
    return await SearchMemoryTuned(query, limit, t);
}

/**
 * The retrieval weights the system currently uses, so tuning controls start from
 * the real values rather than a copy that drifts.
 */
export async function defaultMemoryWeights() {
    return await DefaultMemoryWeights();
}

/**
 * Queue an edge rebuild. Needed when a corpus was indexed before the embedding
 * model was provisioned: its sequential and link edges exist but its similarity
 * graph does not.
 */
export async function rebuildMemoryEdges() {
    return await RebuildMemoryEdges();
}

/**
 * Queue the optional LLM entity pass. Off by default and unproven — see the README.
 * @param {number} [limit=0] Batch size; 0 uses the default.
 */
export async function enrichMemoryEntities(limit = 0) {
    return await EnrichMemoryEntities(limit);
}

/**
 * Remove every association the LLM entity tier produced, leaving the pattern-based
 * and tag tiers untouched, and mark each source pending again.
 * @returns {number} Associations removed.
 */
export async function rollbackMemoryEnrichment() {
    return await RollbackMemoryEntityEnrichment();
}
