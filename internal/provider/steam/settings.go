package steam

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// settingsFile holds the Steam account settings in data\secrets (protected
// DACL + DPAPI, docs/ARCHITECTURE.md → Mod platforms › Sessions and secrets).
// The Web API key and the web cookies never go to config.json or the log.
const settingsFile = "steam.json"

// Settings is what the user enters in Settings › Платформы › Steam.
type Settings struct {
	SteamID string `json:"steamId"`         // SteamID64 of the workshop owner
	AppID   int    `json:"appId,omitempty"` // limit the owner's items to one game (0 = all)
	APIKey  string `json:"apiKey,omitempty"`
	// Web session cookies for posting (steamcommunity.com).
	LoginSecure string `json:"steamLoginSecure,omitempty"`
	SessionID   string `json:"sessionid,omitempty"`
	// SessionExpired: Steam refused the cookies; the user must paste new ones.
	SessionExpired bool      `json:"sessionExpired,omitempty"`
	CheckedAt      time.Time `json:"checkedAt,omitzero"`
}

// Session states reported by Status.
const (
	SessionNone    = "none"    // no cookies stored: read-only
	SessionStored  = "stored"  // cookies stored, not yet refused
	SessionExpired = "expired" // Steam refused them: re-login needed
)

// Status is the non-secret view of Settings (GET /api/providers/steam).
type Status struct {
	Configured bool   `json:"configured"`
	SteamID    string `json:"steamId"`
	AppID      int    `json:"appId"`
	HasAPIKey  bool   `json:"hasApiKey"`
	HasCookies bool   `json:"hasCookies"`
	Session    string `json:"session"` // none | stored | expired
	CheckedAt  string `json:"checkedAt,omitempty"`
}

func (s Settings) status() Status {
	st := Status{
		Configured: s.SteamID != "", SteamID: s.SteamID, AppID: s.AppID,
		HasAPIKey: s.APIKey != "", HasCookies: s.LoginSecure != "" && s.SessionID != "",
		Session: SessionNone,
	}
	switch {
	case st.HasCookies && s.SessionExpired:
		st.Session = SessionExpired
	case st.HasCookies:
		st.Session = SessionStored
	}
	if !s.CheckedAt.IsZero() {
		st.CheckedAt = s.CheckedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Update changes Settings; nil fields stay as they are, "" clears.
type Update struct {
	SteamID     *string `json:"steamId"`
	AppID       *int    `json:"appId"`
	APIKey      *string `json:"apiKey"`
	LoginSecure *string `json:"steamLoginSecure"`
	SessionID   *string `json:"sessionid"`
}

// ErrBadSettings is a validation failure of an Update.
var ErrBadSettings = errors.New("steam: bad settings")

var (
	steamID64Re  = regexp.MustCompile(`^7656119\d{10}$`)
	profileURLRe = regexp.MustCompile(`^https?://steamcommunity\.com/profiles/(7656119\d{10})/?$`)
	tokenRe      = regexp.MustCompile(`^[A-Za-z0-9%|._\-]+$`)
	apiKeyRe     = regexp.MustCompile(`^[0-9A-Fa-f]{32}$`)
)

// NormalizeSteamID accepts a SteamID64 or a /profiles/<id64> URL.
func NormalizeSteamID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if steamID64Re.MatchString(s) {
		return s, true
	}
	if m := profileURLRe.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

func (s Settings) apply(u Update) (Settings, error) {
	if u.SteamID != nil {
		if *u.SteamID == "" {
			s.SteamID = ""
		} else {
			id, ok := NormalizeSteamID(*u.SteamID)
			if !ok {
				return s, fmt.Errorf("%w: steamId must be a SteamID64 (7656119…) or a steamcommunity.com/profiles/<id> URL", ErrBadSettings)
			}
			s.SteamID = id
		}
	}
	if u.AppID != nil {
		if *u.AppID < 0 {
			return s, fmt.Errorf("%w: appId", ErrBadSettings)
		}
		s.AppID = *u.AppID
	}
	if u.APIKey != nil {
		k := strings.TrimSpace(*u.APIKey)
		if k != "" && !apiKeyRe.MatchString(k) {
			return s, fmt.Errorf("%w: apiKey must be 32 hex characters", ErrBadSettings)
		}
		s.APIKey = k
	}
	cookies := false
	for _, f := range []struct {
		in  *string
		out *string
	}{{u.LoginSecure, &s.LoginSecure}, {u.SessionID, &s.SessionID}} {
		if f.in == nil {
			continue
		}
		v := strings.TrimSpace(*f.in)
		if v != "" && (len(v) > 4096 || !tokenRe.MatchString(v)) {
			return s, fmt.Errorf("%w: cookie values contain unexpected characters", ErrBadSettings)
		}
		*f.out, cookies = v, true
	}
	if cookies {
		s.SessionExpired, s.CheckedAt = false, time.Time{}
	}
	return s, nil
}

func loadSettings(dir string) (Settings, error) {
	var s Settings
	err := secret.ReadProtectedJSON(filepath.Join(dir, settingsFile), &s)
	if errors.Is(err, secret.ErrNotFound) {
		return Settings{}, nil
	}
	return s, err
}

func saveSettings(dir string, s Settings) error {
	if s == (Settings{}) {
		return secret.Remove(filepath.Join(dir, settingsFile))
	}
	return secret.WriteProtectedJSON(filepath.Join(dir, settingsFile), s)
}
