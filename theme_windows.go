//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

var dwm = syscall.NewLazyDLL("dwmapi.dll")
var themes = syscall.NewLazyDLL("uxtheme.dll")
var common = syscall.NewLazyDLL("comctl32.dll")
var themeSubclass uintptr

func init() { themeSubclass = syscall.NewCallback(themedControlProc) }

// Activate the installed v6 common controls without shipping another runtime or DLL.
func activateVisualStyles() func() {
	file, err := os.CreateTemp("", "CoopRecord-theme-*.manifest")
	if err != nil {
		return func() {}
	}
	name := file.Name()
	_, err = file.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0"><assemblyIdentity version="1.0.0.0" processorArchitecture="amd64" name="CoopRecord" type="win32"/><dependency><dependentAssembly><assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="amd64" publicKeyToken="6595b64144ccf1df" language="*"/></dependentAssembly></dependency></assembly>`)
	file.Close()
	if err != nil {
		os.Remove(name)
		return func() {}
	}
	ctx := struct {
		Size, Flags              uint32
		Source                   *uint16
		Arch, Lang               uint16
		Directory, Resource, App *uint16
		Module                   uintptr
	}{Source: wide(name)}
	ctx.Size = uint32(unsafe.Sizeof(ctx))
	h, _, _ := kernel.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&ctx)))
	if h == ^uintptr(0) {
		os.Remove(name)
		return func() {}
	}
	var cookie uintptr
	ok, _, _ := kernel.NewProc("ActivateActCtx").Call(h, uintptr(unsafe.Pointer(&cookie)))
	return func() {
		if ok != 0 {
			kernel.NewProc("DeactivateActCtx").Call(0, cookie)
		}
		kernel.NewProc("ReleaseActCtx").Call(h)
		os.Remove(name)
	}
}

type uiTheme struct {
	enabled, dark, contrast, glass                        bool
	preference                                            string
	bg, field, text, muted, line, accent, onAccent, hover uint32
	bgBrush, fieldBrush                                   uintptr
	children                                              map[uintptr]string
	fields                                                map[uintptr]rect
	cards                                                 []rect
	hovered                                               uintptr
}

func rgb(hex uint32) uint32 { return hex>>16 | hex&0xff00 | (hex&0xff)<<16 }
func gdi(name string, args ...uintptr) uintptr {
	r, _, _ := gdi32.NewProc(name).Call(args...)
	return r
}

func windowsDark(value string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	n, _, err := k.GetIntegerValue(value)
	return err == nil && n == 0
}

func darkTheme(preference string, systemDark bool) bool {
	return preference == "dark" || (preference != "light" && systemDark)
}

// Save only this preference, even if the microphone/session form is incomplete.
func saveThemePreference(preference string) error {
	path, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(path, "CoopRecord")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file := filepath.Join(dir, "settings.json")
	values := make(map[string]json.RawMessage)
	b, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &values); err != nil {
			return err
		}
	}
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	values["Theme"], _ = json.Marshal(preference)
	b, err = json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, b, 0600)
}

func (t *uiTheme) close() {
	if t.bgBrush != 0 {
		gdi("DeleteObject", t.bgBrush)
		t.bgBrush = 0
	}
	if t.fieldBrush != 0 {
		gdi("DeleteObject", t.fieldBrush)
		t.fieldBrush = 0
	}
}

