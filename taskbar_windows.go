package main

import (
	"fmt"
	"log"
	"math"
	"runtime"
	"slices"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The taskbar bar is a layered child window of the taskbar, so it hides
// with the taskbar for full-screen apps and needs no topmost tricks. It sits
// left of the notification area and of any other app docked there. One
// thread owns it: a hidden top-level window there gets the timer and the
// broadcasts that a child window never sees, and recreates the bar after
// Explorer restarts.

const taskbarSupported = true

var (
	gdi32 = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClassEx       = user32.NewProc("RegisterClassExW")
	procCreateWindowEx        = user32.NewProc("CreateWindowExW")
	procDefWindowProc         = user32.NewProc("DefWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetMessage            = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessage       = user32.NewProc("DispatchMessageW")
	procPostMessage           = user32.NewProc("PostMessageW")
	procFindWindow            = user32.NewProc("FindWindowW")
	procFindWindowEx          = user32.NewProc("FindWindowExW")
	procScreenToClient        = user32.NewProc("ScreenToClient")
	procUpdateLayeredWindow   = user32.NewProc("UpdateLayeredWindow")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procLoadCursor            = user32.NewProc("LoadCursorW")
	procSetThreadDpiContext   = user32.NewProc("SetThreadDpiAwarenessContext")
	procTrackMouseEvent       = user32.NewProc("TrackMouseEvent")

	procCreateCompatibleDC   = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection     = gdi32.NewProc("CreateDIBSection")
	procSelectObject         = gdi32.NewProc("SelectObject")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	procCreateFont           = gdi32.NewProc("CreateFontW")
	procGetTextExtentPoint32 = gdi32.NewProc("GetTextExtentPoint32W")
	procTextOut              = gdi32.NewProc("TextOutW")
	procSetTextColor         = gdi32.NewProc("SetTextColor")
	procSetBkMode            = gdi32.NewProc("SetBkMode")

	barCtrlProc   = windows.NewCallback(taskbarCtrlProc)
	barWindowProc = windows.NewCallback(taskbarBarProc)
	barEnumProc   = windows.NewCallback(taskbarEnumChild)
)

const (
	wmSettingChange  = 0x001A
	wmDisplayChange  = 0x007E
	wmMouseActivate  = 0x0021
	wmNCDestroy      = 0x0082
	wmUser           = 0x0400
	wmBarSync        = 0x8000 + 1 // WM_APP+1
	maNoActivate     = 3
	wsExLayered      = 0x00080000
	wsExNoActivate   = 0x08000000
	wsClipSiblings   = 0x04000000
	swpNoActivate    = 0x0010
	swpShowWindow    = 0x0040
	ulwAlpha         = 0x2
	barTimerID       = 1
	barTimerMillis   = 2000
	wmMouseMove      = 0x0200
	wmLButtonDown    = 0x0201
	wmMouseHover     = 0x02A1
	wmMouseLeave     = 0x02A3
	tmeHover         = 0x1
	tmeLeave         = 0x2
	hoverMillis      = 400
	ttsAlwaysTip     = 0x1
	ttsNoPrefix      = 0x2
	ttfTrack         = 0x20
	ttfAbsolute      = 0x80
	ttmTrackActivate = wmUser + 17
	ttmTrackPosition = wmUser + 18
	ttmSetMaxTipW    = wmUser + 24
	ttmAddTool       = wmUser + 50
	ttmUpdateTipText = wmUser + 57
	transparentBkgnd = 1
)

// taskbar is what the menu wants shown, shared with the bar's thread.
var taskbar struct {
	mu      sync.Mutex
	items   []barItem
	started bool
	ctrl    uintptr
}

// showTaskbar shows items on the taskbar, or removes the bar when there are none.
func showTaskbar(items []barItem) {
	taskbar.mu.Lock()
	defer taskbar.mu.Unlock()
	if slices.Equal(items, taskbar.items) {
		return
	}
	taskbar.items = items
	switch {
	case taskbar.ctrl != 0:
		procPostMessage.Call(taskbar.ctrl, wmBarSync, 0, 0)
	case !taskbar.started && len(items) > 0:
		taskbar.started = true
		go runTaskbar()
	}
}

// bar is the state of the bar's thread; only that thread touches it.
var bar struct {
	inst      uintptr
	class     *uint16
	hwnd, tip uintptr
	tb        uintptr
	dc        uintptr
	font      uintptr
	fontPx    int32
	key       string
	// The tooltip's text per account, the account under the mouse and the
	// one the tooltip shows, -1 for none.
	tipTexts     []string
	itemW        int32
	hover, shown int
	tracking     bool
	created      uint32 // the TaskbarCreated message
	// Windows of other apps on the taskbar, found by taskbarEnumChild.
	explorer, self uint32
	others         []windows.Rect
}

func runTaskbar() {
	// The windows belong to this thread for as long as the switcher runs.
	runtime.LockOSThread()
	// Coordinates in physical pixels like Explorer's, whatever the manifest says.
	const perMonitorAwareV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, -4
	procSetThreadDpiContext.Call(perMonitorAwareV2)
	var inst windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &inst)
	bar.inst = uintptr(inst)
	bar.self = windows.GetCurrentProcessId()
	ctrlClass, _ := windows.UTF16PtrFromString("ClaudeAccountSwitcherTaskbar")
	bar.class, _ = windows.UTF16PtrFromString("ClaudeAccountSwitcherBar")
	registerClass(ctrlClass, barCtrlProc)
	registerClass(bar.class, barWindowProc)
	name, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(name)))
	bar.created = uint32(r)
	bar.dc, _, _ = procCreateCompatibleDC.Call(0)

	ctrl, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(ctrlClass)), 0, wsPopup, 0, 0, 0, 0, 0, 0, bar.inst, 0)
	if ctrl == 0 {
		log.Printf("taskbar: create window: %v", err)
		taskbar.mu.Lock()
		taskbar.started = false
		taskbar.mu.Unlock()
		return
	}
	procSetTimer.Call(ctrl, barTimerID, barTimerMillis, 0)
	taskbar.mu.Lock()
	taskbar.ctrl = ctrl
	taskbar.mu.Unlock()
	syncBar(true)

	var m struct {
		Hwnd           uintptr
		Message        uint32
		WParam, LParam uintptr
		Time           uint32
		Pt             struct{ X, Y int32 }
		Private        uint32
	}
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func registerClass(name *uint16, proc uintptr) {
	cursor, _, _ := procLoadCursor.Call(0, 32512) // IDC_ARROW
	wc := struct {
		Size, Style                        uint32
		WndProc                            uintptr
		ClsExtra, WndExtra                 int32
		Instance, Icon, Cursor, Background uintptr
		MenuName, ClassName                *uint16
		IconSm                             uintptr
	}{WndProc: proc, Instance: bar.inst, Cursor: cursor, ClassName: name}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
}

func taskbarCtrlProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch uint32(msg) {
	case wmBarSync, wmTimer:
		syncBar(false)
		return 0
	case bar.created:
		// Explorer restarted; the bar went with the old taskbar.
		bar.hwnd, bar.tip, bar.shown, bar.tracking = 0, 0, -1, false
		syncBar(true)
		return 0
	case wmSettingChange, wmDisplayChange:
		syncBar(true)
	}
	r, _, _ := procDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

func taskbarBarProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch uint32(msg) {
	case wmMouseActivate:
		return maNoActivate
	case wmMouseMove:
		// The tooltip follows the mouse from account to account once it
		// shows; before that it waits for the mouse to rest on one.
		i := barItemAt(lp)
		if !bar.tracking || (bar.shown < 0 && i != bar.hover) {
			trackMouse(hwnd)
		}
		bar.hover = i
		if bar.shown >= 0 && i != bar.shown {
			showTip(i)
		}
		return 0
	case wmMouseHover:
		bar.hover = barItemAt(lp)
		showTip(bar.hover)
		return 0
	case wmMouseLeave:
		bar.tracking, bar.hover = false, -1
		hideTip()
		return 0
	case wmLButtonDown:
		hideTip()
	case wmNCDestroy:
		if hwnd == bar.hwnd {
			bar.hwnd = 0
		}
	}
	r, _, _ := procDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

