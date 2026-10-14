package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
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
		var text strings.Builder
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&text, "Реплика %d\r\n", i)
		}
		u.replaceTranscriptText(text.String())
		first, _, _ := sendMessage.Call(h, 0xce, 0, 0)
		if first < 100 {
			t.Fatal("new text did not scroll to bottom", first)
		}
		sendMessage.Call(h, 0xb6, 0, uintptr(0)-first+20)
		before, _, _ := sendMessage.Call(h, 0xce, 0, 0)
		u.replaceTranscriptText(text.String() + "Новая фраза\r\n")
		after, _, _ := sendMessage.Call(h, 0xce, 0, 0)
		if before != after {
			t.Fatalf("reading position changed: %d -> %d", before, after)
		}
		call("DestroyWindow", parent)
		gdi("DeleteObject", u.font)
		u.theme.close()
	}
}
