package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/signin"
)

// Platform errors reach the dashboard as codes it has its own text for; the
// raw Go message is never the only thing shown.
func TestPlatformErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{curseforge.ErrRelogin, CodeRelogin},
		{steam.ErrSessionExpired, CodeRelogin},
		{fmt.Errorf("nexus: %w", provider.ErrNotSignedIn), CodeNotSignedIn},
		{fmt.Errorf("start: %w", browser.ErrNoBrowser), CodeNoBrowser},
		{browser.ErrChallenge, CodeChallenge},
		{fmt.Errorf("cdp: %w", browser.ErrClosed), CodeBrowser},
		{ErrBusy, CodeBusy},
		{fmt.Errorf("nexus: %w", ErrSwitchedOff), CodeSwitchedOff},
		{context.DeadlineExceeded, CodeNetwork},
		{errors.New(`Get "https://x": dial tcp: lookup x: no such host`), CodeNetwork},
		{errors.New("something odd"), CodeFailed},
	} {
		if got := ErrorCode(c.err); got != c.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	// A sync source keeps only the message.
	if got := ReasonCode("sync: " + curseforge.ErrRelogin.Error()); got != CodeRelogin {
		t.Errorf("ReasonCode relogin = %q", got)
	}
	if got := ReasonCode("HTTP 500"); got != "" {
		t.Errorf("ReasonCode unknown = %q", got)
	}
	for detail, want := range map[string]string{
		"":                                 "",
		signin.DetailCancelled:             CodeCancelled,
		signin.DetailTimeout:               CodeTimeout,
		signin.DetailWindowClosed:          CodeWindowClosed,
		signin.DetailBusy + ": curseforge": CodeWindowBusy,
		"weird":                            CodeFailed,
	} {
		if got := LoginDetailCode(detail); got != want {
			t.Errorf("LoginDetailCode(%q) = %q, want %q", detail, got, want)
		}
	}
	for msg, want := range map[string]string{
		"steam: bad settings: steamId must be a SteamID64": "bad_steam_id",
		"steam: bad settings: apiKey must be 32 hex":       "bad_api_key",
		"steam: bad settings: cookie values contain":       "bad_cookies",
		"steam: bad settings: appId":                       "bad_settings",
	} {
		if got := steamSettingsCode(msg); got != want {
			t.Errorf("steamSettingsCode(%q) = %q, want %q", msg, got, want)
		}
	}
}