func (u *windowUI) applyTheme() {
	t := &u.theme
	t.dark = darkTheme(t.preference, windowsDark("AppsUseLightTheme"))
	hc := struct {
		Size, Flags uint32
		Scheme      *uint16
	}{}
	hc.Size = uint32(unsafe.Sizeof(hc))
	call("SystemParametersInfoW", 0x42, uintptr(hc.Size), uintptr(unsafe.Pointer(&hc)), 0)
	t.contrast = hc.Flags&1 != 0
	t.bg, t.field, t.text, t.muted = rgb(0xf3f5f8), rgb(0xffffff), rgb(0x20232a), rgb(0x616a78)
	t.line, t.accent, t.onAccent, t.hover = rgb(0xd3d9e3), rgb(0x0067c0), rgb(0xffffff), rgb(0xe7effa)
	if t.dark {
		t.bg, t.field, t.text, t.muted = rgb(0x202020), rgb(0x2d2d2d), rgb(0xf1f4f9), rgb(0xa7b0bf)
		t.line, t.accent, t.onAccent, t.hover = rgb(0x414141), rgb(0x75baff), rgb(0x102439), rgb(0x393939)
	}
	if t.contrast {
		t.bg, t.field = uint32(call("GetSysColor", 15)), uint32(call("GetSysColor", 5))
		t.text, t.muted = uint32(call("GetSysColor", 8)), uint32(call("GetSysColor", 17))
		t.line, t.accent, t.onAccent, t.hover = t.text, uint32(call("GetSysColor", 13)), uint32(call("GetSysColor", 14)), t.field
	}
	t.close()
	t.bgBrush = gdi("CreateSolidBrush", uintptr(t.bg))
	t.fieldBrush = gdi("CreateSolidBrush", uintptr(t.field))
	attribute := func(id uintptr, value uint32) uintptr {
		r, _, _ := dwm.NewProc("DwmSetWindowAttribute").Call(u.hwnd, id, uintptr(unsafe.Pointer(&value)), 4)
		return r
	}
	frameDark := uint32(0)
	if t.dark {
		frameDark = 1
	}
	attribute(20, frameDark)
	attribute(33, 2)          // DWMWCP_ROUND
	attribute(34, 0xfffffffe) // no contrasting one-pixel outer border
	backdrop := uint32(3)     // Desktop Acrylic, Windows 11 22H2+.
	if t.contrast {
		backdrop = 1
	}
	t.glass = attribute(38, backdrop) == 0 && !t.contrast
	margin := int32(0)
	if t.glass {
		margin = int32(u.px(32))
	}
	margins := [4]int32{0, 0, margin, 0}
	dwm.NewProc("DwmExtendFrameIntoClientArea").Call(u.hwnd, uintptr(unsafe.Pointer(&margins)))
	for hwnd, class := range t.children {
		// Classic controls honor the documented CTLCOLOR palette in both themes.
		themes.NewProc("SetWindowTheme").Call(hwnd, uintptr(unsafe.Pointer(wide(""))), uintptr(unsafe.Pointer(wide(""))))
		if class == "COMBOBOX" {
			u.fitCombo(hwnd)
		}
		if class == "msctls_progress32" {
			sendMessage.Call(hwnd, 0x2001, 0, uintptr(t.line)) // PBM_SETBKCOLOR
			sendMessage.Call(hwnd, 0x409, 0, uintptr(t.accent))
		}
	}
	label := "Тёмная тема: выкл."
	if t.dark {
		label = "Тёмная тема: вкл."
	}
	setText(u.controls[idTheme], label)
	label = "Как в Windows"
	if t.preference == "" {
		label = "Тема Windows · автоматически"
	}
	setText(u.controls[idSystemTheme], label)
	call("RedrawWindow", u.hwnd, 0, 0, 0x485) // invalidate, erase, all children, frame
}