// syncBar brings the bar in line with what the menu wants. Without force it
// only paints again when something it shows or where it sits has changed.
func syncBar(force bool) {
	taskbar.mu.Lock()
	items := taskbar.items
	taskbar.mu.Unlock()
	tb := findWindow("Shell_TrayWnd", 0)
	if len(items) == 0 || tb == 0 {
		destroyBar()
		return
	}
	if bar.hwnd != 0 && (!windows.IsWindow(windows.HWND(bar.hwnd)) || bar.tb != tb) {
		destroyBar()
	}
	tbr := windowRect(tb)
	h := tbr.Bottom - tbr.Top
	if h <= 0 || tbr.Right-tbr.Left < h {
		// No room on a taskbar this small, or one at the side of the screen.
		destroyBar()
		return
	}
	dpi, _, _ := procGetDpiForWindow.Call(tb)
	if dpi == 0 {
		dpi = 96
	}
	light := taskbarLight()
	lay := newBarLayout(h, int32(dpi), len(items))
	edge := barEdge(tb, tbr, lay.width)
	key := fmt.Sprint(tb, tbr, dpi, light, edge, items)
	if !force && key == bar.key && bar.hwnd != 0 {
		return
	}
	if bar.hwnd == 0 && !createBar(tb) {
		return
	}
	pt := struct{ X, Y int32 }{edge - lay.width, tbr.Top}
	procScreenToClient.Call(tb, uintptr(unsafe.Pointer(&pt)))
	procSetWindowPos.Call(bar.hwnd, 0, uintptr(pt.X), uintptr(pt.Y), uintptr(lay.width), uintptr(h), swpNoActivate|swpShowWindow)
	if !paintBar(items, lay, light) {
		return
	}
	setTips(items, lay)
	bar.key = key
}

func createBar(tb uintptr) bool {
	hwnd, _, err := procCreateWindowEx.Call(wsExLayered|wsExNoActivate, uintptr(unsafe.Pointer(bar.class)), 0,
		wsChild|wsVisible|wsClipSiblings, 0, 0, 1, 1, tb, 0, bar.inst, 0)
	if hwnd == 0 {
		log.Printf("taskbar: create bar: %v", err)
		return false
	}
	bar.hwnd, bar.tb = hwnd, tb
	cls, _ := windows.UTF16PtrFromString("tooltips_class32")
	bar.tip, _, _ = procCreateWindowEx.Call(wsExTopmost, uintptr(unsafe.Pointer(cls)), 0,
		wsPopup|ttsAlwaysTip|ttsNoPrefix, 0, 0, 0, 0, hwnd, 0, bar.inst, 0)
	if bar.tip != 0 {
		procSendMessage.Call(bar.tip, ttmSetMaxTipW, 0, 600)
		// One tool the bar moves and shows itself, see showTip.
		ti := barTool(" ")
		procSendMessage.Call(bar.tip, ttmAddTool, 0, uintptr(unsafe.Pointer(&ti)))
	}
	bar.hover, bar.shown, bar.tracking = -1, -1, false
	return true
}

func destroyBar() {
	if bar.tip != 0 {
		procDestroyWindow.Call(bar.tip)
	}
	if bar.hwnd != 0 {
		procDestroyWindow.Call(bar.hwnd)
	}
	bar.hwnd, bar.tip, bar.key = 0, 0, ""
	bar.hover, bar.shown, bar.tracking = -1, -1, false
}

// barEdge returns the screen x the bar's right edge goes to: left of the
// notification area and of whatever other apps put on the taskbar there.
func barEdge(tb uintptr, tbr windows.Rect, width int32) int32 {
	edge := tbr.Right
	if n := findWindow("TrayNotifyWnd", tb); n != 0 {
		if r := windowRect(n); r.Right > r.Left && r.Left > tbr.Left {
			edge = r.Left
		}
	}
	_, _ = windows.GetWindowThreadProcessId(windows.HWND(tb), &bar.explorer)
	bar.others = bar.others[:0]
	windows.EnumChildWindows(windows.HWND(tb), barEnumProc, nil)
	for moved := true; moved; {
		moved = false
		for _, r := range bar.others {
			if r.Left < edge && r.Right > edge-width {
				edge, moved = r.Left, true
			}
		}
	}
	return edge
}

