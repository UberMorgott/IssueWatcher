//go:build windows

package steamugc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The Steamworks flat API (steam_api_flat.h) over a steam_api64.dll from the
// owner's Steam library. Call results are polled with ISteamUtils
// (IsAPICallCompleted / GetAPICallResult) while SteamAPI_RunCallbacks pumps
// the client pipe; no callback registration is needed.

const (
	ugcCallbacks          = 3400
	cbCreateItemResult    = ugcCallbacks + 3
	cbSubmitItemUpdate    = ugcCallbacks + 4
	workshopFileCommunity = 0
	callTimeout           = 10 * time.Minute
)

// createItemResult is CreateItemResult_t (callback pack 8 on Windows).
type createItemResult struct {
	EResult int32
	_       int32
	Item    uint64
	Legal   bool
	_       [7]byte
}

// submitItemUpdateResult is SubmitItemUpdateResult_t.
type submitItemUpdateResult struct {
	EResult int32
	Legal   bool
	_       [3]byte
	Item    uint64
}

type flat struct {
	dll   *syscall.DLL
	ugc   uintptr
	utils uintptr
	user  uintptr
	procs map[string]*syscall.Proc
}

// NewAPI loads steam_api64.dll from path (the helper's API).
func NewAPI(path string) (API, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return &flat{dll: dll, procs: map[string]*syscall.Proc{}}, nil
}

func (f *flat) proc(name string) (*syscall.Proc, error) {
	if p, ok := f.procs[name]; ok {
		return p, nil
	}
	p, err := f.dll.FindProc(name)
	if err != nil {
		return nil, err
	}
	f.procs[name] = p
	return p, nil
}

// call calls an export; a missing one panics (checked in Init).
func (f *flat) call(name string, a ...uintptr) uintptr {
	p, err := f.proc(name)
	if err != nil {
		panic(fmt.Sprintf("steam_api64.dll has no %s", name))
	}
	r, _, _ := p.Call(a...)
	return r
}

func cstr(s string) *byte {
	b, err := syscall.BytePtrFromString(strings.ReplaceAll(s, "\x00", ""))
	if err != nil {
		return new(byte)
	}
	return b
}

func isTrue(r uintptr) bool { return r&0xff != 0 }

// low32 is an int-sized enum result (EResult, ESteamAPIInitResult).
func low32(r uintptr) int { return int(r & 0xffffffff) }

// accessor finds the newest versioned interface accessor, e.g. SteamAPI_SteamUGC_v020.
func (f *flat) accessor(base string, hi, lo int) (uintptr, error) {
	for v := hi; v >= lo; v-- {
		name := fmt.Sprintf("SteamAPI_%s_v%03d", base, v)
		if _, err := f.proc(name); err == nil {
			if ptr := f.call(name); ptr != 0 {
				return ptr, nil
			}
		}
	}
	return 0, fmt.Errorf("steam_api64.dll has no usable %s interface (too old?)", base)
}

var required = []string{"SteamAPI_RunCallbacks", "SteamAPI_Shutdown", "SteamAPI_ISteamUGC_CreateItem", "SteamAPI_ISteamUGC_StartItemUpdate",
	"SteamAPI_ISteamUGC_SetItemUpdateLanguage", "SteamAPI_ISteamUGC_SetItemTitle", "SteamAPI_ISteamUGC_SetItemDescription",
	"SteamAPI_ISteamUGC_SetItemPreview", "SteamAPI_ISteamUGC_SetItemVisibility", "SteamAPI_ISteamUGC_SetItemTags",
	"SteamAPI_ISteamUGC_SubmitItemUpdate", "SteamAPI_ISteamUtils_IsAPICallCompleted", "SteamAPI_ISteamUtils_GetAPICallResult",
	"SteamAPI_ISteamUser_GetSteamID"}

func (f *flat) Init() error {
	for _, n := range required {
		if _, err := f.proc(n); err != nil {
			return fmt.Errorf("steam_api64.dll lacks %s (too old)", n)
		}
	}
	var msg [1024]byte
	switch {
	case f.has("SteamAPI_InitFlat"):
		if r := f.call("SteamAPI_InitFlat", uintptr(unsafe.Pointer(&msg[0]))); low32(r) != 0 { //nolint:gosec // G103: the API's out buffer
			return initError(low32(r), msg[:])
		}
	case f.has("SteamInternal_SteamAPI_Init"):
		if r := f.call("SteamInternal_SteamAPI_Init", 0, uintptr(unsafe.Pointer(&msg[0]))); low32(r) != 0 { //nolint:gosec // G103: the API's out buffer
			return initError(low32(r), msg[:])
		}
	case f.has("SteamAPI_Init"):
		if !isTrue(f.call("SteamAPI_Init")) {
			return errors.New("SteamAPI_Init failed: is Steam running and signed in, and does the account own the game?")
		}
	default:
		return errors.New("steam_api64.dll has no init export")
	}
	var err error
	if f.ugc, err = f.accessor("SteamUGC", 25, 14); err != nil {
		return err
	}
	if f.utils, err = f.accessor("SteamUtils", 12, 9); err != nil {
		return err
	}
	if f.user, err = f.accessor("SteamUser", 25, 19); err != nil {
		return err
	}
	return nil
}

