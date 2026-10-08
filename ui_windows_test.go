//go:build windows

package main

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

func guidedTestUI(t *testing.T, scale float64) *windowUI {
	t.Helper()
	syscall.NewLazyDLL("comctl32.dll").NewProc("InitCommonControls").Call()
	class := wide("CoopRecordGuidedTestWindow")
	wc := winClass{Size: uint32(unsafe.Sizeof(winClass{})), Proc: user32.NewProc("DefWindowProcW").Addr(), Background: 16, Class: class}
	call("RegisterClassExW", uintptr(unsafe.Pointer(&wc)))
	r := rect{Right: int32(1440 * scale), Bottom: int32(1035 * scale)}
	call("AdjustWindowRectEx", uintptr(unsafe.Pointer(&r)), 0x00c80000, 0, 0)
	parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(wide("CoopRecord — UI preview"))), 0x00c80000, 20, 20, uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0, 0, 0, 0)
	if parent == 0 {
		t.Fatal("cannot create guided UI test window")
	}
	u := &windowUI{hwnd: parent, controls: map[int]uintptr{}, scale: scale, e: newTestEngine(syntheticCapture)}
	font := func(size, weight int) uintptr {
		height := int32(-float64(size) * scale)
		h, _, _ := gdi32.NewProc("CreateFontW").Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
		return h
	}
	u.font, u.titleFont = font(14, 400), font(25, 600)
	t.Cleanup(func() {
		call("DestroyWindow", parent)
		gdi32.NewProc("DeleteObject").Call(u.font)
		gdi32.NewProc("DeleteObject").Call(u.titleFont)
	})
	u.createControls(settings{Name: "Алексей", Address: "0.0.0.0:47652", Folder: `C:\Podcasts`, Key: "preview-session-key"})
	u.devices = []inputDevice{{ID: "synthetic", Name: "Тестовый микрофон"}}
	sendMessage.Call(u.controls[idMic], 0x143, 0, uintptr(unsafe.Pointer(wide("Тестовый микрофон"))))
	sendMessage.Call(u.controls[idMic], 0x14e, 0, 0)
	u.savedDraft, u.savedPadKeys, u.draftReady = u.draft(), u.readPadKeys(), true
	u.update()
	return u
}

func TestGuidedControls(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	u := guidedTestUI(t, 1)
	if u.hasDraftChanges() {
		t.Fatal("untouched form must close without draft warning")
	}
	setText(u.controls[idName], "Мария")
	if !u.hasDraftChanges() {
		t.Fatal("changed name must be protected")
	}
	setText(u.controls[idName], "Алексей")
	if u.hasDraftChanges() {
		t.Fatal("reverted change must not warn")
	}
	u.padFiles[0] = "unsaved.wav"
	if !u.hasDraftChanges() {
		t.Fatal("failed sound settings save must remain protected")
	}
	u.padFiles[0] = ""
	sendMessage.Call(u.controls[idPadKeyFirst], 0x401, 0, 0)
	if !u.hasDraftChanges() {
		t.Fatal("unapplied hotkey must be protected")
	}
	visible := func(id int) bool { return call("GetWindowLongPtrW", u.controls[id], ^uintptr(15))&0x10000000 != 0 }
	for _, v := range []viewState{
		{Mode: "host", CanRecord: true},
		{Mode: "host", Recording: true},
		{Mode: "guest", Recording: true},
		{Mode: "host", Recording: true, Exporting: true},
	} {
		host := v.Mode == "host"
		u.updateGuidance(v, host)
		if visible(idRecord) != (host && !v.Recording) || visible(idStop) != (host && v.Recording) {
			t.Fatalf("wrong recording actions for %+v", v)
		}
		if v.Exporting && u.primaryAction != 0 {
			t.Fatal("export must have no next action")
		}
	}
	sendMessage.Call(u.controls[idRole], 0x14e, 1, 0)
	setText(u.controls[idAddress], "0.0.0.0:47652")
	if _, err := u.settings(); err == nil {
		t.Fatal("guest must not connect to wildcard address")
	}
	if getText(u.controls[idKey]) != "preview-session-key" {
		t.Fatal("validation changed key")
	}
	u.e.mode = "host"
	u.e.cfg.Name = "Алексей"
	u.update()
	if call("IsWindowEnabled", u.controls[idKey]) == 0 {
		t.Fatal("session key must remain available for copying")
	}
	u.editShortcut(u.controls[idKey], 'A')
	u.editShortcut(u.controls[idKey], 8)
	if getText(u.controls[idKey]) != "preview-session-key" {
		t.Fatal("Ctrl+Backspace changed read-only session key")
	}
}