func taskbarEnumChild(hwnd windows.HWND, _ uintptr) uintptr {
	var pid uint32
	_, _ = windows.GetWindowThreadProcessId(hwnd, &pid)
	if pid == bar.explorer || pid == bar.self || !windows.IsWindowVisible(hwnd) {
		return 1
	}
	if r := windowRect(uintptr(hwnd)); r.Right > r.Left && r.Bottom > r.Top {
		bar.others = append(bar.others, r)
	}
	return 1
}

// barLayout is the bar's geometry in pixels for one taskbar height and DPI.
type barLayout struct {
	h, icon, pad, gap, gap2   int32
	labelW, numW, line, itemW int32
	width                     int32
}

var barLabels = [2]string{"5h", "7d"}

func newBarLayout(h, dpi int32, n int) barLayout {
	s := float64(dpi) / 96
	px := func(v float64) int32 { return int32(math.Round(v)) }
	l := barLayout{h: h,
		icon: min(px(float64(h)*0.66), px(26*s)),
		pad:  px(6 * s), gap: px(6 * s), gap2: px(4 * s),
	}
	setFont(min(px(12*s), px(float64(h)*0.34)))
	w0, line := textSize(barLabels[0])
	w1, _ := textSize(barLabels[1])
	l.labelW, l.line = max(w0, w1), line
	l.numW, _ = textSize("100%")
	l.itemW = l.pad + l.icon + l.gap + l.labelW + l.gap2 + l.numW + l.pad
	l.width = l.itemW * int32(n)
	return l
}

func setFont(px int32) {
	if px == bar.fontPx && bar.font != 0 {
		return
	}
	if bar.font != 0 {
		procDeleteObject.Call(bar.font)
	}
	face, _ := windows.UTF16PtrFromString("Segoe UI")
	const fwNormal, defaultCharset, antialiasedQuality = 400, 1, 4
	bar.font, _, _ = procCreateFont.Call(uintptr(-px), 0, 0, 0, fwNormal, 0, 0, 0, defaultCharset, 0, 0, antialiasedQuality, 0,
		uintptr(unsafe.Pointer(face)))
	bar.fontPx = px
	procSelectObject.Call(bar.dc, bar.font)
}

func textSize(s string) (w, h int32) {
	u, _ := windows.UTF16FromString(s)
	var sz struct{ CX, CY int32 }
	procGetTextExtentPoint32.Call(bar.dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&sz)))
	return sz.CX, sz.CY
}

// textRun is a piece of text and how opaque it is drawn.
type textRun struct {
	text  string
	x, y  int32
	alpha float64
}

