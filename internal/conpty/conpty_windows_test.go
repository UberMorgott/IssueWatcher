//go:build windows

package conpty

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the console child: CONPTY_CHILD=1 reads two lines from
// the console and echoes them.
func TestMain(m *testing.M) {
	if os.Getenv("CONPTY_CHILD") == "1" {
		buf := make([]byte, 0, 64)
		one := make([]byte, 1)
		lines := 0
		say := func(s string) {
			if _, err := os.Stdout.WriteString(s); err != nil {
				os.Exit(2)
			}
		}
		say("prompt: ")
		for lines < 2 {
			n, err := os.Stdin.Read(one)
			if err != nil {
				break
			}
			if n == 1 && (one[0] == '\r' || one[0] == '\n') {
				if len(buf) > 0 {
					say("\ngot <" + string(buf) + ">\n")
					buf = buf[:0]
					lines++
				}
				continue
			}
			buf = append(buf, one[:n]...)
		}
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func TestConsoleInputReachesChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(exe, []string{"-test.run=^$"}, Options{Env: append(os.Environ(), "CONPTY_CHILD=1")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, p.Output()); close(done) }()
	time.Sleep(300 * time.Millisecond)
	_, _ = p.Write([]byte("hello\r"))
	_, _ = p.Write([]byte("secret word\r"))
	code, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	<-done
	got := Plain(out.String())
	if code != 7 || !strings.Contains(got, "got <hello>") || !strings.Contains(got, "got <secret word>") {
		t.Fatalf("exit %d, output %q", code, got)
	}
}

// TestRealSteamCMD (opt-in: CONPTY_STEAMCMD=<steamcmd.exe>) types "info" and
// "quit" into a real steamcmd; no login.
func TestRealSteamCMD(t *testing.T) {
	exe := os.Getenv("CONPTY_STEAMCMD")
	if exe == "" {
		t.Skip("CONPTY_STEAMCMD not set")
	}
	if _, err := exec.LookPath(exe); err != nil {
		t.Skip(err)
	}
	p, err := Start(exe, nil, Options{Dir: strings.TrimSuffix(exe, `\steamcmd.exe`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, p.Output()); close(done) }()
	time.Sleep(15 * time.Second)
	_, _ = p.Write([]byte("quit\r"))
	exited := make(chan int, 1)
	go func() { c, _ := p.Wait(); exited <- c }()
	select {
	case c := <-exited:
		t.Logf("exit %d", c)
	case <-time.After(60 * time.Second):
		_ = p.Kill()
		t.Fatalf("steamcmd ignored console input: %q", Plain(out.String()))
	}
	<-done
	t.Logf("output: %q", Plain(out.String()))
}
