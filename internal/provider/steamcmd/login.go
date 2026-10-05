package steamcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sign-in states (LoginState.State).
const (
	LoginIdle      = "idle"
	LoginStarting  = "starting"       // steamcmd starts
	LoginUpdating  = "updating"       // steamcmd updates itself (first run: up to a few minutes)
	LoginLoggingIn = "logging_in"     // steamcmd talks to Steam
	LoginNeedCode  = "need_code"      // steamcmd asks for the Steam Guard code
	LoginConfirm   = "confirm_mobile" // waiting for the approval in the Steam Mobile app
	LoginOK        = "ok"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
)

// Sign-in failure codes (LoginState.Code; the UI words them, Error is the
// English detail).
const (
	FailInvalidPassword = "invalid_password"
	FailRateLimit       = "rate_limit"
	FailBadCode         = "bad_code"
	FailLogonDenied     = "logon_denied"
	FailNetwork         = "network"
	FailSteam           = "steam_error"
	FailPasswordAgain   = "password_again"
	FailTimeoutUpdate   = "timeout_update"
	FailTimeoutLogin    = "timeout_login"
	FailTimeoutCode     = "timeout_code"
	FailTimeoutMobile   = "timeout_mobile"
	FailExited          = "exited"
	FailInternal        = "internal"
)

// LoginState is a sign-in's progress.
type LoginState struct {
	State string `json:"state"`
	Code  string `json:"code,omitempty"` // a failure's code (Fail*)
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
	mu     sync.Mutex
	st     LoginState
	proc   Proc
	code   chan string
	dir    string // steamcmd's folder (its logs\connection_log.txt)
	logOff int64  // the connection log's size before this run
}

func (l *loginRun) state() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *loginRun) set(state, msg string, now time.Time) {
	l.setCode(state, "", msg, now)
}

func (l *loginRun) setCode(state, code, msg string, now time.Time) {
	l.mu.Lock()
	l.st = LoginState{State: state, Code: code, Error: msg, At: now.UTC().Format(time.RFC3339)}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	exe, err := w.ensure(ctx) // not set up yet: download it now
	cancel()
	if err != nil {
		return LoginState{}, err
	}
	if !w.run.TryLock() {
		return LoginState{}, ErrBusy
	}
	off := logSize(connLog(filepath.Dir(exe)))
	p, err := w.opts.Run(exe, []string{"+login", user}, filepath.Dir(exe))
	if err != nil {
		w.run.Unlock()
		return LoginState{}, fmt.Errorf("steam: start steamcmd: %w", err)
	}
	l := &loginRun{proc: p, code: make(chan string, 1), dir: filepath.Dir(exe), logOff: off}
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
	// steamcmd prints nothing while it waits for the Steam Mobile approval
	// (its console only shows "Logging in user ..." until "OK"): only its
	// logs\connection_log.txt says "Waiting for confirmation". Poll it.
	poll := time.NewTicker(w.opts.LogPoll)
	defer poll.Stop()
	cursor, passwordSent, codeSent := 0, false, false
	fail := func(fcode, msg string) {
		if l.state().State != LoginCancelled {
			l.setCode(LoginFailed, fcode, scrub(msg, password, code), w.opts.Now())
		}
		_ = l.proc.Kill()
	}
	// wait: the owner has to act (a code, the phone): the time starts anew.
	wait := func(state string) {
		if l.state().State == state {
			return
		}
		l.set(state, "", w.opts.Now())
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
		deadline.Reset(w.opts.LoginTimeout)
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
		st := l.state().State
		switch {
		case loggedInRe.MatchString(text):
			_ = write("quit")
			if _, err := w.update(func(s *settings) {
				s.User, s.LoggedInAt, s.CheckedAt, s.Expired = user, w.opts.Now().UTC(), w.opts.Now().UTC(), false
			}); err != nil {
				fail(FailInternal, "store the sign-in: "+err.Error())
				return
			}
			w.waitExit(l.proc, c, 60*time.Second)
			l.set(LoginOK, "", w.opts.Now())
			return
		case failRe.MatchString(text):
			line := lastLine(failRe, text)
			fail(failCode(line), line)
			return
		case passwordRe.MatchString(text):
			if passwordSent {
				fail(FailPasswordAgain, "steamcmd asked for the password again")
				return
			}
			passwordSent, cursor = true, len(all)
			l.set(LoginLoggingIn, "", w.opts.Now())
			if !write(password) {
				fail(FailExited, "steamcmd closed its console")
				return
			}
			continue
		case guardRe.MatchString(text):
			if code != "" && !codeSent {
				codeSent, cursor = true, len(all)
				if !write(code) {
					fail(FailExited, "steamcmd closed its console")
					return
				}
				continue
			}
			wait(LoginNeedCode)
			select {
			case code = <-l.code:
				codeSent, cursor = true, len(c.String())
				l.set(LoginLoggingIn, "", w.opts.Now())
				if !write(code) {
					fail(FailExited, "steamcmd closed its console")
					return
				}
				continue
			case <-c.done:
			case <-deadline.C:
				fail(FailTimeoutCode, "no Steam Guard code entered in time")
				return
			}
		case mobileRe.MatchString(text):
			wait(LoginConfirm)
		case st == LoginStarting || st == LoginUpdating: // the password sent = signing in already
			if p := phase(all); p != "" && p != st {
				l.set(p, "", w.opts.Now())
			}
		}
		select {
		case <-c.ping:
		case <-poll.C:
			if st := l.state().State; (st == LoginLoggingIn || st == LoginStarting) && w.awaitsMobile(l) {
				wait(LoginConfirm)
			}
		case <-c.done:
			if !ended { // the output is complete: look at it once more
				ended = true
				continue
			}
			if l.state().State != LoginCancelled {
				fail(FailExited, fmt.Sprintf("steamcmd exited (code %d) before signing in: %s", <-exitCode, lastOutput(c.String())))
			}
			return
		case <-deadline.C:
			switch l.state().State {
			case LoginUpdating:
				fail(FailTimeoutUpdate, "steamcmd did not finish updating itself in "+w.opts.LoginTimeout.String())
			case LoginConfirm:
				fail(FailTimeoutMobile, "the sign-in was not approved in the Steam Mobile app in "+w.opts.LoginTimeout.String())
			default:
				fail(FailTimeoutLogin, "steamcmd did not sign in within "+w.opts.LoginTimeout.String()+": "+lastOutput(c.String()))
			}
			return
		}
	}
}

