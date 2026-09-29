package mcpbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
