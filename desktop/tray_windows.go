//go:build windows

package main

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	shell32               = syscall.NewLazyDLL("shell32.dll")
	shellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	user32                = syscall.NewLazyDLL("user32.dll")
	loadIcon              = user32.NewProc("LoadIconW")
	createPopupMenu       = user32.NewProc("CreatePopupMenu")
	appendMenu            = user32.NewProc("AppendMenuW")
	trackPopupMenu        = user32.NewProc("TrackPopupMenu")
	destroyMenu           = user32.NewProc("DestroyMenu")
	getCursorPos          = user32.NewProc("GetCursorPos")
	setForegroundWnd      = user32.NewProc("SetForegroundWindow")
	defWindowProc         = user32.NewProc("DefWindowProcW")
	registerClassEx       = user32.NewProc("RegisterClassExW")
	createWindowEx        = user32.NewProc("CreateWindowExW")
	getMessage            = user32.NewProc("GetMessageW")
	translateMessage      = user32.NewProc("TranslateMessage")
	dispatchMessage       = user32.NewProc("DispatchMessageW")
	postQuitMessage       = user32.NewProc("PostQuitMessage")
)

const (
	NIM_ADD        = 0x00000000
	NIM_MODIFY     = 0x00000001
	NIM_DELETE     = 0x00000002

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
	NIF_INFO    = 0x00000010

	NIIF_NONE    = 0x00000000
	NIIF_INFO    = 0x00000001
	NIIF_WARNING = 0x00000002

	WM_USER      = 0x0400
	WM_TRAYICON  = WM_USER + 1
	WM_COMMAND   = 0x0111
	WM_RBUTTONUP = 0x0205
	WM_LBUTTONUP = 0x0202
	WM_DESTROY   = 0x0002

	IDI_APPLICATION = 32512

	MF_STRING    = 0x00000000
	MF_SEPARATOR = 0x00000800
	MF_GRAYED    = 0x00000001

	TPM_RIGHTBUTTON = 0x0002
	TPM_BOTTOMALIGN = 0x0020

	ID_STATUS    = 1001
	ID_IP        = 1002
	ID_SPEEDTEST = 1003
	ID_IPINFO    = 1004
	ID_EXIT      = 1005

	ID_MODE_SPLIT = 1010
	ID_MODE_DUAL  = 1011
	ID_MODE_FULL  = 1012
	ID_MODE_HOME  = 1013
)

type POINT struct {
	X, Y int32
}

type MSG struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type NOTIFYICONDATAW struct {
	CbSize            uint32
	HWnd              uintptr
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             uintptr
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UTimeoutOrVersion uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
	GuidItem          [16]byte
	HBalloonIcon      uintptr
}

type TrayManager struct {
	hWnd         uintptr
	nid          NOTIFYICONDATAW
	mu           sync.Mutex
	statusText   string
	ipText       string
	currentMode  string
	onModeChange func(string)
	onExit       func()
}

var globalTray *TrayManager

func wndProc(hWnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_TRAYICON:
		if lParam == WM_RBUTTONUP || lParam == WM_LBUTTONUP {
			if globalTray != nil {
				globalTray.showMenu()
			}
		}
	case WM_COMMAND:
		switch wParam {
		case ID_MODE_SPLIT:
			if globalTray != nil {
				globalTray.SelectMode("split")
			}
		case ID_MODE_DUAL:
			if globalTray != nil {
				globalTray.SelectMode("dual")
			}
		case ID_MODE_FULL:
			if globalTray != nil {
				globalTray.SelectMode("full")
			}
		case ID_MODE_HOME:
			if globalTray != nil {
				globalTray.SelectMode("home")
			}
		case ID_SPEEDTEST:
			exec.Command("cmd", "/c", "start", "https://www.speedtest.net").Start()
		case ID_IPINFO:
			exec.Command("cmd", "/c", "start", "https://ipinfo.io").Start()
		case ID_EXIT:
			if globalTray != nil && globalTray.onExit != nil {
				globalTray.onExit()
			}
			postQuitMessage.Call(0)
		}
	case WM_DESTROY:
		postQuitMessage.Call(0)
	default:
		r, _, _ := defWindowProc.Call(hWnd, uintptr(msg), wParam, lParam)
		return r
	}
	return 0
}

