// Package redact strips secrets (one-time sign-in tokens, OAuth codes, API
// keys, cookies, bearer tokens) from text before it reaches a log.
package redact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// Mask replaces a redacted value.
const Mask = "***"

var (
	// Query parameters that carry secrets: t is the one-time /auth?t= token.
	queryRe = regexp.MustCompile(`(?i)([?&](?:t|token|code|key|api_?key|access_token|refresh_token|id_token|client_secret|secret|password|sig|signature|session|sessionid)=)[^&\s"'#<>]+`)
	// Authorization: Bearer <token> in free-form text.
	bearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// Cookie headers: cookie: a=b; c=d.
	cookieRe = regexp.MustCompile(`(?i)\b((?:set-)?cookie:\s*)[^\r\n]+`)
)

// String masks secret query parameters, bearer tokens and cookie headers in s.
func String(s string) string {
	if l := strings.ToLower(s); !strings.ContainsAny(s, "?&") && !strings.Contains(l, "bearer") && !strings.Contains(l, "cookie") {
		return s // fast path: nothing to mask
	}
	s = queryRe.ReplaceAllString(s, "${1}"+Mask)
	s = bearerRe.ReplaceAllString(s, "${1} "+Mask)
	return cookieRe.ReplaceAllString(s, "${1}"+Mask)
}

// secretKeys are attribute keys whose whole value is a secret.
var secretKeys = map[string]bool{
	"token": true, "access_token": true, "refresh_token": true, "id_token": true,
	"api_key": true, "apikey": true, "secret": true, "client_secret": true, "password": true,
	"cookie": true, "cookies": true, "authorization": true,
}

// Attr redacts one attribute: secret keys lose the value, strings, errors and
// Stringers are masked by String, groups recursively.
func Attr(a slog.Attr) slog.Attr {
	if secretKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, Mask)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindGroup:
		g := v.Group()
		out := make([]slog.Attr, len(g))
		for i, x := range g {
			out[i] = Attr(x)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, String(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, String(x.String()))
		}
	default: // numbers, times, durations: nothing to mask
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// File rewrites the log at path with secrets masked, if it holds any: logs
// from older versions carried the one-time sign-in URL in full. A missing
// file is fine.
func File(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the caller's own log file
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	clean := String(string(b))
	if clean == string(b) {
		return nil
	}
	tmp := path + ".redact"
	if err := os.WriteFile(tmp, []byte(clean), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Handler masks secrets in every message and attribute before h sees them.
type Handler struct{ h slog.Handler }

// NewHandler wraps h.
func NewHandler(h slog.Handler) *Handler { return &Handler{h: h} }

// Enabled implements slog.Handler.
func (r *Handler) Enabled(ctx context.Context, l slog.Level) bool { return r.h.Enabled(ctx, l) }

// Handle implements slog.Handler.
func (r *Handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(Attr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (r *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = Attr(a)
	}
	return &Handler{h: r.h.WithAttrs(out)}
}

// WithGroup implements slog.Handler.
func (r *Handler) WithGroup(name string) slog.Handler { return &Handler{h: r.h.WithGroup(name)} }