// paintBar draws the bar and hands it to the window. Text is drawn white on
// black by GDI first, which gives its coverage, then put in the theme's
// color with that coverage as alpha: GDI text itself has no alpha.
func paintBar(items []barItem, l barLayout, light bool) bool {
	w, h := l.width, l.h
	maskBits, mask := newDIB(w, h)
	if mask == 0 {
		return false
	}
	defer procDeleteObject.Call(mask)
	outBits, out := newDIB(w, h)
	if out == 0 {
		return false
	}
	defer procDeleteObject.Call(out)
	// A layered window lets the mouse through where it is fully
	// transparent, which would leave the tooltip to the gaps between glyphs.
	// An alpha of 1 is invisible and keeps the whole bar under the mouse.
	for i := 3; i < len(outBits); i += 4 {
		outBits[i] = 1
	}

	y0 := (h - 2*l.line) / 2
	if y0 < 0 {
		y0 = 0
	}
	y1 := min(y0+l.line, h-l.line)
	var runs []textRun
	for i, it := range items {
		x := int32(i)*l.itemW + l.pad + l.icon + l.gap
		for j, pct := range []float64{it.Session, it.Weekly} {
			y := []int32{y0, y1}[j]
			num := pctText(pct)
			nw, _ := textSize(num)
			runs = append(runs,
				textRun{barLabels[j], x, y, 0.65},
				textRun{num, x + l.labelW + l.gap2 + l.numW - nw, y, 1})
		}
	}

	old, _, _ := procSelectObject.Call(bar.dc, mask)
	procSetBkMode.Call(bar.dc, transparentBkgnd)
	procSetTextColor.Call(bar.dc, 0xFFFFFF)
	for _, r := range runs {
		u, _ := windows.UTF16FromString(r.text)
		procTextOut.Call(bar.dc, uintptr(r.x), uintptr(r.y), uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1))
	}

	var ink float64 // the text's gray level, 0 black to 1 white
	if !light {
		ink = 1
	}
	stride := int(w) * 4
	put := func(x, y int, gray, a float64) {
		if x < 0 || y < 0 || x >= int(w) || y >= int(h) || a <= 0 {
			return
		}
		v := uint8(gray*a*255 + 0.5)
		i := y*stride + x*4
		outBits[i], outBits[i+1], outBits[i+2], outBits[i+3] = v, v, v, uint8(a*255+0.5)
	}
	for _, r := range runs {
		rw, rh := textSize(r.text)
		for y := r.y; y < r.y+rh && y < h; y++ {
			for x := r.x; x < r.x+rw && x < w; x++ {
				put(int(x), int(y), ink, float64(maskBits[int(y)*stride+int(x)*4+2])/255*r.alpha)
			}
		}
	}
	for i, it := range items {
		ix, iy := int(int32(i)*l.itemW+l.pad), int((h-l.icon)/2)
		img := drawRings(int(l.icon), it.Session, it.Weekly, it.Threshold)
		for y := 0; y < int(l.icon); y++ {
			for x := 0; x < int(l.icon); x++ {
				p := img.NRGBAAt(x, y)
				if p.A == 0 {
					continue
				}
				a := float64(p.A) / 255
				j := (iy+y)*stride + (ix+x)*4
				outBits[j] = uint8(float64(p.B)*a + 0.5)
				outBits[j+1] = uint8(float64(p.G)*a + 0.5)
				outBits[j+2] = uint8(float64(p.R)*a + 0.5)
				outBits[j+3] = p.A
			}
		}
		if it.Marked {
			// Like the taskbar's own mark under a running app, in Windows'
			// default blue for the taskbar's theme.
			blue := [3]byte{0xFF, 0xC2, 0x4C} // #4CC2FF, BGR
			if light {
				blue = [3]byte{0xB8, 0x5F, 0x00} // #005FB8
			}
			s := float64(l.icon) / 26
			mw, mh := int(float64(l.icon)*0.4), max(2, int(math.Round(2*s)))
			mx, my := ix+(int(l.icon)-mw)/2, int(h)-mh-max(1, int(math.Round(s)))
			for y := max(my, 0); y < min(my+mh, int(h)); y++ {
				for x := mx; x < mx+mw; x++ {
					j := y*stride + x*4
					outBits[j], outBits[j+1], outBits[j+2], outBits[j+3] = blue[0], blue[1], blue[2], 0xFF
				}
			}
		}
	}

	procSelectObject.Call(bar.dc, out)
	size := struct{ CX, CY int32 }{w, h}
	var src struct{ X, Y int32 }
	blend := [4]byte{0, 0, 255, 1} // AC_SRC_OVER, opaque, AC_SRC_ALPHA
	ok, _, err := procUpdateLayeredWindow.Call(bar.hwnd, 0, 0, uintptr(unsafe.Pointer(&size)), bar.dc,
		uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	procSelectObject.Call(bar.dc, old)
	if ok == 0 {
		log.Printf("taskbar: update window: %v", err)
		return false
	}
	return true
}