func StartTray(onExit func(), onModeChange func(string)) *TrayManager {
	tm := &TrayManager{
		statusText:   "TetherFlow: 等待手机连接...",
		currentMode:  "split",
		onModeChange: onModeChange,
		onExit:       onExit,
	}
	globalTray = tm

	readyChan := make(chan bool)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		className := syscall.StringToUTF16Ptr("TetherFlowTrayClass")
		hIcon, _, _ := loadIcon.Call(0, uintptr(IDI_APPLICATION))

		var wc WNDCLASSEXW
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		wc.LpfnWndProc = syscall.NewCallback(wndProc)
		wc.LpszClassName = className
		wc.HIcon = hIcon

		registerClassEx.Call(uintptr(unsafe.Pointer(&wc)))

		hWnd, _, _ := createWindowEx.Call(
			0,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(className)),
			0, 0, 0, 0, 0,
			0, 0, 0, 0,
		)
		tm.hWnd = hWnd

		tm.nid.CbSize = uint32(unsafe.Sizeof(tm.nid))
		tm.nid.HWnd = hWnd
		tm.nid.UID = 1
		tm.nid.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
		tm.nid.UCallbackMessage = WM_TRAYICON
		tm.nid.HIcon = hIcon
		copyStringToUTF16(tm.nid.SzTip[:], "TetherFlow: 5G 极速中继")

		shellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&tm.nid)))
		readyChan <- true

		var msg MSG
		for {
			r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}

		// Delete icon on exit
		shellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&tm.nid)))
	}()

	<-readyChan
	return tm
}

func (tm *TrayManager) SetStatus(status, ip string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.statusText = status
	tm.ipText = ip

	tm.nid.UFlags = NIF_TIP
	copyStringToUTF16(tm.nid.SzTip[:], "TetherFlow: "+status)
	shellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&tm.nid)))
}

func (tm *TrayManager) NotifyBalloon(title, info string, isWarn bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.nid.UFlags = NIF_INFO | NIF_TIP
	copyStringToUTF16(tm.nid.SzInfoTitle[:], title)
	copyStringToUTF16(tm.nid.SzInfo[:], info)
	if isWarn {
		tm.nid.DwInfoFlags = NIIF_WARNING
	} else {
		tm.nid.DwInfoFlags = NIIF_INFO
	}
	shellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&tm.nid)))
}

func (tm *TrayManager) SelectMode(mode string) {
	tm.mu.Lock()
	tm.currentMode = mode
	cb := tm.onModeChange
	tm.mu.Unlock()

	if cb != nil {
		cb(mode)
	}
}

func (tm *TrayManager) showMenu() {
	hMenu, _, _ := createPopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer destroyMenu.Call(hMenu)

	tm.mu.Lock()
	st := tm.statusText
	ip := tm.ipText
	mode := tm.currentMode
	tm.mu.Unlock()

	appendMenu.Call(hMenu, MF_STRING|MF_GRAYED, ID_STATUS, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("● 连接: "+st))))
	if ip != "" {
		appendMenu.Call(hMenu, MF_STRING|MF_GRAYED, ID_IP, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("● 出口: "+ip+" (零热点配额)"))))
	}
	appendMenu.Call(hMenu, MF_SEPARATOR, 0, 0)

	splitPrefix := "  "
	dualPrefix := "  "
	fullPrefix := "  "
	homePrefix := "  "
	switch mode {
	case "split":
		splitPrefix = "✔ "
	case "dual":
		dualPrefix = "✔ "
	case "full":
		fullPrefix = "✔ "
	case "home":
		homePrefix = "✔ "
	default:
		splitPrefix = "✔ "
	}

	appendMenu.Call(hMenu, MF_STRING, ID_MODE_SPLIT, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(splitPrefix+"🎯 智能动静分流 (游戏3ms/视频1.2G)"))))
	appendMenu.Call(hMenu, MF_STRING, ID_MODE_DUAL, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(dualPrefix+"⚖️ 双网并发叠加 (下载双网叠加破千兆)"))))
	appendMenu.Call(hMenu, MF_STRING, ID_MODE_FULL, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(fullPrefix+"🚀 5G 极速独享 (全量 5G 满血)"))))
	appendMenu.Call(hMenu, MF_STRING, ID_MODE_HOME, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(homePrefix+"🏠 仅家庭 Wi-Fi (直连路由，不走5G)"))))

	appendMenu.Call(hMenu, MF_SEPARATOR, 0, 0)
	appendMenu.Call(hMenu, MF_STRING, ID_SPEEDTEST, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("🚀 打开 Speedtest 测速"))))
	appendMenu.Call(hMenu, MF_STRING, ID_IPINFO, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("🌐 打开 IP 检测 (ipinfo.io)"))))
	appendMenu.Call(hMenu, MF_SEPARATOR, 0, 0)
	appendMenu.Call(hMenu, MF_STRING, ID_EXIT, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("退出 TetherFlow (自动恢复网络)"))))

	var pt POINT
	getCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	setForegroundWnd.Call(tm.hWnd)
	trackPopupMenu.Call(hMenu, TPM_RIGHTBUTTON|TPM_BOTTOMALIGN, uintptr(pt.X), uintptr(pt.Y), 0, tm.hWnd, 0)
}

func copyStringToUTF16(dst []uint16, src string) {
	u16 := syscall.StringToUTF16(src)
	maxLen := len(dst) - 1
	if len(u16) < maxLen {
		maxLen = len(u16)
	}
	for i := 0; i < maxLen; i++ {
		dst[i] = u16[i]
	}
	dst[maxLen] = 0
}