// Opt-in visual fixture: actual Win32 controls, synthetic input, no network or
// microphone access, no persisted settings and no global hotkey registrations.
func TestGuidedLayoutPreview(t *testing.T) {
	scale, err := strconv.ParseFloat(os.Getenv("COOPRECORD_UI_PREVIEW"), 64)
	if err != nil || scale < 0.5 || scale > 2 {
		t.Skip("set COOPRECORD_UI_PREVIEW to a scale, e.g. 1 or 0.66")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	u := guidedTestUI(t, scale)
	call("ShowWindow", u.hwnd, 1)
	call("ShowWindow", u.hwnd, 1) // Also show when the test runner was launched hidden.
	call("UpdateWindow", u.hwnd)
	deadline := time.Now().Add(90 * time.Second)
	var m winMessage
	for time.Now().Before(deadline) {
		for call("PeekMessageW", uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1) != 0 {
			call("TranslateMessage", uintptr(unsafe.Pointer(&m)))
			call("DispatchMessageW", uintptr(unsafe.Pointer(&m)))
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestMixerControls(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	syscall.NewLazyDLL("comctl32.dll").NewProc("InitCommonControls").Call()
	parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 1440, 1035, 0, 0, 0, 0)
	if parent == 0 {
		t.Fatal("cannot create test window")
	}
	defer call("DestroyWindow", parent)
	u := &windowUI{hwnd: parent, controls: map[int]uintptr{}, scale: 1, e: newTestEngine(syntheticCapture)}
	u.createMixer()
	u.updateMixer()
	h := u.controls[idMixerFirst+1]
	if h == 0 {
		t.Fatal("no native fader")
	}
	for _, position := range []uintptr{0, 25, 100} {
		sendMessage.Call(h, 0x405, 1, position)
		u.mixerScroll(h)
		if got := u.e.padMixer.volume.Load(); got != int32(100-position) {
			t.Fatal("fader direction/gain", got)
		}
	}
	u.e.padMixer.meter(75)
	u.updateMixer()
	n, _, _ := sendMessage.Call(u.controls[idMixerFirst+2], 0x408, 0, 0)
	if n != 75 {
		t.Fatal("meter position", n)
	}
	u.updateMixer()
	n, _, _ = sendMessage.Call(u.controls[idMixerFirst+2], 0x408, 0, 0)
	if n != 0 {
		t.Fatal("silent meter retained stale peak", n)
	}
}

func TestEditShortcutsAndConnectionFields(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), uintptr(unsafe.Pointer(wide("CoopRecord UI test"))), 0, 0, 0, 720, 700, 0, 0, 0, 0)
	if parent == 0 {
		t.Fatal("cannot create hidden test window")
	}
	defer call("DestroyWindow", parent)
	u := &windowUI{hwnd: parent, controls: map[int]uintptr{}, scale: 1, e: newTestEngine(syntheticCapture), role: 1}
	for _, id := range []int{idName, idCorrection, idAddress, idKey, idFolder} {
		u.edit(id, "", 0, 0, 200)
		h := u.controls[id]
		setText(h, "Привет мир")
		if !u.editShortcut(h, 'A') {
			t.Fatal("shortcut ignored")
		}
		var first, last uint32
		sendMessage.Call(h, 0xB0, uintptr(unsafe.Pointer(&first)), uintptr(unsafe.Pointer(&last)))
		if first != 0 || last != 10 {
			t.Fatalf("Ctrl+A selection: %d..%d", first, last)
		}
		u.editShortcut(h, 8)
		if getText(h) != "" {
			t.Fatal("Ctrl+Backspace did not delete selection")
		}
		sendMessage.Call(h, 0xC7, 0, 0) // EM_UNDO
		if getText(h) != "Привет мир" {
			t.Fatal("word deletion is not undoable")
		}
		for _, test := range []struct{ before, after string }{
			{"Привет мир", "Привет "}, {"one two   ", "one "},
			{"26.12.34.56:47652", "26.12.34.56:"}, {"C:\\Мои записи\\подкаст", "C:\\Мои записи\\"},
			{"🎙️ голос", "🎙️ "}, {"hello 😀", "hello "}, {"", ""},
		} {
			setText(h, test.before)
			caret := uintptr(len(utf16.Encode([]rune(test.before))))
			sendMessage.Call(h, 0xB1, caret, caret)
			u.editShortcut(h, 8)
			if got := getText(h); got != test.after {
				t.Fatalf("field %d: Ctrl+Backspace %q = %q, want %q", id, test.before, got, test.after)
			}
		}
	}
	u.combo(idRole, []string{"Host", "Guest"}, 0, 0, 200)
	sendMessage.Call(u.controls[idRole], 0x14e, 1, 0)
	address, key := "26.10.20.30:47652", "keep-this-session-key"
	setText(u.controls[idAddress], address)
	setText(u.controls[idKey], key)
	u.connections[0].address, u.connections[0].key = "0.0.0.0:47652", "host-session-key"
	// Repeated selection notifications must not clear the already selected role.
	u.command(idRole, 1)
	u.e.disconnect()
	u.update()
	if getText(u.controls[idAddress]) != address || getText(u.controls[idKey]) != key {
		t.Fatal("connection cleanup reset input")
	}
	// Both roles retain their own draft, including a manually entered key.
	sendMessage.Call(u.controls[idRole], 0x14e, 0, 0)
	u.command(idRole, 1)
	if getText(u.controls[idKey]) != "host-session-key" {
		t.Fatal("host draft not restored")
	}
	sendMessage.Call(u.controls[idRole], 0x14e, 1, 0)
	u.command(idRole, 1)
	if getText(u.controls[idAddress]) != address || getText(u.controls[idKey]) != key {
		t.Fatal("guest draft lost after role switch")
	}
}
