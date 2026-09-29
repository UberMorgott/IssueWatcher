package mcpbridge

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
)

// maxStderrLine bounds one logged server line (a stack trace is cut, not buffered forever).
const maxStderrLine = 2000

// stderrLog writes the server's stderr into the app log, one record per line:
// sign-in steps ("[login] …") at info, failures at warn, the rest at debug.
type stderrLog struct {
	log    *slog.Logger
	server string

	mu  sync.Mutex
	buf []byte
}

func (w *stderrLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > maxStderrLine {
				w.emit(string(w.buf[:maxStderrLine]))
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

func (w *stderrLog) emit(line string) {
	line = strings.TrimSpace(line)
	if line == "" || w.log == nil {
		return
	}
	if len(line) > maxStderrLine {
		line = line[:maxStderrLine] + "…"
	}
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(line, "[login]"):
		w.log.Info("mcp: "+line, "server", w.server)
	case strings.Contains(lower, "error") || strings.Contains(lower, "fail"):
		w.log.Warn("mcp: "+line, "server", w.server)
	default:
		w.log.Debug("mcp: "+line, "server", w.server)
	}
}
