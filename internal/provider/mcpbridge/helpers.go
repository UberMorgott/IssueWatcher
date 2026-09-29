package mcpbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Title is the first non-empty line of body, at most 80 characters.
func Title(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > 80 {
			r := []rune(line)
			return strings.TrimSpace(string(r[:79])) + "…"
		}
		return line
	}
	return "(empty)"
}

// Time parses an ISO-8601 timestamp; nil or bad → zero.
func Time(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Number is a numeric id as an int (0 when not numeric).
func Number(id string) int {
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0
	}
	return n
}

// Latest returns the latest of ts.
func Latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// Signature hashes parts (a page-1 fingerprint for the change check).
func Signature(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

// PageChanged stores sig as the page-1 fingerprint under key and reports
// whether the project must be reconciled: a different fingerprint, none yet
// with no full read on record (a comment may have landed since), or the last
// full read (st.FullAt) longer ago than every (a reply on a thread past page 1
// may not change page 1; 0 = never). A fingerprint missing or taken before the
// last full read is adopted as the new baseline instead: that read (the
// syncer's reconcile) already stored everything, so no second full read
// follows it. Asking for a reconcile stamps FullAt (the syncer restores the
// state when the reconcile fails).
func PageChanged(st *provider.PollState, key, sig string, now time.Time, every time.Duration) bool {
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	prev := st.ETags[key]
	st.ETags[key] = sig
	full := st.FullAt
	if last, err := strconv.ParseInt(st.ETags[key+"@full"], 10, 64); err == nil && full.IsZero() {
		full = time.Unix(last, 0) // state written before FullAt
	}
	delete(st.ETags, key+"@full")
	sigAt, sigErr := strconv.ParseInt(st.ETags[key+"@at"], 10, 64)
	st.ETags[key+"@at"] = strconv.FormatInt(now.Unix(), 10)
	due := every > 0 && now.Sub(full) >= every
	switch {
	case due:
	case prev != "" && prev == sig:
		return false
	case !full.IsZero() && (prev == "" || (sigErr == nil && time.Unix(sigAt, 0).Before(full))):
		return false // the fingerprint predates the last full read: adopt it
	}
	st.FullAt = now
	return true
}

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

// HTTPS returns u when it is an absolute https URL, else fallback: a page URL
// from a third party (CFWidget, a server) ends up in the dashboard's href, where
// a javascript: URL would run.
func HTTPS(u, fallback string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || !strings.EqualFold(p.Scheme, "https") || p.Host == "" {
		return fallback
	}
	return p.String()
}

// SameText compares two bodies ignoring whitespace differences (read-back).
func SameText(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
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
