import { ListModels, SendChat, GetModel, SetModel } from '../wailsjs/go/main/App';
import { ListCotModes, GetCotMode, SetCotMode } from '../wailsjs/go/main/App';
import { CreateSession, GetCurrentSession, GetSessions, SwitchSession, DeleteSession, RenameSession, GetMessages } from '../wailsjs/go/main/App';

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

/**
 * Send a user message (optionally with history) and return the assistant reply.
 * @param {string} text - The user's message.
 * @param {{role:string,content:string}[]} [history=[]] - Prior messages.
 */
export async function sendMessage(text, history = []) {
    if (!text || !text.trim()) throw new Error('Empty message');
    return await SendChat(text, history);
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
 * @returns {{role:string,content:string,time:string}[]}
 */
export async function loadMessages(sessionId) {
    return await GetMessages(sessionId);
}
