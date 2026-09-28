package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

// ErrBadCursor is returned for a cursor this store did not issue.
var ErrBadCursor = errors.New("store: bad cursor")

// Keyset cursors are opaque to clients: base64url(JSON [sortValue, id]). The
// sort value is a string (timestamps, names) or a number (counts).
type cursorKey struct {
	Value any
	ID    int64
}

func encodeCursor(value any, id int64) string {
	b, _ := json.Marshal([]any{value, id}) // string/int values cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursorKey, error) {
	var k cursorKey
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return k, ErrBadCursor
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 2 {
		return k, ErrBadCursor
	}
	if err := json.Unmarshal(raw[1], &k.ID); err != nil {
		return k, ErrBadCursor
	}
	var str string
	var num float64
	switch {
	case json.Unmarshal(raw[0], &str) == nil:
		k.Value = str
	case json.Unmarshal(raw[0], &num) == nil:
		k.Value = num
	default:
		return k, ErrBadCursor
	}
	return k, nil
}

// clampLimit applies the default (50) and the maximum (200) chunk size.
func clampLimit(n int) int {
	if n <= 0 {
		return 50
	}
	return min(n, 200)
}
