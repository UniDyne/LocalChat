package main

import "simple-cot-chat/store"

// GetArtifacts returns metadata for all artifacts belonging to a session, for
// display in the right sidebar.
func (a *App) GetArtifacts(sessionID string) ([]store.ArtifactMeta, error) {
	return a.sess.GetArtifactsForSession(sessionID)
}

// GetArtifactContent returns the full artifact record (including content), for
// preview or download.
func (a *App) GetArtifactContent(id string) (store.Artifact, error) {
	return a.sess.GetArtifact(id)
}

// CreateArtifactManual persists a new artifact under the current session.
// This is the manual/dev-facing counterpart to the create-artifact tool the
// model will use once tool-calling is wired in.
func (a *App) CreateArtifactManual(title, content, contentType string) (string, error) {
	return a.sess.CreateArtifact(a.sess.CurrentSession(), title, content, contentType)
}
