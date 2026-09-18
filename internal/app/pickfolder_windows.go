package app

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The modern Explorer-style folder picker (IFileOpenDialog with
// FOS_PICKFOLDERS), called through its COM vtable without cgo.

var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidIShellItem       = windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}

	ole32   = windows.NewLazySystemDLL("ole32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	user32  = windows.NewLazySystemDLL("user32.dll")

	procCoCreateInstance            = ole32.NewProc("CoCreateInstance")
	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDestroyWindow               = user32.NewProc("DestroyWindow")
	procGetSystemMetrics            = user32.NewProc("GetSystemMetrics")
	procAttachThreadInput           = user32.NewProc("AttachThreadInput")
	procSetForegroundWindow         = user32.NewProc("SetForegroundWindow")
)

const (
	fosNoChangeDir      = 0x8
	fosPickFolders      = 0x20
	fosForceFileSystem  = 0x40
	fosPathMustExist    = 0x800
	sigdnFileSysPath    = 0x80058000
	wsExTopmost         = 0x8
	wsExToolWindow      = 0x80
	wsPopup             = 0x80000000
	smCxScreen          = 0
	smCyScreen          = 1
	hresultCancelled    = 0x80070000 | uintptr(windows.ERROR_CANCELLED)
	vtblRelease         = 2
	vtblShow            = 3
	vtblSetOptions      = 9
	vtblGetOptions      = 10
	vtblSetFolder       = 12
	vtblSetTitle        = 17
	vtblGetResult       = 20
	vtblItemDisplayName = 5
)

// comObject is any COM interface pointer: its first word points at the vtable.
type comObject struct{ vtbl *[32]uintptr }

func (o *comObject) call(method int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *comObject) release() { o.call(vtblRelease) }

func hresult(what string, hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", what, uint32(hr))
	}
	return nil
}

// pickFolder shows the folder dialog on its own OS thread with COM
// initialized there, and waits for the user.
func pickFolder(start, title string) (string, error) {
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := pickFolderOnThread(start, title)
		done <- result{p, err}
	}()
	r := <-done
	return r.path, r.err
}

func pickFolderOnThread(start, title string) (string, error) {
	err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
	if err != nil && !errors.Is(err, syscall.Errno(1)) { // S_FALSE: already initialized, still paired
		return "", fmt.Errorf("CoInitializeEx: %w", err)
	}
	defer windows.CoUninitialize()

	var dlg *comObject
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
	if err := hresult("CoCreateInstance", hr); err != nil {
		return "", err
	}
	defer dlg.release()

	// Out-parameters and strings live on the heap (new, UTF16PtrFromString):
	// a stack address turned into uintptr could move when the stack grows.
	opts := new(uint32)
	if err := hresult("GetOptions", dlg.call(vtblGetOptions, uintptr(unsafe.Pointer(opts)))); err != nil {
		return "", err
	}
	*opts |= fosPickFolders | fosForceFileSystem | fosPathMustExist | fosNoChangeDir
	if err := hresult("SetOptions", dlg.call(vtblSetOptions, uintptr(*opts))); err != nil {
		return "", err
	}
	if t, err := windows.UTF16PtrFromString(title); err == nil {
		dlg.call(vtblSetTitle, uintptr(unsafe.Pointer(t)))
		runtime.KeepAlive(t)
	}
	if folder := startItem(start); folder != nil {
		dlg.call(vtblSetFolder, uintptr(unsafe.Pointer(folder)))
		folder.release()
	}

	owner := foregroundOwner()
	defer func() { _, _, _ = procDestroyWindow.Call(owner) }()
	hr = dlg.call(vtblShow, owner)
	if hr == hresultCancelled {
		return "", ErrPickCancelled
	}
	if err := hresult("Show", hr); err != nil {
		return "", err
	}

	item := new(*comObject)
	if err := hresult("GetResult", dlg.call(vtblGetResult, uintptr(unsafe.Pointer(item)))); err != nil {
		return "", err
	}
	defer (*item).release()
	name := new(*uint16)
	if err := hresult("GetDisplayName", (*item).call(vtblItemDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(name)))); err != nil {
		return "", err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(*name))
	return windows.UTF16PtrToString(*name), nil
}

// startItem returns the shell item of an existing start folder, or nil.
func startItem(start string) *comObject {
	if fi, err := os.Stat(start); start == "" || err != nil || !fi.IsDir() {
		return nil
	}
	p, err := windows.UTF16PtrFromString(start)
	if err != nil {
		return nil
	}
	var item *comObject
	hr, _, _ := procSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&iidIShellItem)), uintptr(unsafe.Pointer(&item)))
	if int32(hr) < 0 {
		return nil
	}
	return item
}

// foregroundOwner creates an invisible topmost window and brings it to the
// foreground, so the dialog it owns opens above the browser instead of behind
// it. The dialog opens at the owner's top-left corner, and its default size is
// about half the screen, so a quarter in from the corner roughly centres it.
// The tray process has no window of its own and did not receive the click, so
// it borrows the foreground thread's input state for the switch.
func foregroundOwner() uintptr {
	cls, _ := windows.UTF16PtrFromString("STATIC")
	cx, _, _ := procGetSystemMetrics.Call(smCxScreen)
	cy, _, _ := procGetSystemMetrics.Call(smCyScreen)
	hwnd, _, _ := procCreateWindowExW.Call(wsExTopmost|wsExToolWindow, uintptr(unsafe.Pointer(cls)), 0,
		wsPopup, cx/4, cy/4, 0, 0, 0, 0, 0, 0)
	if hwnd == 0 {
		return 0
	}
	self := windows.GetCurrentThreadId()
	fg, _ := windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), nil)
	if fg != 0 && fg != self {
		_, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fg), 1)
		defer func() { _, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fg), 0) }()
	}
	_, _, _ = procSetForegroundWindow.Call(hwnd)
	return hwnd
}