func pctText(pct float64) string {
	if pct < 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// newDIB makes a top-down 32-bit bitmap and returns its pixels, BGRA.
func newDIB(w, h int32) ([]byte, uintptr) {
	bi := struct {
		Size                  uint32
		Width, Height         int32
		Planes, BitCount      uint16
		Compression, SizeImg  uint32
		XPels, YPels          int32
		ClrUsed, ClrImportant uint32
	}{Width: w, Height: -h, Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(bar.dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return nil, 0
	}
	return unsafe.Slice((*byte)(bits), int(w)*int(h)*4), hbm
}

// setTips keeps every account's tooltip text, and updates the tooltip
// in place when it shows.
func setTips(items []barItem, l barLayout) {
	bar.tipTexts = bar.tipTexts[:0]
	for _, it := range items {
		bar.tipTexts = append(bar.tipTexts, it.Tip)
	}
	bar.itemW = l.itemW
	if bar.shown >= 0 {
		showTip(bar.shown)
	}
}

type toolInfo struct {
	Size, Flags uint32
	Hwnd, ID    uintptr
	Rect        windows.Rect
	Inst        uintptr
	Text        *uint16
	Param       uintptr
}

// barTool is the bar's one tooltip tool. It is placed by hand: the taskbar
// stays above other topmost windows near it, so a tooltip at the mouse,
// where Windows would put it, ends up under the taskbar.
func barTool(text string) toolInfo {
	ti := toolInfo{Flags: ttfTrack | ttfAbsolute, Hwnd: bar.hwnd}
	ti.Size = uint32(unsafe.Sizeof(ti))
	ti.Text, _ = windows.UTF16PtrFromString(text)
	return ti
}

func trackMouse(hwnd uintptr) {
	tme := struct {
		Size, Flags uint32
		Hwnd        uintptr
		HoverTime   uint32
	}{Flags: tmeHover | tmeLeave, Hwnd: hwnd, HoverTime: hoverMillis}
	tme.Size = uint32(unsafe.Sizeof(tme))
	procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
	bar.tracking = true
}

// barItemAt returns the account under a mouse message's position.
func barItemAt(lp uintptr) int {
	x := int32(int16(lp & 0xFFFF))
	if bar.itemW <= 0 || x < 0 {
		return -1
	}
	if i := int(x / bar.itemW); i < len(bar.tipTexts) {
		return i
	}
	return -1
}

// showTip shows account i's tooltip just above the taskbar, over it.
func showTip(i int) {
	if bar.tip == 0 || i < 0 || i >= len(bar.tipTexts) {
		hideTip()
		return
	}
	ti := barTool(bar.tipTexts[i])
	procSendMessage.Call(bar.tip, ttmUpdateTipText, 0, uintptr(unsafe.Pointer(&ti)))
	r := windowRect(bar.hwnd)
	x := r.Left + int32(i)*bar.itemW
	if bar.shown < 0 {
		// Well above the taskbar until its size is known, so it never
		// flashes over the bar.
		procSendMessage.Call(bar.tip, ttmTrackPosition, 0, pointParam(x, r.Top-3*(r.Bottom-r.Top)))
		procSendMessage.Call(bar.tip, ttmTrackActivate, 1, uintptr(unsafe.Pointer(&ti)))
	}
	bar.shown = i
	// Its size is known once it has the text.
	tr := windowRect(bar.tip)
	w, h := tr.Right-tr.Left, tr.Bottom-tr.Top
	const monitorDefaultToNearest = 2
	mon, _, _ := procMonitorFromWindow.Call(bar.hwnd, monitorDefaultToNearest)
	var mi struct {
		Size          uint32
		Monitor, Work windows.Rect
		Flags         uint32
	}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if ok, _, _ := procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
		x = max(mi.Monitor.Left, min(x, mi.Monitor.Right-w))
	}
	y := r.Top - h - (r.Bottom-r.Top)/6
	procSendMessage.Call(bar.tip, ttmTrackPosition, 0, pointParam(x, y))
	const hwndTopmost, swpNoSize, swpNoMove = ^uintptr(0), 0x1, 0x2
	procSetWindowPos.Call(bar.tip, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpNoActivate)
}

func hideTip() {
	if bar.tip != 0 && bar.shown >= 0 {
		ti := barTool("")
		procSendMessage.Call(bar.tip, ttmTrackActivate, 0, uintptr(unsafe.Pointer(&ti)))
	}
	bar.shown = -1
}

func pointParam(x, y int32) uintptr {
	return uintptr(uint16(x)) | uintptr(uint16(y))<<16
}

func findWindow(class string, parent uintptr) uintptr {
	c, _ := windows.UTF16PtrFromString(class)
	if parent == 0 {
		h, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(c)), 0)
		return h
	}
	h, _, _ := procFindWindowEx.Call(parent, 0, uintptr(unsafe.Pointer(c)), 0)
	return h
}

func windowRect(hwnd uintptr) windows.Rect {
	var r windows.Rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// taskbarLight reports whether the taskbar uses the light theme, which
// follows the system's theme rather than the apps'.
func taskbarLight() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	return err == nil && v == 1
}
