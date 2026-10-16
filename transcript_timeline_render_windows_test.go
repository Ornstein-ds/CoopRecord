package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Optional offscreen renders use the actual Windows renderer without opening a
// window, capturing the desktop, or accessing microphone devices.
func TestTranscriptTimelineRender(t *testing.T) {
	dir := os.Getenv("COOPRECORD_TIMELINE_PREVIEW")
	if dir == "" {
		t.Skip("set COOPRECORD_TIMELINE_PREVIEW to render timeline PNGs")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release := activateVisualStyles()
	defer release()
	const width, height = 640, 640
	parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 1000, 1000, 0, 0, 0, 0)
	if parent == 0 {
		t.Fatal("cannot create offscreen window")
	}
	defer call("DestroyWindow", parent)
	u := &windowUI{hwnd: parent, scale: 1, controls: map[int]uintptr{}}
	fontHeight := int32(-14)
	u.font = gdi("CreateFontW", uintptr(fontHeight), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
	defer gdi("DeleteObject", u.font)
	hwnd := u.control(idTranscript, "STATIC", "", 0, 0, 0, width, height)
	timeline := newTranscriptTimeline(u, hwnd)
	second := int64(time.Second)
	lines := []transcriptLine{
		{ID: 1, Source: 0, Name: "Влад", At: second, EndAt: 9 * second, Time: 12 * second, Recording: "Подкаст", Text: "Давай разберём, как устроена транскрипция. Когда мы говорим одновременно, моя реплика остаётся целиком в своей колонке, а твой комментарий появляется рядом на той же шкале времени."},
		{ID: 2, Source: 1, Name: "Друг", At: 3 * second, EndAt: 5 * second, Time: 14 * second, Recording: "Подкаст", Text: "Да, теперь перебивка видна рядом, и читать намного удобнее."},
		{ID: 3, Source: 1, Name: "Друг", At: 6 * second, EndAt: 8 * second, Time: 17 * second, Recording: "Подкаст", Text: "И начало каждого фрагмента привязано к аудиозаписи."},
		{ID: 4, Source: 0, Name: "Влад", At: 10 * second, EndAt: 14 * second, Time: 21 * second, Recording: "Подкаст", Text: "А эта фраза ещё распознаётся. Текст будет уточняться в этой же карточке…", Partial: true},
	}
	timeline.update(lines)
	timeline.y = 0
	dc := gdi("CreateCompatibleDC", 0)
	defer gdi("DeleteDC", dc)
	info := struct {
		Size                   uint32
		Width, Height          int32
		Planes, Bits           uint16
		Compression, ImageSize uint32
		XPels, YPels           int32
		Used, Important        uint32
	}{Size: 40, Width: width, Height: -height, Planes: 1, Bits: 32}
	var bits unsafe.Pointer
	bitmap := gdi("CreateDIBSection", dc, uintptr(unsafe.Pointer(&info)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bitmap == 0 || bits == nil {
		t.Fatal("cannot create render bitmap")
	}
	defer gdi("DeleteObject", bitmap)
	old := gdi("SelectObject", dc, bitmap)
	defer gdi("SelectObject", dc, old)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dark := range []bool{false, true} {
		u.theme = uiTheme{enabled: true, bg: rgb(0xf3f5f8), field: rgb(0xffffff), text: rgb(0x20232a), muted: rgb(0x616a78), line: rgb(0xd3d9e3), accent: rgb(0x0067c0)}
		name := "timeline-light.png"
		if dark {
			u.theme.bg, u.theme.field, u.theme.text, u.theme.muted, u.theme.line, u.theme.accent = rgb(0x202020), rgb(0x2d2d2d), rgb(0xf1f4f9), rgb(0xa7b0bf), rgb(0x414141), rgb(0x75baff)
			name = "timeline-dark.png"
		}
		timeline.draw(dc, rect{Right: width, Bottom: height})
		gdi("GdiFlush")
		source := unsafe.Slice((*byte)(bits), width*height*4)
		out := image.NewRGBA(image.Rect(0, 0, width, height))
		for i := 0; i < len(source); i += 4 {
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = source[i+2], source[i+1], source[i], 255
		}
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(file, out)
		closeErr := file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}