func (f *flat) has(name string) bool { _, err := f.proc(name); return err == nil }

func initError(code int, msg []byte) error {
	s := string(msg)
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return fmt.Errorf("Steam API init failed (%d: %s): is Steam running, signed in as the item's owner, and does it own the game?", code, s) //nolint:staticcheck // ST1005: Steam is a name
}

func (f *flat) Shutdown() { f.call("SteamAPI_Shutdown") }

func (f *flat) SteamID() uint64 { return uint64(f.call("SteamAPI_ISteamUser_GetSteamID", f.user)) }

// wait polls call until it completes and copies its result into out.
func (f *flat) wait(call uint64, cb int, out unsafe.Pointer, size uintptr) error {
	if call == 0 {
		return errors.New("the call was not started (k_uAPICallInvalid)")
	}
	deadline := time.Now().Add(callTimeout)
	var failed bool
	for {
		f.call("SteamAPI_RunCallbacks")
		if isTrue(f.call("SteamAPI_ISteamUtils_IsAPICallCompleted", f.utils, uintptr(call), uintptr(unsafe.Pointer(&failed)))) { //nolint:gosec // G103: out flag
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no answer from Steam in %s", callTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !isTrue(f.call("SteamAPI_ISteamUtils_GetAPICallResult", f.utils, uintptr(call), uintptr(out), size, uintptr(cb), uintptr(unsafe.Pointer(&failed)))) || failed { //nolint:gosec // G103: out struct
		return errors.New("Steam reported the call as failed (IO failure)") //nolint:staticcheck // ST1005: Steam is a name
	}
	return nil
}

func (f *flat) CreateItem(appID uint32) (uint64, bool, int, error) {
	call := uint64(f.call("SteamAPI_ISteamUGC_CreateItem", f.ugc, uintptr(appID), workshopFileCommunity))
	var r createItemResult
	if err := f.wait(call, cbCreateItemResult, unsafe.Pointer(&r), unsafe.Sizeof(r)); err != nil { //nolint:gosec // G103: out struct
		return 0, false, 0, err
	}
	return r.Item, r.Legal, int(r.EResult), nil
}

func (f *flat) Update(appID uint32, item uint64, u Update) (int, bool, error) {
	h := uint64(f.call("SteamAPI_ISteamUGC_StartItemUpdate", f.ugc, uintptr(appID), uintptr(item)))
	if h == 0 || h == ^uint64(0) {
		return 0, false, errors.New("StartItemUpdate refused")
	}
	keep := []any{}
	set := func(name string, a ...uintptr) error {
		if !isTrue(f.call(name, append([]uintptr{f.ugc, uintptr(h)}, a...)...)) {
			return fmt.Errorf("%s refused", strings.TrimPrefix(name, "SteamAPI_ISteamUGC_"))
		}
		return nil
	}
	str := func(s string) uintptr {
		p := cstr(s)
		keep = append(keep, p)
		return uintptr(unsafe.Pointer(p)) //nolint:gosec // G103: C string for the call
	}
	steps := []func() error{
		func() error { return set("SteamAPI_ISteamUGC_SetItemUpdateLanguage", str(u.Language)) },
		func() error { return set("SteamAPI_ISteamUGC_SetItemTitle", str(u.Title)) },
		func() error { return set("SteamAPI_ISteamUGC_SetItemDescription", str(u.Description)) },
	}
	if u.Preview != "" {
		steps = append(steps, func() error { return set("SteamAPI_ISteamUGC_SetItemPreview", str(u.Preview)) })
	}
	if u.Visibility >= 0 {
		steps = append(steps, func() error { return set("SteamAPI_ISteamUGC_SetItemVisibility", uintptr(u.Visibility)) })
	}
	if u.Tags != nil {
		ptrs := make([]*byte, len(u.Tags)+1)
		for i, t := range u.Tags {
			ptrs[i] = cstr(t)
		}
		arr := &struct {
			Strings **byte
			N       int32
		}{Strings: &ptrs[0], N: int32(len(u.Tags))} //nolint:gosec // G115: at most 20 tags
		keep = append(keep, ptrs, arr)
		// SetItemTags(handle, tags[, bAllowAdminTags]): the extra false is ignored by older DLLs.
		steps = append(steps, func() error { return set("SteamAPI_ISteamUGC_SetItemTags", uintptr(unsafe.Pointer(arr)), 0) }) //nolint:gosec // G103: SteamParamStringArray_t
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return 0, false, err
		}
	}
	call := uint64(f.call("SteamAPI_ISteamUGC_SubmitItemUpdate", f.ugc, uintptr(h), str(u.ChangeNote)))
	var r submitItemUpdateResult
	err := f.wait(call, cbSubmitItemUpdate, unsafe.Pointer(&r), unsafe.Sizeof(r)) //nolint:gosec // G103: out struct
	runtime.KeepAlive(keep)
	if err != nil {
		return 0, false, err
	}
	return int(r.EResult), r.Legal, nil
}
