package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// sessionFile keeps the browser session secret across app restarts, so the
// dashboard tab and the installed app window stay signed in (the runtime
// bearer token in runtime.json still changes every run).
const sessionFile = "session.json"

type sessionSecret struct {
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"createdAt"`
}

// loadSession returns the persistent session secret from data\secrets,
// creating it on first use. A damaged file is replaced (everyone signs in again).
func loadSession(secretsDir string) (string, error) {
	path := filepath.Join(secretsDir, sessionFile)
	var s sessionSecret
	err := secret.ReadJSON(path, &s)
	if err == nil && validSecret(s.Secret) {
		return s.Secret, nil
	}
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		// Unreadable: fall through and replace it.
		_ = secret.Remove(path)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	s = sessionSecret{Secret: hex.EncodeToString(b), CreatedAt: time.Now().UTC()}
	if err := secret.WriteJSON(path, s); err != nil {
		return "", err
	}
	return s.Secret, nil
}

func validSecret(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
