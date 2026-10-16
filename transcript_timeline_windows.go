package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var timelineWindows sync.Map
var timelineSubclass uintptr

func init() { timelineSubclass = syscall.NewCallback(transcriptTimelineProc) }

type transcriptTimeline struct {
	u                         *windowUI
	hwnd                      uintptr
	lines                     []transcriptLine
	heights                   map[string]int
	layout                    transcriptLayout
	x, y                      int
	width, height             int
	laneWidth, gutter, header int
}

func newTranscriptTimeline(u *windowUI, hwnd uintptr) *transcriptTimeline {
	t := &transcriptTimeline{u: u, hwnd: hwnd, laneWidth: int(u.px(258)), gutter: int(u.px(100)), header: int(u.px(42))}
	timelineWindows.Store(hwnd, t)
	common.NewProc("SetWindowSubclass").Call(hwnd, timelineSubclass, 2, 0)
	t.update(nil)
	return t
}

func (t *transcriptTimeline) viewport() (int, int) {
	var r rect
	call("GetClientRect", t.hwnd, uintptr(unsafe.Pointer(&r)))
	return int(r.Right), max(1, int(r.Bottom)-t.header)
}

func (t *transcriptTimeline) update(lines []transcriptLine) {
	_, oldPage := t.viewport()
	follow := t.y >= max(0, t.layout.Height-oldPage)-int(t.u.px(24))
	// Anchor a reader to an utterance, even if an earlier preview changes height.
	anchor, offset := uint64(0), 0
	for _, c := range t.layout.Cards {
		if c.Top <= t.y && c.Bottom >= t.y && c.Line.ID != 0 {
			anchor, offset = c.Line.ID, t.y-c.Top
			break
		}
	}
	t.lines = append([]transcriptLine(nil), lines...)
	enable(t.u.controls[idTranscriptCopy], len(lines) > 0)
	dc := call("GetDC", t.hwnd)
	old := gdi("SelectObject", dc, t.u.font)
	heights := make(map[string]int, len(lines))
	t.layout = layoutTranscript(lines, 18*t.u.scale, max(1, int(t.u.px(8))), func(text string) int {
		if height, exists := t.heights[text]; exists {
			heights[text] = height
			return height
		}
		r := rect{Right: int32(t.laneWidth) - int32(t.u.px(28))}
		call("DrawTextW", dc, uintptr(unsafe.Pointer(wide(text))), ^uintptr(0), uintptr(unsafe.Pointer(&r)), 0x10|0x400|0x800|0x2000)
		height := int(r.Bottom) + int(t.u.px(90))
		heights[text] = height
		return height
	})
	t.heights = heights
	gdi("SelectObject", dc, old)
	call("ReleaseDC", t.hwnd, dc)
	if follow {
		t.y = 1 << 30
	} else if anchor != 0 {
		for _, c := range t.layout.Cards {
			if c.Line.ID == anchor {
				t.y = c.Top + offset
				break
			}
		}
	}
	t.scrollbars()
	call("InvalidateRect", t.hwnd, 0, 0)
}

type timelineScrollInfo struct {
	Size, Mask uint32
	Min, Max   int32
	Page       uint32
	Pos, Track int32
}

func (t *transcriptTimeline) scrollbars() {
	// Iterate because showing one scrollbar changes the other's viewport.
	for pass := 0; pass < 2; pass++ {
		width, height := t.viewport()
		t.width, t.height = width, height
		contentWidth := t.gutter + len(t.layout.Lanes)*t.laneWidth
		t.x = max(0, min(t.x, max(0, contentWidth-width)))
		t.y = max(0, min(t.y, max(0, t.layout.Height-height)))
		for bar, dimension := range []struct{ total, page, pos int }{{contentWidth, width, t.x}, {t.layout.Height, height, t.y}} {
			info := timelineScrollInfo{Mask: 7, Max: int32(max(0, dimension.total-1)), Page: uint32(dimension.page), Pos: int32(dimension.pos)}
			info.Size = uint32(unsafe.Sizeof(info))
			call("SetScrollInfo", t.hwnd, uintptr(bar), uintptr(unsafe.Pointer(&info)), 1)
		}
	}
}