// phase is what steamcmd's console last showed: its self-update ("[ 12%] ..."
// lines, localized), the start ("Loading Steam API") or the sign-in
// ("Logging in user"); "" = none yet.
func phase(all string) string {
	best, at := "", -1
	for _, m := range []struct {
		re    *regexp.Regexp
		state string
	}{{updateRe, LoginUpdating}, {loadingRe, LoginStarting}, {loggingInRe, LoginLoggingIn}} {
		if loc := m.re.FindAllStringIndex(all, -1); len(loc) > 0 && loc[len(loc)-1][0] > at {
			best, at = m.state, loc[len(loc)-1][0]
		}
	}
	return best
}

// awaitsMobile: steamcmd's connection log of this run says it waits for the
// approval in the Steam Mobile app.
func (w *Workshop) awaitsMobile(l *loginRun) bool {
	f, err := os.Open(connLog(l.dir))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	off := l.logOff
	if fi.Size() < off { // steamcmd started the log anew
		off = 0
	}
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, off, fi.Size()-off), 16<<20))
	if err != nil {
		return false
	}
	return confirmLogRe.Match(b)
}

// connLog is steamcmd's connection log; it grows across runs.
func connLog(dir string) string { return filepath.Join(dir, "logs", "connection_log.txt") }

// logSize is a log's size now (0 = none): a run reads only what follows.
func logSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// failCode classifies steamcmd's failure line.
func failCode(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "invalid password"):
		return FailInvalidPassword
	case strings.Contains(l, "rate limit"):
		return FailRateLimit
	case strings.Contains(l, "auth code"), strings.Contains(l, "two-factor"), strings.Contains(l, "two factor"):
		return FailBadCode
	case strings.Contains(l, "logon denied"):
		return FailLogonDenied
	case strings.Contains(l, "no connection"), strings.Contains(l, "timeout"), strings.Contains(l, "service unavailable"), strings.Contains(l, "try another cm"):
		return FailNetwork
	}
	return FailSteam
}

// lastOutput is steamcmd's last non-empty console line (a failure's detail).
func lastOutput(s string) string {
	for _, line := range slices.Backward(strings.Split(strings.TrimSpace(s), "\n")) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
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
	exe, err := w.steamcmd()
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
