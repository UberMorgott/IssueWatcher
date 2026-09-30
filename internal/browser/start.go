package browser

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// startBrowser launches exe over the DevTools pipe, else (an old build that
// ignores --remote-debugging-io-pipes) over a loopback port.
func startBrowser(exe string, args []string) (*process, error) {
	p, err := startPipe(exe, args)
	if err == nil {
		if err = probe(p); err == nil {
			return p, nil
		}
		p.kill()
	}
	pp, perr := startPort(exe, args)
	if perr != nil {
		return nil, fmt.Errorf("browser: pipe: %w; port: %w", err, perr)
	}
	return pp, nil
}

// probe checks that the pipe answers (Browser.getVersion within 20 s),
// before any Conn reads it.
func probe(p *process) error {
	if err := p.t.Write([]byte(`{"id":1,"method":"Browser.getVersion"}`)); err != nil {
		return err
	}
	got := make(chan error, 1)
	go func() {
		b, err := p.t.Read()
		if err == nil && !strings.Contains(string(b), `"id":1`) {
			err = errors.New("browser: unexpected first message")
		}
		got <- err
	}()
	select {
	case err := <-got:
		return err
	case <-time.After(20 * time.Second):
		return errors.New("browser: no answer on the DevTools pipe")
	}
}

// startPort is the fallback: --remote-debugging-port=0 (loopback only, origin
// pinned) and the websocket from DevToolsActivePort.
func startPort(exe string, args []string) (*process, error) {
	dir := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--user-data-dir="); ok {
			dir = v
		}
	}
	if dir == "" {
		return nil, errors.New("browser: no profile dir")
	}
	portFile := filepath.Join(dir, "DevToolsActivePort")
	_ = os.Remove(portFile)
	args = append(args, "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--remote-allow-origins=http://127.0.0.1")
	proc, err := startPlain(exe, args)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if f, err := os.Open(portFile); err == nil { //nolint:gosec // G304: our own profile dir
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			_ = f.Close()
			if len(lines) >= 2 && lines[0] != "" {
				t, err := DialWebSocket("ws://127.0.0.1:" + lines[0] + lines[1])
				if err != nil {
					proc.kill()
					return nil, err
				}
				proc.t = t
				return proc, nil
			}
		}
		select {
		case <-proc.exited:
			proc.kill()
			return nil, errors.New("browser: exited before DevTools came up")
		case <-time.After(200 * time.Millisecond):
		}
	}
	proc.kill()
	return nil, errors.New("browser: DevTools port did not come up")
}
