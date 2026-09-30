package mcpbridge

import (
	"errors"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// WriteUnsure reports whether a write tool's error leaves the outcome open (the
// caller reads back, never retries): a lost answer, the server's
// outcome_unknown, or an unclassified tool error. The servers raise the other
// codes (not_logged_in, cloudflare, invalid, disabled, not_found,
// rate_limited) for refusals: nothing was saved.
func WriteUnsure(err error) bool {
	if errors.Is(err, ErrOutcomeUnknown) {
		return true
	}
	te, ok := errors.AsType[*ToolError](err)
	return ok && (te.Code == CodeOutcomeUnknown || te.Code == CodeError)
}

// ProviderError maps a bridge error to the provider contract: not_logged_in →
// provider.ErrNotSignedIn, rate_limited → *provider.RateLimitError (retry in
// 15 min), everything else wrapped as is.
func ProviderError(err error, now time.Time) error {
	if err == nil {
		return nil
	}
	switch {
	case IsCode(err, CodeNotLoggedIn):
		return fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
	case IsCode(err, CodeRateLimited):
		return &provider.RateLimitError{Reset: now.Add(15 * time.Minute)}
	}
	return err
}

// State is a mod platform's connection state (Settings › Платформы).
type State string

// Connection states.
const (
	StateUnavailable State = "unavailable" // server missing / not starting
	StateSignedOut   State = "signed_out"
	StateConnected   State = "connected"
	StateError       State = "error" // session expired, Cloudflare, …
)
