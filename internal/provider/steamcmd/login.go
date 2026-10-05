package steamcmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sign-in states (LoginState.State).
const (
	LoginIdle      = "idle"
	LoginStarting  = "starting"       // steamcmd starts (the first run updates it)
	LoginNeedCode  = "need_code"      // steamcmd asks for the Steam Guard code
	LoginConfirm   = "confirm_mobile" // waiting for the approval in the Steam Mobile app
	LoginOK        = "ok"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
)

// LoginState is a sign-in's progress.
type LoginState struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	At    string `json:"at,omitempty"`
}

// ErrBadLogin: the form's values are malformed (nothing was started).
var ErrBadLogin = errors.New("steam: bad sign-in values")

var (
	userRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{2,64}$`)
	codeRe = regexp.MustCompile(`^[A-Za-z0-9]{5,8}$`)
)

type loginRun struct {
	mu   sync.Mutex
	st   LoginState
	proc Proc
	code chan string
}

func (l *loginRun) state() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *loginRun) set(state, msg string, now time.Time) {
	l.mu.Lock()
	l.st = LoginState{State: state, Error: msg, At: now.UTC().Format(time.RFC3339)}
	l.mu.Unlock()
}

// StartLogin signs steamcmd in with the owner's values from the form: the
// user goes on the command line, the password and the Steam Guard code
// (optional: asked for later when steamcmd wants one) are typed into its
// console. Nothing of them is stored or logged. Poll Status().Login.
func (w *Workshop) StartLogin(user, password, code string) (LoginState, error) {
	user, code = strings.TrimSpace(user), strings.TrimSpace(code)
	switch {
	case !userRe.MatchString(user):
		return LoginState{}, fmt.Errorf("%w: Steam account name", ErrBadLogin)
	case password == "" || len(password) > 256 || strings.ContainsAny(password, "\r\n\x00"):
		return LoginState{}, fmt.Errorf("%w: password", ErrBadLogin)
	case code != "" && !codeRe.MatchString(code):
		return LoginState{}, fmt.Errorf("%w: Steam Guard code", ErrBadLogin)
	}
	exe, _, err := w.steamcmd()
	if err != nil {
		return LoginState{}, err
	}
	if !w.run.TryLock() {
		return LoginState{}, ErrBusy
	}
	p, err := w.opts.Run(exe, []string{"+login", user}, filepath.Dir(exe))
	if err != nil {
		w.run.Unlock()
		return LoginState{}, fmt.Errorf("steam: start steamcmd: %w", err)
	}
	l := &loginRun{proc: p, code: make(chan string, 1)}
	l.set(LoginStarting, "", w.opts.Now())
	w.mu.Lock()
	w.cur = l
	w.mu.Unlock()
	go func() {
		defer w.run.Unlock()
		w.drive(l, user, password, code)
		_ = p.Close()
		w.mu.Lock()
		w.last, w.cur = l.state(), nil
		w.mu.Unlock()
	}()
	return l.state(), nil
}

// SubmitCode types the Steam Guard code a running sign-in asks for.
func (w *Workshop) SubmitCode(code string) (LoginState, error) {
	code = strings.TrimSpace(code)
	if !codeRe.MatchString(code) {
		return LoginState{}, fmt.Errorf("%w: Steam Guard code", ErrBadLogin)
	}
	w.mu.Lock()
	l := w.cur
	w.mu.Unlock()
	if l == nil || l.state().State != LoginNeedCode {
		return LoginState{}, errors.New("steam: no sign-in is waiting for a code")
	}
	select {
	case l.code <- code:
	default:
	}
	return l.state(), nil
}

// CancelLogin stops a running sign-in.
func (w *Workshop) CancelLogin() {
	w.mu.Lock()
	l := w.cur
	w.mu.Unlock()
	if l != nil {
		l.set(LoginCancelled, "", w.opts.Now())
		_ = l.proc.Kill()
	}
}

// drive answers steamcmd's prompts until it is signed in, fails or times out.
func (w *Workshop) drive(l *loginRun, user, password, code string) {
	c := readConsole(l.proc)
	deadline := time.NewTimer(w.opts.LoginTimeout)
	defer deadline.Stop()
	cursor, passwordSent, codeSent := 0, false, false
	fail := func(msg string) {
		if l.state().State != LoginCancelled {
			l.set(LoginFailed, scrub(msg, password, code), w.opts.Now())
		}
		_ = l.proc.Kill()
	}
	write := func(s string) bool {
		_, err := l.proc.Write([]byte(s + "\r"))
		return err == nil
	}
	// Waiting closes the console when steamcmd exits, which ends the output.
	exitCode := make(chan int, 1)
	go func() { code, _ := l.proc.Wait(); exitCode <- code }()
	ended := false
	for {
		all := c.String()
		if cursor > len(all) {
			cursor = len(all)
		}
		text := all[cursor:]
		switch {
		case loggedInRe.MatchString(text):
			_ = write("quit")
			if _, err := w.update(func(s *settings) {
				s.User, s.LoggedInAt, s.CheckedAt, s.Expired = user, w.opts.Now().UTC(), w.opts.Now().UTC(), false
			}); err != nil {
				fail("store the sign-in: " + err.Error())
				return
			}
			w.waitExit(l.proc, c, 60*time.Second)
			l.set(LoginOK, "", w.opts.Now())
			return
		case failRe.MatchString(text):
			fail(lastLine(failRe, text))
			return
		case passwordRe.MatchString(text):
			if passwordSent {
				fail("steamcmd asked for the password again")
				return
			}
			passwordSent, cursor = true, len(all)
			if !write(password) {
				fail("steamcmd closed its console")
				return
			}
			continue
		case guardRe.MatchString(text):
			if code != "" && !codeSent {
				codeSent, cursor = true, len(all)
				if !write(code) {
					fail("steamcmd closed its console")
					return
				}
				continue
			}
			l.set(LoginNeedCode, "", w.opts.Now())
			select {
			case code = <-l.code:
				codeSent, cursor = true, len(c.String())
				l.set(LoginStarting, "", w.opts.Now())
				if !write(code) {
					fail("steamcmd closed its console")
					return
				}
				continue
			case <-c.done:
			case <-deadline.C:
				fail("no Steam Guard code entered in time")
				return
			}
		case mobileRe.MatchString(text):
			if st := l.state().State; st != LoginConfirm {
				l.set(LoginConfirm, "", w.opts.Now())
			}
		}
		select {
		case <-c.ping:
		case <-c.done:
			if !ended { // the output is complete: look at it once more
				ended = true
				continue
			}
			if l.state().State != LoginCancelled {
				fail(fmt.Sprintf("steamcmd exited (code %d) before signing in", <-exitCode))
			}
			return
		case <-deadline.C:
			fail("the sign-in timed out")
			return
		}
	}
}

// waitExit waits for steamcmd to quit, killing it after d.
func (w *Workshop) waitExit(p Proc, c *console, d time.Duration) int {
	exited := make(chan int, 1)
	go func() { code, _ := p.Wait(); exited <- code }()
	select {
	case code := <-exited:
		<-c.done
		return code
	case <-time.After(d):
		_ = p.Kill()
		<-c.done
		return -1
	}
}

// batch is a non-interactive steamcmd run's outcome.
type batch struct {
	out    string
	code   int
	prompt bool // it asked for a password / Steam Guard: no cached sign-in
}

// runBatch runs steamcmd without typing anything; a sign-in prompt kills it.
func (w *Workshop) runBatch(ctx context.Context, exe string, args []string, timeout time.Duration) (batch, error) {
	p, err := w.opts.Run(exe, args, filepath.Dir(exe))
	if err != nil {
		return batch{}, fmt.Errorf("steam: start steamcmd: %w", err)
	}
	defer func() { _ = p.Close() }()
	c := readConsole(p)
	exited := make(chan int, 1)
	go func() { code, _ := p.Wait(); exited <- code }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	var b batch
	for {
		select {
		case b.code = <-exited:
			<-c.done
			b.out = c.String()
			return b, nil
		case <-c.ping:
			s := c.String()
			if passwordRe.MatchString(s) || guardRe.MatchString(s) || mobileRe.MatchString(s) {
				b.prompt = true
				_ = p.Kill()
			}
		case <-t.C:
			_ = p.Kill()
			<-c.done
			return batch{out: c.String()}, fmt.Errorf("steam: steamcmd did not finish in %s", timeout)
		case <-ctx.Done():
			_ = p.Kill()
			<-c.done
			return batch{out: c.String()}, ctx.Err()
		}
	}
}

// loginFailed: the run did not sign in with the cached credentials.
func (b batch) loginFailed() bool {
	if loggedInRe.MatchString(b.out) {
		return false
	}
	return b.prompt || cachedNone.MatchString(b.out) || failRe.MatchString(b.out)
}

// markExpired records an expired sign-in; OnRelogin runs once per expiry.
func (w *Workshop) markExpired() {
	var was bool
	_, _ = w.update(func(s *settings) {
		was = s.Expired
		s.Expired, s.CheckedAt = true, w.opts.Now().UTC()
	})
	if !was && w.opts.OnRelogin != nil {
		w.opts.OnRelogin()
	}
}

// batchArgs are the flags of a non-interactive run: fail instead of prompting.
func batchArgs(user string, cmds ...string) []string {
	return append([]string{"+@ShutdownOnFailedCommand", "1", "+@NoPromptForPassword", "1", "+login", user}, append(cmds, "+quit")...)
}

// user is the signed-in account (ErrNoLogin when none).
func (w *Workshop) user() (settings, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := w.load()
	if err != nil {
		return s, err
	}
	if s.User == "" {
		return s, ErrNoLogin
	}
	return s, nil
}

// CheckSession logs in with the cached sign-in and quits (read-only).
func (w *Workshop) CheckSession(ctx context.Context) (Status, error) {
	s, err := w.user()
	if err != nil {
		return Status{}, err
	}
	exe, _, err := w.steamcmd()
	if err != nil {
		return Status{}, err
	}
	if err := w.lock(ctx, 2*time.Minute); err != nil {
		return Status{}, err
	}
	b, err := w.runBatch(ctx, exe, batchArgs(s.User), w.opts.CheckTimeout)
	w.run.Unlock()
	if err != nil {
		return Status{}, err
	}
	if b.loginFailed() {
		w.markExpired()
		return Status{}, ErrRelogin
	}
	if b.code != 0 || !loggedInRe.MatchString(b.out) {
		return Status{}, fmt.Errorf("steam: steamcmd exited (code %d): %s", b.code, scrub(lastLine(failRe, b.out)))
	}
	if _, err := w.update(func(s *settings) { s.Expired, s.CheckedAt = false, w.opts.Now().UTC() }); err != nil {
		return Status{}, err
	}
	return w.Status()
}
