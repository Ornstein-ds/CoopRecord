package main

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestTranscriptPanelScrollAndBounds(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release := activateVisualStyles()
	defer release()
	for _, scale := range []float64{0.75, 1, 1.5} {
		parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 2500, 1700, 0, 0, 0, 0)
		if parent == 0 {
			t.Fatal("hidden window creation failed")
		}
		u := &windowUI{hwnd: parent, scale: scale, controls: map[int]uintptr{}}
		u.theme = uiTheme{enabled: true, children: map[uintptr]string{}, fields: map[uintptr]rect{}}
		u.font = gdi("CreateFontW", uintptr(int32(-13*scale)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
		u.createTranscript(settings{})
		h := u.controls[idTranscript]
		var bounds rect
		call("GetWindowRect", h, uintptr(unsafe.Pointer(&bounds)))
		call("MapWindowPoints", 0, parent, uintptr(unsafe.Pointer(&bounds)), 2)
		if bounds.Right > int32(u.px(windowWidth)) || bounds.Bottom > int32(u.px(windowHeight)) {
			t.Fatal("panel outside window", bounds)
		}
		if call("GetWindowLongPtrW", h, ^uintptr(15))&0x100 == 0 {
			t.Fatal("timeline cannot receive mouse input")
		}
		if code, _, _ := sendMessage.Call(h, 0x87, 0, 0); code&1 == 0 {
			t.Fatal("timeline arrow keys escape into the dialog")
		}
		var lines []transcriptLine
		for i := 0; i < 100; i++ {
			stamp := int64(i+1) * int64(time.Second)
			lines = append(lines, transcriptLine{ID: uint64(i + 1), Source: i % 3, Name: "Участник", At: stamp, EndAt: stamp + int64(time.Second), Time: stamp, Text: "Реплика участника"})
		}
		u.timeline.update(lines)
		if u.timeline.y < 100 {
			t.Fatal("new text did not scroll to bottom", u.timeline.y)
		}
		u.timeline.y = 200
		u.timeline.scrollbars()
		before := u.timeline.y
		lines = append(lines, transcriptLine{ID: 101, Source: 0, At: int64(102 * time.Second), EndAt: int64(103 * time.Second), Text: "Новая фраза"})
		u.timeline.update(lines)
		if before != u.timeline.y {
			t.Fatalf("reading position changed: %d -> %d", before, u.timeline.y)
		}
		u.timeline.scroll(true, 7)
		if u.timeline.x == 0 {
			t.Fatal("speaker columns cannot scroll horizontally")
		}
		// Exercise the native paint callback with all clipping and GDI resources.
		call("RedrawWindow", h, 0, 0, 0x101)

		call("DestroyWindow", parent)
		gdi("DeleteObject", u.font)
		u.theme.close()
	}
}
