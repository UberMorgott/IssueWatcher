// Package conpty runs a console program inside a Windows pseudo console, so
// what is written to it arrives as console input. Programs that read their
// prompts from the console (steamcmd's "password:" and Steam Guard prompts
// ignore a redirected stdin) can be answered this way without putting the
// answer on the command line.
package conpty

import (
	"errors"
	"io"
	"regexp"
	"strings"
)

// ErrUnsupported: no pseudo console on this OS.
var ErrUnsupported = errors.New("conpty: not supported on this OS")

// Options configures Start.
type Options struct {
	Dir string   // working directory ("" = inherit)
	Env []string // environment ("" = inherit)
}

// vtRe matches the VT sequences a pseudo console adds to the output (CSI, OSC,
// charset and keypad modes).
var vtRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78]")

// moveRe matches cursor moves to another line (CUP, HVP, CNL, CPL): the
// console draws line breaks with them.
var moveRe = regexp.MustCompile("\x1b\\[[0-9;]*[HfEF]")

// Plain strips the VT sequences and carriage returns from pseudo console
// output; a cursor move to another line becomes a line break.
func Plain(s string) string {
	s = moveRe.ReplaceAllString(s, "\n")
	return strings.ReplaceAll(vtRe.ReplaceAllString(s, ""), "\r", "")
}

// Proc is a running program in a pseudo console.
type Proc interface {
	// Write sends console input (use "\r" for Enter).
	io.Writer
	// Output is the console output (VT sequences included; see Plain). It
	// ends after the program exits.
	Output() io.Reader
	// Wait waits for the exit and returns the exit code.
	Wait() (int, error)
	// Kill ends the program.
	Kill() error
	// Close frees the console; call it once after Wait or Kill.
	Close() error
}
