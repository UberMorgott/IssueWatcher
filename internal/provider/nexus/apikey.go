package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// The Nexus personal API key (Settings › Платформы › Nexus) is what the v3
// upload API takes (header apikey, openapi.yaml ApiKeyAuth). It lives in
// data\secrets\nexus-api.json (protected DACL + DPAPI) and is write-only: the
// dashboard sees only whether one is stored and the account it belongs to.
const apiFile = "nexus-api.json"

// Endpoints of the official API.
const (
	apiV1 = "https://api.nexusmods.com/v1"
	apiV3 = "https://api.nexusmods.com/v3"
	// AppName is the Application-Name header Nexus asks every API client to send.
	AppName = "IssueWatcher"
)

// ErrNoAPIKey: no API key is stored (Settings › Платформы › Nexus).
var ErrNoAPIKey = errors.New("nexus: no API key")

// ErrBadAPIKey: the key was refused by users/validate (or is malformed).
var ErrBadAPIKey = errors.New("nexus: API key refused")

// The owner-facing hints: the key is set only in Settings › Платформы › Nexus.
const (
	hintNoKey  = "ключ Nexus API не задан: вставьте его в Настройки › Платформы › Nexus"
	hintBadKey = "Nexus отклонил ключ API: замените его в Настройки › Платформы › Nexus"
)

// errNoKey is ErrNoAPIKey with the hint where to set the key.
var errNoKey = fmt.Errorf("%w (%s)", ErrNoAPIKey, hintNoKey)

// badKeyErr is ErrBadAPIKey for an HTTP status, with the hint.
func badKeyErr(status int) error {
	return fmt.Errorf("%w: HTTP %d (%s)", ErrBadAPIKey, status, hintBadKey)
}

// KeysOptions configures Keys.
type KeysOptions struct {
	Dir     string       // data\secrets
	HTTP    *http.Client // default http.DefaultClient
	V1      string       // v1 base URL override (tests)
	Version string       // Application-Version header (the app version)
	Now     func() time.Time
}

// Keys stores and validates the Nexus API key.
type Keys struct {
	opts KeysOptions
	mu   sync.Mutex
}

// keyFile is the stored form.
type keyFile struct {
	APIKey    string    `json:"apiKey"`
	User      string    `json:"user,omitempty"`
	UserID    int       `json:"userId,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// KeyStatus is the non-secret view (GET /api/providers/nexus).
type KeyStatus struct {
	HasAPIKey bool   `json:"hasApiKey"`
	User      string `json:"user,omitempty"` // account name users/validate returned
	UserID    int    `json:"userId,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

// NewKeys returns the key store.
func NewKeys(opts KeysOptions) *Keys {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.V1 == "" {
		opts.V1 = apiV1
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Keys{opts: opts}
}

func (k *Keys) path() string { return filepath.Join(k.opts.Dir, apiFile) }

func (k *Keys) load() (keyFile, error) {
	var f keyFile
	err := secret.ReadProtectedJSON(k.path(), &f)
	if errors.Is(err, secret.ErrNotFound) {
		return keyFile{}, nil
	}
	return f, err
}

func (f keyFile) status() KeyStatus {
	st := KeyStatus{HasAPIKey: f.APIKey != "", User: f.User, UserID: f.UserID}
	if !f.CheckedAt.IsZero() {
		st.CheckedAt = f.CheckedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Status reports whether a key is stored and whose it is.
func (k *Keys) Status() (KeyStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f, err := k.load()
	return f.status(), err
}

// Key is the stored key (ErrNoAPIKey when none). Never log it.
func (k *Keys) Key() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f, err := k.load()
	if err != nil {
		return "", err
	}
	if f.APIKey == "" {
		return "", errNoKey
	}
	return f.APIKey, nil
}

// maxKeyLen bounds a pasted key (real ones are a few hundred characters).
const maxKeyLen = 4096

// Save validates key with users/validate and stores it with the account it
// names; "" removes the stored key. A refused key is not stored (ErrBadAPIKey);
// a failed check (network, rate limit) stores nothing either.
func (k *Keys) Save(ctx context.Context, key string) (KeyStatus, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		k.mu.Lock()
		defer k.mu.Unlock()
		return KeyStatus{}, secret.Remove(k.path())
	}
	if len(key) > maxKeyLen || strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return KeyStatus{}, fmt.Errorf("%w: unexpected characters", ErrBadAPIKey)
	}
	u, err := k.validate(ctx, key)
	if err != nil {
		return KeyStatus{}, err
	}
	f := keyFile{APIKey: key, User: u.Name, UserID: u.UserID, CheckedAt: k.opts.Now().UTC()}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := secret.WriteProtectedJSON(k.path(), f); err != nil {
		return KeyStatus{}, err
	}
	return f.status(), nil
}

// Check re-validates the stored key («Проверить») and records the account.
func (k *Keys) Check(ctx context.Context) (KeyStatus, error) {
	key, err := k.Key()
	if err != nil {
		return KeyStatus{}, err
	}
	u, err := k.validate(ctx, key)
	if err != nil {
		return KeyStatus{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	f, err := k.load()
	if err != nil {
		return KeyStatus{}, err
	}
	if f.APIKey != key { // replaced meanwhile: report the new one unchanged
		return f.status(), nil
	}
	f.User, f.UserID, f.CheckedAt = u.Name, u.UserID, k.opts.Now().UTC()
	if err := secret.WriteProtectedJSON(k.path(), f); err != nil {
		return KeyStatus{}, err
	}
	return f.status(), nil
}

// validUser is the part of GET /v1/users/validate this app reads.
type validUser struct {
	UserID int    `json:"user_id"`
	Name   string `json:"name"`
}

func (k *Keys) validate(ctx context.Context, key string) (validUser, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(k.opts.V1, "/")+"/users/validate", nil)
	if err != nil {
		return validUser{}, err
	}
	setAPIHeaders(req.Header, key, k.opts.Version)
	res, err := k.opts.HTTP.Do(req)
	if err != nil {
		return validUser{}, fmt.Errorf("nexus: users/validate: %w", redactErr(err, key))
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return validUser{}, badKeyErr(res.StatusCode)
	case res.StatusCode != http.StatusOK:
		return validUser{}, fmt.Errorf("nexus: users/validate: HTTP %d: %s", res.StatusCode, redact(snippet(body), key))
	}
	var u validUser
	if err := json.Unmarshal(body, &u); err != nil || u.Name == "" {
		return validUser{}, fmt.Errorf("nexus: users/validate: unexpected answer")
	}
	return u, nil
}

// setAPIHeaders sets the official API's headers: the key and the
// Application-Name / -Version Nexus asks every client to send.
func setAPIHeaders(h http.Header, key, version string) {
	h.Set("Accept", "application/json")
	h.Set("Application-Name", AppName)
	h.Set("Application-Version", version)
	h.Set("User-Agent", AppName+"/"+version)
	h.Set("apikey", key)
}

// redact removes the key from text bound for an error or the log.
func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// redactErr keeps the key out of a transport error's text.
func redactErr(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(redact(err.Error(), key))
}
