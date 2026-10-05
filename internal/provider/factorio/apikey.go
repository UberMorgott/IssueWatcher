package factorio

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// The factorio.com API key the mod upload API takes (Authorization: Bearer,
// https://wiki.factorio.com/Mod_upload_API, usage «ModPortal: Upload Mods»).
// It lives in data\secrets\factorio-api.json (protected DACL + DPAPI) and is
// never shown: when none is stored, the app creates one on
// https://factorio.com/create-api-key with the signed-in session (the same
// form the owner would fill), so publishing needs no manual step.
const apiFile = "factorio-api.json"

// Account site and the key form.
const (
	accountSite = "https://factorio.com"
	createPath  = "/create-api-key"
	// UploadUsage is the label of the one usage the created key gets.
	UploadUsage = "ModPortal: Upload Mods"
	keyNote     = "IssueWatcher (mod upload)"
)

// ErrNoAPIKey: no API key is stored and none could be created (signed out).
var ErrNoAPIKey = errors.New("factorio: no API key")

// ErrBadAPIKey: the mod portal refused the key (InvalidApiKey).
var ErrBadAPIKey = errors.New("factorio: API key refused")

// KeysOptions configures Keys.
type KeysOptions struct {
	Dir  string       // data\secrets
	HTTP *http.Client // default http.DefaultClient
	Site string       // https://factorio.com override (tests)
	Now  func() time.Time
}

// Keys stores the factorio.com API key and creates it from a signed-in session.
type Keys struct {
	opts KeysOptions
	mu   sync.Mutex
}

type keyFile struct {
	APIKey    string    `json:"apiKey"`
	Source    string    `json:"source,omitempty"` // created | pasted
	Account   string    `json:"account,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// KeyStatus is the non-secret view.
type KeyStatus struct {
	HasAPIKey bool   `json:"hasApiKey"`
	Source    string `json:"source,omitempty"`
	Account   string `json:"account,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// NewKeys returns the key store.
func NewKeys(opts KeysOptions) *Keys {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.Site == "" {
		opts.Site = accountSite
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
	st := KeyStatus{HasAPIKey: f.APIKey != "", Source: f.Source, Account: f.Account}
	if !f.CreatedAt.IsZero() {
		st.CreatedAt = f.CreatedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Status reports whether a key is stored.
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
		return "", ErrNoAPIKey
	}
	return f.APIKey, nil
}

const maxKeyLen = 512

func validKey(key string) bool {
	return key != "" && len(key) <= maxKeyLen && strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r > '~' }) < 0
}

// Save stores a key (source "pasted" or "created"); "" removes it.
func (k *Keys) Save(key, source, account string) (KeyStatus, error) {
	key = strings.TrimSpace(key)
	k.mu.Lock()
	defer k.mu.Unlock()
	if key == "" {
		return KeyStatus{}, secret.Remove(k.path())
	}
	if !validKey(key) {
		return KeyStatus{}, fmt.Errorf("%w: unexpected characters", ErrBadAPIKey)
	}
	f := keyFile{APIKey: key, Source: source, Account: account, CreatedAt: k.opts.Now().UTC()}
	if err := secret.WriteProtectedJSON(k.path(), f); err != nil {
		return KeyStatus{}, err
	}
	return f.status(), nil
}

// The create-api-key form: its csrf token, the «ModPortal: Upload Mods»
// usage checkbox, and the one-time key on the answer page.
var (
	csrfRe    = regexp.MustCompile(`<input[^>]*name="csrf_token"[^>]*value="([^"]+)"`)
	usageRe   = regexp.MustCompile(`(?s)<input[^>]*name="usages"[^>]*value="([^"]+)"[^>]*>\s*(?:<div class="checkbox"></div>)?\s*<div>\s*([^<]+?)\s*</div>`)
	createdRe = regexp.MustCompile(`(?s)id="created_api_key"[^>]*>\s*<code>\s*([^<\s]+)\s*</code>`)
)

// parseCreateForm returns the csrf token and the usage value of UploadUsage.
func parseCreateForm(body string) (csrf, usage string, err error) {
	m := csrfRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return "", "", errors.New("factorio: create-api-key: no csrf token on the form")
	}
	csrf = html.UnescapeString(m[1])
	for _, u := range usageRe.FindAllStringSubmatch(body, -1) {
		if strings.EqualFold(strings.TrimSpace(html.UnescapeString(u[2])), UploadUsage) {
			return csrf, html.UnescapeString(u[1]), nil
		}
	}
	return "", "", fmt.Errorf("factorio: create-api-key: no %q usage on the form", UploadUsage)
}

// parseCreatedKey returns the key shown once after the form is sent.
func parseCreatedKey(body string) (string, error) {
	m := createdRe.FindStringSubmatch(body)
	if len(m) < 2 || !validKey(html.UnescapeString(m[1])) {
		return "", errors.New("factorio: create-api-key: the answer shows no key")
	}
	return html.UnescapeString(m[1]), nil
}

// Create makes a key with the UploadUsage usage on factorio.com with the
// signed-in session jar (the create-api-key form, sent once) and stores it.
// The key is shown by factorio.com only once: it is stored before returning.
func (k *Keys) Create(ctx context.Context, jar *websession.Jar) (KeyStatus, error) {
	if jar == nil || jar.Empty() {
		return KeyStatus{}, fmt.Errorf("%w: not signed in to factorio.com (Settings › Платформы › Factorio › Подключить)", ErrNoAPIKey)
	}
	u, err := url.Parse(k.opts.Site)
	if err != nil {
		return KeyStatus{}, err
	}
	cl := &websession.Client{Jar: jar, Hosts: []string{u.Hostname()}, HTTP: k.opts.HTTP}
	form := k.opts.Site + createPath
	res, err := cl.Do(ctx, http.MethodGet, form, nil, nil, false)
	if err != nil {
		return KeyStatus{}, fmt.Errorf("factorio: create-api-key: %w", err)
	}
	if res.Status != http.StatusOK || !strings.HasSuffix(strings.TrimRight(res.URL, "/"), createPath) {
		// Signed out: factorio.com sends the form to /login.
		return KeyStatus{}, fmt.Errorf("%w: factorio.com session signed out (HTTP %d, %s)", ErrNoAPIKey, res.Status, res.URL)
	}
	csrf, usage, err := parseCreateForm(string(res.Body))
	if err != nil {
		return KeyStatus{}, err
	}
	body := createBody(csrf, usage)
	h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Origin": {k.opts.Site}, "Referer": {form}}
	res, err = cl.Do(context.WithoutCancel(ctx), http.MethodPost, form, h, []byte(body), false)
	if err != nil {
		return KeyStatus{}, fmt.Errorf("factorio: create-api-key: %w", err)
	}
	if res.Status != http.StatusOK {
		return KeyStatus{}, fmt.Errorf("factorio: create-api-key: HTTP %d", res.Status)
	}
	key, err := parseCreatedKey(string(res.Body))
	if err != nil {
		return KeyStatus{}, err
	}
	return k.Save(key, "created", jar.Account())
}

// createBody is the exact create-api-key form body: the one usage, the note,
// the csrf token and the submit button's empty action.
func createBody(csrf, usage string) string {
	return url.Values{"usages": {usage}, "note": {keyNote}, "csrf_token": {csrf}, "action": {""}}.Encode()
}