func (t *transcriptTimeline) scroll(horizontal bool, command int) {
	position, page, total := t.y, t.height, t.layout.Height
	bar := uintptr(1)
	if horizontal {
		position, page, total, bar = t.x, max(1, t.width-t.gutter), len(t.layout.Lanes)*t.laneWidth, 0
	}
	step := max(1, int(t.u.px(36)))
	switch command {
	case 0:
		position -= step
	case 1:
		position += step
	case 2:
		position -= page
	case 3:
		position += page
	case 4, 5:
		info := timelineScrollInfo{Mask: 0x10}
		info.Size = uint32(unsafe.Sizeof(info))
		call("GetScrollInfo", t.hwnd, bar, uintptr(unsafe.Pointer(&info)))
		position = int(info.Track)
	case 6:
		position = 0
	case 7:
		position = total
	default:
		return
	}
	if horizontal {
		t.x = position
	} else {
		t.y = position
	}
	t.scrollbars()
	call("InvalidateRect", t.hwnd, 0, 0)
}

func (t *transcriptTimeline) paint() {
	var ps paintInfo
	dc := call("BeginPaint", t.hwnd, uintptr(unsafe.Pointer(&ps)))
	var bounds rect
	call("GetClientRect", t.hwnd, uintptr(unsafe.Pointer(&bounds)))
	dc, present := bufferControl(dc, bounds)
	t.draw(dc, bounds)
	present()
	call("EndPaint", t.hwnd, uintptr(unsafe.Pointer(&ps)))
}

func transcriptTimelineProc(hwnd uintptr, msg uint32, w, l, id, data uintptr) uintptr {
	if value, exists := timelineWindows.Load(hwnd); exists {
		t := value.(*transcriptTimeline)
		switch msg {
		case 0x87: // WM_GETDLGCODE: keep arrow navigation within the timeline.
			return 1 | 0x80
		case 0xf:
			t.paint()
			return 0
		case 0x14:
			return 1
		case 0x114, 0x115:
			t.scroll(msg == 0x114, int(w&0xffff))
			return 0
		case 0x20a, 0x20e:
			delta := int(int16(w >> 16))
			step := delta * int(t.u.px(100)) / 120
			if msg == 0x20e {
				t.x += step
			} else if w&4 != 0 {
				t.x -= step
			} else {
				t.y -= step
			}
			t.scrollbars()
			call("InvalidateRect", hwnd, 0, 0)
			return 0
		case 0x201:
			call("SetFocus", hwnd)
			return 0
		case 0x100:
			if w == 'C' && call("GetKeyState", 0x11)&0x8000 != 0 {
				_ = t.copyText()
				return 0
			}
			switch w {
			case 0x26:
				t.scroll(false, 0)
			case 0x28:
				t.scroll(false, 1)
			case 0x21:
				t.scroll(false, 2)
			case 0x22:
				t.scroll(false, 3)
			case 0x24:
				t.scroll(false, 6)
			case 0x23:
				t.scroll(false, 7)
			case 0x25:
				t.scroll(true, 0)
			case 0x27:
				t.scroll(true, 1)
			}
			return 0
		case 5:
			t.scrollbars()
			return 0
		case 0x82:
			timelineWindows.Delete(hwnd)
			common.NewProc("RemoveWindowSubclass").Call(hwnd, timelineSubclass, id)
		}
	}
	r, _, _ := common.NewProc("DefSubclassProc").Call(hwnd, uintptr(msg), w, l)
	return r
}

func (t *transcriptTimeline) copyText() error {
	lines := append([]transcriptLine(nil), t.lines...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At < lines[j].At })
	var text strings.Builder
	for _, line := range lines {
		if line.Partial {
			line.Text += " (уточняется)"
		}
		text.WriteString(strings.ReplaceAll(transcriptMarkdown(line), "\n", "\r\n"))
	}
	units, err := syscall.UTF16FromString(text.String())
	if err != nil {
		return err
	}
	if call("OpenClipboard", t.hwnd) == 0 {
		return fmt.Errorf("Не удалось открыть буфер обмена. Повторите копирование.")
	}
	defer call("CloseClipboard")
	size := uintptr(len(units) * 2)
	memory, _, _ := kernel.NewProc("GlobalAlloc").Call(2, size)
	if memory == 0 {
		return fmt.Errorf("Недостаточно памяти для копирования текста.")
	}
	owned := true
	defer func() {
		if owned {
			kernel.NewProc("GlobalFree").Call(memory)
		}
	}()
	data, _, _ := kernel.NewProc("GlobalLock").Call(memory)
	if data == 0 {
		return fmt.Errorf("Не удалось подготовить текст к копированию.")
	}
	kernel.NewProc("RtlMoveMemory").Call(data, uintptr(unsafe.Pointer(&units[0])), size)
	kernel.NewProc("GlobalUnlock").Call(memory)
	if call("EmptyClipboard") == 0 || call("SetClipboardData", 13, memory) == 0 {
		return fmt.Errorf("Не удалось скопировать текст.")
	}
	owned = false // Windows now owns the global allocation.
	return nil
}

