//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// After the program exits the console may still render its last frame:
// Wait closes it once the output was quiet for exitQuiet (at most exitGrace).
const (
	exitQuiet = 250 * time.Millisecond
	exitGrace = 3 * time.Second
)

type proc struct {
	hpc     windows.Handle
	process windows.Handle
	in      *os.File     // our end of the console input
	out     *os.File     // our end of the console output
	lastOut atomic.Int64 // unix nanos of the last output read

	closeOnce sync.Once
}

// Start runs exe with args (args[0] is not the program; exe is) in a new
// 200x50 pseudo console.
func Start(exe string, args []string, opts Options) (Proc, error) {
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("conpty: input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		_ = windows.CloseHandle(inR)
		_ = windows.CloseHandle(inW)
		return nil, fmt.Errorf("conpty: output pipe: %w", err)
	}
	// The pseudo console duplicates its ends: ours are closed after the start.
	defer func() {
		_ = windows.CloseHandle(inR)
		_ = windows.CloseHandle(outW)
	}()
	p := &proc{in: os.NewFile(uintptr(inW), "conpty-in"), out: os.NewFile(uintptr(outR), "conpty-out")}
	fail := func(err error) (Proc, error) {
		_ = p.in.Close()
		_ = p.out.Close()
		if p.hpc != 0 {
			windows.ClosePseudoConsole(p.hpc)
		}
		return nil, err
	}
	if err := windows.CreatePseudoConsole(windows.Coord{X: 200, Y: 50}, inR, outW, 0, &p.hpc); err != nil {
		return fail(fmt.Errorf("conpty: create: %w", err))
	}
	al, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(fmt.Errorf("conpty: attributes: %w", err))
	}
	defer al.Delete()
	// The attribute value is the HPCON itself, not a pointer to it.
	hpc := *(*unsafe.Pointer)(unsafe.Pointer(&p.hpc)) //nolint:gosec // G103: UpdateProcThreadAttribute takes the HPCON value as the pointer argument
	if err := al.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpc, unsafe.Sizeof(p.hpc)); err != nil {
		return fail(fmt.Errorf("conpty: attribute: %w", err))
	}
	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = al.List()
	// Empty std handles: otherwise a child of a process with redirected std
	// handles (a service, a test) writes to those instead of the console.
	si.Flags = windows.STARTF_USESTDHANDLES

	exeW, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return fail(err)
	}
	cmdW, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return fail(err)
	}
	var dirW *uint16
	if opts.Dir != "" {
		if dirW, err = windows.UTF16PtrFromString(opts.Dir); err != nil {
			return fail(err)
		}
	}
	var envW *uint16
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)
	if opts.Env != nil {
		block, err := envBlock(opts.Env)
		if err != nil {
			return fail(err)
		}
		envW = &block[0]
		flags |= windows.CREATE_UNICODE_ENVIRONMENT
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(exeW, cmdW, nil, nil, false, flags, envW, dirW, &si.StartupInfo, &pi); err != nil {
		return fail(fmt.Errorf("conpty: start %s: %w", exe, err))
	}
	_ = windows.CloseHandle(pi.Thread)
	p.process = pi.Process
	return p, nil
}

// envBlock is a CREATE_UNICODE_ENVIRONMENT block: k=v NUL ... NUL.
func envBlock(env []string) ([]uint16, error) {
	var b []uint16
	for _, kv := range env {
		for _, r := range kv {
			if r == 0 {
				return nil, errors.New("conpty: NUL in the environment")
			}
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	return append(b, 0), nil
}

func (p *proc) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *proc) Output() io.Reader { return outReader{p} }

// outReader notes when output arrives (Wait's quiet check).
type outReader struct{ p *proc }

func (r outReader) Read(b []byte) (int, error) {
	n, err := r.p.out.Read(b)
	if n > 0 {
		r.p.lastOut.Store(time.Now().UnixNano())
	}
	return n, err
}

func (p *proc) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return -1, err
	}
	// The program is gone: closing the console ends the output stream. The
	// console renders asynchronously, so its last frame (a program's final
	// lines) is still on its way: give it a moment before closing.
	for start := time.Now(); time.Since(start) < exitGrace; time.Sleep(50 * time.Millisecond) {
		if time.Since(time.Unix(0, p.lastOut.Load())) >= exitQuiet {
			break
		}
	}
	p.closeConsole()
	return int(code), nil
}

func (p *proc) Kill() error {
	err := windows.TerminateProcess(p.process, 1)
	p.closeConsole()
	return err
}

func (p *proc) closeConsole() {
	p.closeOnce.Do(func() {
		_ = p.in.Close()
		windows.ClosePseudoConsole(p.hpc)
	})
}

func (p *proc) Close() error {
	p.closeConsole()
	err := p.out.Close()
	if e := windows.CloseHandle(p.process); err == nil {
		err = e
	}
	return err
}