// A combo's collapsed height is derived from its item font, not CreateWindow's height (the popup height).
func (u *windowUI) fitCombo(hwnd uintptr) {
	field, ok := u.theme.fields[hwnd]
	if !ok {
		return
	}
	var bounds rect
	call("GetWindowRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
	item, _, _ := sendMessage.Call(hwnd, 0x154, ^uintptr(0), 0) // CB_GETITEMHEIGHT, selection
	target := field.Bottom - field.Top - 2
	height := max(1, int32(item)+target-(bounds.Bottom-bounds.Top))
	sendMessage.Call(hwnd, 0x153, ^uintptr(0), uintptr(height)) // CB_SETITEMHEIGHT
	call("GetWindowRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
	height = bounds.Bottom - bounds.Top
	call("SetWindowPos", hwnd, 0, uintptr(field.Left+int32(u.px(6))), uintptr(field.Top+(field.Bottom-field.Top-height)/2), 0, 0, 0x15)
}

func (u *windowUI) themeControl(hwnd uintptr, class string) {
	if u.theme.fields == nil {
		u.theme.fields = make(map[uintptr]rect)
	}
	u.theme.children[hwnd] = class
	if class != "STATIC" {
		common.NewProc("SetWindowSubclass").Call(hwnd, themeSubclass, 1, 0)
	}
	if class == "msctls_progress32" || class == "EDIT" || class == "COMBOBOX" || class == "msctls_hotkey32" {
		style := call("GetWindowLongPtrW", hwnd, ^uintptr(15))
		ex := call("GetWindowLongPtrW", hwnd, ^uintptr(19))
		call("SetWindowLongPtrW", hwnd, ^uintptr(15), style&^0x800000)
		call("SetWindowLongPtrW", hwnd, ^uintptr(19), ex&^(0x200|0x20000))
		call("SetWindowPos", hwnd, 0, 0, 0, 0, 0, 0x27)
		if class == "EDIT" && getText(hwnd) == "" {
			call("ShowScrollBar", hwnd, 1, 0)
		}
	}
}

func hotkeyLabel(key uintptr) string {
	if key == 0 {
		return "Нет"
	}
	var parts []string
	for _, mod := range []struct {
		mask uintptr
		name string
	}{{2, "Ctrl"}, {4, "Alt"}, {1, "Shift"}} {
		if key>>8&mod.mask != 0 {
			parts = append(parts, mod.name)
		}
	}
	if key&255 != 0 {
		scan := call("MapVirtualKeyW", key&255, 0) << 16
		if key>>8&8 != 0 {
			scan |= 1 << 24
		}
		var name [64]uint16
		call("GetKeyNameTextW", scan, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		parts = append(parts, syscall.UTF16ToString(name[:]))
	}
	return strings.Join(parts, " + ")
}

type paintInfo struct {
	DC              uintptr
	Erase           int32
	Rect            rect
	Restore, Update int32
	Reserved        [32]byte
}

func fillRect(dc uintptr, r rect, color uint32) {
	brush := gdi("CreateSolidBrush", uintptr(color))
	call("FillRect", dc, uintptr(unsafe.Pointer(&r)), brush)
	gdi("DeleteObject", brush)
}

// Present a complete control in one blit, never its cleared background or a half-drawn value.
func bufferControl(dc uintptr, r rect) (uintptr, func()) {
	mem := gdi("CreateCompatibleDC", dc)
	bitmap := gdi("CreateCompatibleBitmap", dc, uintptr(r.Right), uintptr(r.Bottom))
	if mem == 0 || bitmap == 0 {
		if mem != 0 {
			gdi("DeleteDC", mem)
		}
		if bitmap != 0 {
			gdi("DeleteObject", bitmap)
		}
		return dc, func() {}
	}
	old := gdi("SelectObject", mem, bitmap)
	return mem, func() {
		gdi("BitBlt", dc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), mem, uintptr(r.Left), uintptr(r.Top), 0xcc0020)
		gdi("SelectObject", mem, old)
		gdi("DeleteObject", bitmap)
		gdi("DeleteDC", mem)
	}
}

func rounded(dc uintptr, r rect, radius int32, fill, border uint32) {
	brush, pen := gdi("CreateSolidBrush", uintptr(fill)), gdi("CreatePen", 0, 1, uintptr(border))
	oldBrush, oldPen := gdi("SelectObject", dc, brush), gdi("SelectObject", dc, pen)
	gdi("RoundRect", dc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom), uintptr(radius), uintptr(radius))
	gdi("SelectObject", dc, oldPen)
	gdi("SelectObject", dc, oldBrush)
	gdi("DeleteObject", pen)
	gdi("DeleteObject", brush)
}

func drawText(dc, font uintptr, text string, r rect, color uint32, flags uintptr) {
	old := gdi("SelectObject", dc, font)
	gdi("SetBkMode", dc, 1)
	gdi("SetTextColor", dc, uintptr(color))
	call("DrawTextW", dc, uintptr(unsafe.Pointer(wide(text))), ^uintptr(0), uintptr(unsafe.Pointer(&r)), flags|0x800) // no ampersand mnemonics
	gdi("SelectObject", dc, old)
}

type drawItem struct {
	Type, ID, Item, Action, State uint32
	Window, DC                    uintptr
	Rect                          rect
	Data                          uintptr
}

func (u *windowUI) drawButton(d *drawItem) {
	copy := *d
	d = &copy
	dc, present := bufferControl(d.DC, d.Rect)
	defer present()
	d.DC = dc
	t, r := &u.theme, d.Rect
	fillRect(d.DC, r, t.bg)
	r.Left++
	r.Top++
	r.Right--
	r.Bottom--
	color, text, border := t.field, t.text, t.line
	if d.Window == t.hovered {
		color, border = t.hover, t.accent
	}
	if d.State&1 != 0 {
		color = t.line
	}
	if d.ID == idConnect || d.ID == idRecord || (d.ID == idDesktopMute && u.e.desktopMuted.Load()) {
		color, text, border = t.accent, t.onAccent, t.accent
	}
	if d.State&4 != 0 {
		color, text, border = t.bg, t.muted, t.line
	}
	if d.ID == idDesktopMute && !u.e.desktopMuted.Load() {
		color, text = rgb(0xc42b3e), rgb(0xffffff)
		if t.dark {
			color = rgb(0xd13b4d)
		}
		if t.contrast {
			color, text = t.accent, t.onAccent
		}
		border = color
	}
	radius := int32(u.px(10))
	rounded(d.DC, r, radius, color, border)
	if d.ID == idTheme {
		pill := rect{r.Left + int32(u.px(12)), r.Top + int32(u.px(6)), r.Left + int32(u.px(46)), r.Bottom - int32(u.px(6))}
		fill := t.muted
		if t.dark {
			fill = t.accent
		}
		rounded(d.DC, pill, pill.Bottom-pill.Top, fill, fill)
		x := pill.Left + 3
		if t.dark {
			x = pill.Right - (pill.Bottom - pill.Top) + 3
		}
		knob := rect{x, pill.Top + 3, x + (pill.Bottom - pill.Top) - 6, pill.Bottom - 3}
		rounded(d.DC, knob, knob.Bottom-knob.Top, rgb(0xffffff), rgb(0xffffff))
		r.Left += int32(u.px(49))
	}
	flags := uintptr(1 | 4 | 0x20) // center, vcenter, single line
	label := getText(d.Window)
	if d.ID >= idPadFirst && d.ID < idPadFirst+padCount {
		flags = 1 | 0x10 // word wrap; vertically center using measured height
		measure := r
		old := gdi("SelectObject", d.DC, u.font)
		call("DrawTextW", d.DC, uintptr(unsafe.Pointer(wide(label))), ^uintptr(0), uintptr(unsafe.Pointer(&measure)), flags|0x400|0x800)
		gdi("SelectObject", d.DC, old)
		r.Top += max(0, (r.Bottom-r.Top-(measure.Bottom-measure.Top))/2)
	}
	drawText(d.DC, u.font, label, r, text, flags)
	if d.State&0x10 != 0 && d.State&0x200 == 0 {
		focus := d.Rect
		focus.Left += 4
		focus.Top += 4
		focus.Right -= 4
		focus.Bottom -= 4
		call("DrawFocusRect", d.DC, uintptr(unsafe.Pointer(&focus)))
	}
}

func (u *windowUI) themeMessage(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool) {
	t := &u.theme
	switch msg {
	case 0x83: // The title area belongs to the client; DWM retains its caption buttons.
		if w != 0 {
			return 0, true
		}
	case 0x84:
		var result uintptr
		handled, _, _ := dwm.NewProc("DwmDefWindowProc").Call(hwnd, uintptr(msg), w, l, uintptr(unsafe.Pointer(&result)))
		if handled != 0 {
			return result, true
		}
		point := struct{ X, Y int32 }{int32(int16(l & 0xffff)), int32(int16((l >> 16) & 0xffff))}
		call("ScreenToClient", hwnd, uintptr(unsafe.Pointer(&point)))
		if point.Y >= 0 && point.Y < int32(u.px(32)) {
			return 2, true
		} // HTCAPTION: drag/Alt+Space remain native.
		return 1, true
	case 0x2a2: // Let DWM clear hover on its native caption buttons.
		var result uintptr
		dwm.NewProc("DwmDefWindowProc").Call(hwnd, uintptr(msg), w, l, uintptr(unsafe.Pointer(&result)))
	case 0x14: // WM_ERASEBKGND
		var r rect
		call("GetClientRect", hwnd, uintptr(unsafe.Pointer(&r)))
		if t.glass {
			glass := r
			glass.Bottom = int32(u.px(32))
			var dc uintptr
			buffer, _, _ := themes.NewProc("BeginBufferedPaint").Call(w, uintptr(unsafe.Pointer(&glass)), 2, 0, uintptr(unsafe.Pointer(&dc)))
			if buffer != 0 {
				themes.NewProc("BufferedPaintClear").Call(buffer, 0)
				themes.NewProc("BufferedPaintSetAlpha").Call(buffer, 0, 0)
				themes.NewProc("EndBufferedPaint").Call(buffer, 1)
			} else {
				fillRect(w, glass, 0)
			}
			r.Top = int32(u.px(32))
		}
		fillRect(w, r, t.bg)
		for _, card := range t.cards {
			rounded(w, card, int32(u.px(14)), t.bg, t.line)
		}
		for child, field := range t.fields {
			line := t.line
			if call("GetFocus") == child {
				line = t.accent
			}
			rounded(w, field, int32(u.px(10)), t.field, line)
		}
		return 1, true
	case 0x133, 0x134, 0x135, 0x138: // edit, list, button, static colors
		color, brush := t.bg, t.bgBrush
		class := t.children[l]
		if msg == 0x133 || msg == 0x134 || class == "EDIT" || class == "COMBOBOX" || class == "msctls_hotkey32" {
			color, brush = t.field, t.fieldBrush
		}
		text := t.text
		if call("IsWindowEnabled", l) == 0 {
			text = t.muted
		}
		gdi("SetBkColor", w, uintptr(color))
		gdi("SetTextColor", w, uintptr(text))
		return brush, true
	case 0x1a, 0x320, 0x31e: // system preferences, theme, composition
		if u.hwnd != 0 {
			u.applyTheme()
		}
	}
	return 0, false
}

func themedControlProc(hwnd uintptr, msg uint32, w, l, id, data uintptr) uintptr {
	u := appUI
	if u != nil && u.theme.enabled {
		t := &u.theme
		class := t.children[hwnd]
		if msg == 0x14 && (class == "BUTTON" || class == "COMBOBOX" || class == "msctls_trackbar32" || class == "msctls_progress32" || class == "msctls_hotkey32") {
			return 1
		}
		if class == "BUTTON" {
			if msg == 0x200 && t.hovered != hwnd {
				t.hovered = hwnd
				track := struct {
					Size, Flags uint32
					Window      uintptr
					Time        uint32
				}{Flags: 2, Window: hwnd}
				track.Size = uint32(unsafe.Sizeof(track))
				call("TrackMouseEvent", uintptr(unsafe.Pointer(&track)))
				call("InvalidateRect", hwnd, 0, 0)
			}
			if msg == 0x2a3 {
				t.hovered = 0
				call("InvalidateRect", hwnd, 0, 0)
			}
		}
		if (class == "msctls_trackbar32" || class == "msctls_progress32" || class == "msctls_hotkey32") && msg == 0xf {
			var ps paintInfo
			dc := call("BeginPaint", hwnd, uintptr(unsafe.Pointer(&ps)))
			var r rect
			call("GetClientRect", hwnd, uintptr(unsafe.Pointer(&r)))
			dc, present := bufferControl(dc, r)
			fillRect(dc, r, t.bg)
			if class == "msctls_hotkey32" {
				fillRect(dc, r, t.field)
				key, _, _ := sendMessage.Call(hwnd, 0x402, 0, 0)
				r.Left += int32(u.px(5))
				drawText(dc, u.font, hotkeyLabel(key), r, t.text, 4|0x20)
			} else if class == "msctls_progress32" {
				n, _, _ := sendMessage.Call(hwnd, 0x408, 0, 0)
				rounded(dc, r, int32(u.px(6)), t.line, t.line)
				bar := r
				bar.Left += 2
				bar.Top += 2
				bar.Right -= 2
				bar.Bottom -= 2
				if r.Bottom > r.Right {
					bar.Top = bar.Bottom - (bar.Bottom-bar.Top)*int32(min(n, 100))/100
				} else {
					bar.Right = bar.Left + (bar.Right-bar.Left)*int32(min(n, 100))/100
				}
				if n > 0 {
					rounded(dc, bar, int32(u.px(4)), t.accent, t.accent)
				}
			} else {
				var thumb, track rect
				sendMessage.Call(hwnd, 0x419, 0, uintptr(unsafe.Pointer(&thumb))) // TBM_GETTHUMBRECT
				sendMessage.Call(hwnd, 0x41a, 0, uintptr(unsafe.Pointer(&track))) // TBM_GETCHANNELRECT
				x := (thumb.Left + thumb.Right) / 2
				// Native channel coordinates keep the travel axis in Left/Right even vertically.
				halfThumb := (thumb.Bottom - thumb.Top) / 2
				rail := rect{x - 2, track.Left + halfThumb, x + 2, track.Right - halfThumb}
				tick := func(y int32, unity bool) {
					mark := rect{thumb.Left - 6, y, thumb.Left - 2, y + 1}
					color := t.muted
					if unity {
						mark.Left = max(0, thumb.Left-10)
						mark.Top--
						color = t.text
					}
					fillRect(dc, mark, color)
				}
				tick(rail.Top, false)
				tick(rail.Bottom-1, false)
				count, _, _ := sendMessage.Call(hwnd, 0x410, 0, 0) // TBM_GETNUMTICS includes endpoints
				for i := uintptr(0); i+2 < count; i++ {
					y, _, _ := sendMessage.Call(hwnd, 0x40f, i, 0) // TBM_GETTICPOS
					value, _, _ := sendMessage.Call(hwnd, 0x403, i, 0)
					tick(int32(y), value == mixerUnityPosition)
				}
				rounded(dc, rail, 4, t.line, t.line)
				level := rail
				level.Top = (thumb.Top + thumb.Bottom) / 2
				accent := t.accent
				if call("IsWindowEnabled", hwnd) == 0 {
					accent = t.muted
				}
				rounded(dc, level, 4, accent, accent)
				rounded(dc, thumb, int32(u.px(10)), t.field, accent)
				if call("GetFocus") == hwnd {
					call("DrawFocusRect", dc, uintptr(unsafe.Pointer(&r)))
				}
			}
			present()
			call("EndPaint", hwnd, uintptr(unsafe.Pointer(&ps)))
			return 0
		}
		if class == "COMBOBOX" && msg == 0xf {
			var ps paintInfo
			dc := call("BeginPaint", hwnd, uintptr(unsafe.Pointer(&ps)))
			var bounds rect
			call("GetClientRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
			dc, present := bufferControl(dc, bounds)
			fillRect(dc, bounds, t.field)
			info := struct {
				Size              uint32
				Item, Button      rect
				State             uint32
				Combo, Edit, List uintptr
			}{}
			info.Size = uint32(unsafe.Sizeof(info))
			if call("GetComboBoxInfo", hwnd, uintptr(unsafe.Pointer(&info))) != 0 {
				var bounds rect
				call("GetClientRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
				fillRect(dc, bounds, t.field)
				index, _, _ := sendMessage.Call(hwnd, 0x147, 0, 0)
				if int32(index) >= 0 {
					length, _, _ := sendMessage.Call(hwnd, 0x149, index, 0)
					if length < 32768 {
						text := make([]uint16, length+1)
						sendMessage.Call(hwnd, 0x148, index, uintptr(unsafe.Pointer(&text[0])))
						drawText(dc, u.font, syscall.UTF16ToString(text), info.Item, t.text, 4|0x20|0x8000)
					}
				}
				fillRect(dc, info.Button, t.field)
				drawText(dc, u.font, "⌄", info.Button, t.text, 1|4|0x20)
			}
			present()
			call("EndPaint", hwnd, uintptr(unsafe.Pointer(&ps)))
			return 0
		}
		if msg == 0x82 {
			delete(t.children, hwnd)
			delete(t.fields, hwnd)
			common.NewProc("RemoveWindowSubclass").Call(hwnd, themeSubclass, id)
		}
	}
	r, _, _ := common.NewProc("DefSubclassProc").Call(hwnd, uintptr(msg), w, l)
	if u != nil && u.theme.enabled {
		if msg == 7 || msg == 8 {
			if field, ok := u.theme.fields[hwnd]; ok {
				call("InvalidateRect", u.hwnd, uintptr(unsafe.Pointer(&field)), 1)
			}
		}
		if u.theme.children[hwnd] == "EDIT" && msg == 0xc && call("GetWindowLongPtrW", hwnd, ^uintptr(15))&4 != 0 {
			var bounds rect
			call("GetClientRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
			lines, _, _ := sendMessage.Call(hwnd, 0xba, 0, 0)
			visible := uintptr(0)
			if int32(lines)*int32(u.px(15)) > bounds.Bottom {
				visible = 1
			}
			call("ShowScrollBar", hwnd, 1, visible)
		}
	}
	if u != nil && u.theme.enabled && u.theme.children[hwnd] == "msctls_hotkey32" && (msg == 0x100 || msg == 0x101 || msg == 0x104 || msg == 0x105 || msg == 0x401 || msg == 7 || msg == 8) {
		call("InvalidateRect", hwnd, 0, 1)
	}
	return r
}