func (t *transcriptTimeline) draw(dc uintptr, bounds rect) {
	u, theme := t.u, t.u.theme
	if !theme.enabled {
		theme.bg, theme.field, theme.text, theme.muted, theme.line, theme.accent = 0xf6f6f6, 0xffffff, 0x202020, 0x666666, 0xdddddd, 0xa06020
	}
	fillRect(dc, bounds, theme.bg)
	if len(t.layout.Lanes) == 0 {
		r := bounds
		r.Top += int32(u.px(65))
		r.Left += int32(u.px(20))
		r.Right -= int32(u.px(20))
		drawText(dc, u.font, "Здесь появятся колонки участников.\nГоворите — предварительный текст будет уточняться до конца фразы.", r, theme.muted, 0x10|1)
	}
	saved := gdi("SaveDC", dc)
	gdi("IntersectClipRect", dc, uintptr(t.gutter), uintptr(t.header), uintptr(bounds.Right), uintptr(bounds.Bottom))
	for _, c := range t.layout.Cards {
		left := t.gutter + c.Lane*t.laneWidth - t.x
		top, bottom := t.header+c.Top-t.y, t.header+c.Bottom-t.y
		if bottom < t.header || top > int(bounds.Bottom) || left+t.laneWidth < t.gutter || left > int(bounds.Right) {
			continue
		}
		r := rect{int32(left + int(u.px(4))), int32(top), int32(left + t.laneWidth - int(u.px(4))), int32(bottom)}
		border := theme.line
		if c.Line.Partial {
			border = theme.accent
		}
		rounded(dc, r, int32(u.px(9)), theme.field, border)
		r.Left += int32(u.px(10))
		r.Right -= int32(u.px(10))
		r.Top += int32(u.px(7))
		end := c.Line.Time + max(int64(0), c.Line.EndAt-c.Line.At)
		label := transcriptTimestamp(c.Line.Time)
		meta := r
		meta.Bottom = meta.Top + int32(u.px(20))
		drawText(dc, u.font, label, meta, theme.muted, 0x20|0x8000)
		meta.Top = meta.Bottom
		meta.Bottom += int32(u.px(20))
		caption := "Вне записи"
		if c.Line.Recording != "" {
			caption = "Запись: " + c.Line.Recording
		}
		if c.Line.Partial {
			caption = "Уточняется… · " + caption
		}
		drawText(dc, u.font, caption, meta, theme.muted, 0x20|0x8000)
		footer := r
		footer.Bottom -= int32(u.px(8))
		footer.Top = footer.Bottom - int32(u.px(20))
		drawText(dc, u.font, "Конец: "+transcriptTimestamp(end), footer, theme.muted, 0x20|0x8000)
		r.Top = meta.Bottom + int32(u.px(4))
		r.Bottom = footer.Top - int32(u.px(6))
		drawText(dc, u.font, c.Line.Text, r, theme.text, 0x10|0x2000)
	}
	gdi("RestoreDC", dc, saved)
	// The time gutter and speaker headers stay fixed while the content scrolls.
	fillRect(dc, rect{0, 0, int32(t.gutter), bounds.Bottom}, theme.bg)
	lastTick := -100000
	for _, tick := range t.layout.Ticks {
		y := t.header + tick.Y - t.y
		if y < t.header || y+int(u.px(20)) > int(bounds.Bottom) || y-lastTick < int(u.px(26)) {
			continue
		}
		r := rect{int32(u.px(3)), int32(y), int32(t.gutter) - int32(u.px(4)), int32(y) + int32(u.px(20))}
		drawText(dc, u.font, transcriptTimestamp(tick.Time), r, theme.muted, 0x20|2)
		lastTick = y
	}
	fillRect(dc, rect{0, 0, bounds.Right, int32(t.header)}, theme.field)
	drawText(dc, u.font, "Время", rect{0, 0, int32(t.gutter), int32(t.header)}, theme.muted, 1|4|0x20)
	saved = gdi("SaveDC", dc)
	gdi("IntersectClipRect", dc, uintptr(t.gutter), 0, uintptr(bounds.Right), uintptr(t.header))
	for i, lane := range t.layout.Lanes {
		left := t.gutter + i*t.laneWidth - t.x
		r := rect{int32(left) + int32(u.px(10)), 0, int32(left+t.laneWidth) - int32(u.px(10)), int32(t.header)}
		drawText(dc, u.font, fmt.Sprintf("%s · %d", lane.Name, lane.Source+1), r, theme.text, 4|0x20|0x8000)
	}
	gdi("RestoreDC", dc, saved)
}
