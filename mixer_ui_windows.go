//go:build windows

package main

import (
	"fmt"
	"unsafe"
)

const idMixerFirst = 400
const idDesktopMute = 450
const idMixerScroll = 451
const mixerStripStep = 154

func (u *windowUI) createMixer() {
	u.control(0, "BUTTON", "5. Микшер — прослушивание и передача звука компьютера", 7, 16, mixerTop, soundpadLeft+soundpadWidth-16, 295)
	for i := range u.mixerChannels {
		x, id := mixerLeft, idMixerFirst+i*4
		u.control(id, "STATIC", "", 0x1|0x4000, x, mixerTop+29, 140, 28) // centered, no mnemonic processing
		u.control(id+1, "msctls_trackbar32", "Громкость", 0x10000|0x2|0x4, x+38, mixerTop+59, 44, 160)
		sendMessage.Call(u.controls[id+1], 0x406, 1, 100<<16) // range 0..100, top = loud
		for _, position := range []uintptr{10, 20, 30, mixerUnityPosition, 40, 50, 60, 70, 80, 90} {
			sendMessage.Call(u.controls[id+1], 0x404, 0, position) // TBM_SETTIC
		}
		sendMessage.Call(u.controls[id+1], 0x405, 1, mixerUnityPosition)
		u.control(id+2, "msctls_progress32", "Уровень сигнала", 0x4|0x1, x+88, mixerTop+59, 22, 160)
		u.control(id+3, "STATIC", "", 1, x, mixerTop+222, 140, 22)
		for j := 0; j < 4; j++ {
			showControl(u.controls[id+j], false)
		}
	}
	u.control(idDesktopMute, "BUTTON", "Мьют включён", 0x10000, mixerLeft+mixerStripStep+5, mixerTop+250, 130, 24)
	u.control(idMixerScroll, "SCROLLBAR", "Прокрутка микшера", 0x10000, mixerLeft, mixerTop+278, mixerWidth, 14)
	showControl(u.controls[idMixerScroll], false)
}

func (u *windowUI) updateMixer() {
	caption := "Мьют включён"
	if !u.e.desktopMuted.Load() {
		caption = "Звук передаётся"
	}
	setText(u.controls[idDesktopMute], caption)
	u.e.mu.Lock()
	connected := u.e.voice != nil && u.e.inputReady && u.e.ctx != nil && u.e.ctx.Err() == nil
	u.e.mu.Unlock()
	enable(u.controls[idDesktopMute], connected && !u.busy)
	strips := u.e.mixerStrips()
	// Keep surviving channels in their slots, even when somebody leaves during a drag.
	for i, c := range u.mixerChannels {
		found := false
		for _, s := range strips {
			if s.Channel == c {
				found = true
				break
			}
		}
		if !found {
			if c != nil && call("GetCapture") == u.controls[idMixerFirst+i*4+1] {
				call("ReleaseCapture")
			}
			u.mixerChannels[i] = nil
		}
	}
	for _, s := range strips {
		found := false
		for _, c := range u.mixerChannels {
			if c == s.Channel {
				found = true
				break
			}
		}
		if !found {
			for i, c := range u.mixerChannels {
				if c == nil {
					u.mixerChannels[i] = s.Channel
					break
				}
			}
		}
	}
	for i, c := range u.mixerChannels {
		id, name, percent, level := idMixerFirst+i*4, "Нет участника", "", int32(0)
		if c != nil {
			for _, s := range strips {
				if s.Channel == c {
					name = s.Name
					break
				}
			}
			volume := c.volume.Load()
			percent = fmt.Sprintf("%d%%", volume)
			level = c.peak.Swap(0)
			position, _, _ := sendMessage.Call(u.controls[id+1], 0x400, 0, 0)
			if position != uintptr(mixerPosition(volume)) {
				sendMessage.Call(u.controls[id+1], 0x405, 1, uintptr(mixerPosition(volume)))
			}
		}
		enable(u.controls[id+1], c != nil)
		if getText(u.controls[id]) != name {
			setText(u.controls[id], name)
		}
		if getText(u.controls[id+3]) != percent {
			setText(u.controls[id+3], percent)
		}
		setProgress(u.controls[id+2], int(level))
	}
	u.layoutMixer()
}

func showControl(hwnd uintptr, visible bool) {
	if hwnd == 0 || (call("GetWindowLongPtrW", hwnd, ^uintptr(15))&0x10000000 != 0) == visible {
		return
	}
	command := uintptr(0)
	if visible {
		command = 5
	}
	call("ShowWindow", hwnd, command)
}

func (u *windowUI) layoutMixer() {
	// Keep the dragged HWND attached to the same participant until the drag ends.
	if captured := call("GetCapture"); captured != 0 {
		for i := range u.mixerChannels {
			if captured == u.controls[idMixerFirst+i*4+1] {
				return
			}
		}
	}
	count := 0
	for _, c := range u.mixerChannels {
		if c != nil {
			count++
		}
	}
	page := u.mixerCapacity()
	u.mixerOffset = max(0, min(u.mixerOffset, count-page))
	var layout [maxPeers + 2]int
	n := 0
	for i, c := range u.mixerChannels {
		if c == nil {
			continue
		}
		if n >= u.mixerOffset && n < u.mixerOffset+page {
			layout[i] = n - u.mixerOffset + 1
		}
		n++
	}
	if layout == u.mixerLayout && count == u.mixerCount && page == u.mixerPage {
		return
	}
	u.mixerCount = count
	u.mixerPage = page
	info := struct {
		Size, Mask uint32
		Min, Max   int32
		Page       uint32
		Pos, Track int32
	}{Mask: 7, Max: int32(max(0, count-1)), Page: uint32(page), Pos: int32(u.mixerOffset)}
	info.Size = uint32(unsafe.Sizeof(info))
	call("SetScrollInfo", u.controls[idMixerScroll], 2, uintptr(unsafe.Pointer(&info)), 1)
	showControl(u.controls[idMixerScroll], count > page)
	for i, position := range layout {
		if position == u.mixerLayout[i] {
			continue
		}
		id := idMixerFirst + i*4
		for j, part := range []struct{ x, y int }{{0, 29}, {38, 59}, {88, 59}, {0, 222}} {
			h := u.controls[id+j]
			if position > 0 {
				u.moveMixerControl(h, mixerLeft+(position-1)*mixerStripStep+part.x, mixerTop+part.y)
			}
			showControl(h, position > 0)
		}
	}
	if position := layout[1]; position > 0 {
		u.moveMixerControl(u.controls[idDesktopMute], mixerLeft+5+(position-1)*mixerStripStep, mixerTop+250)
	}
	showControl(u.controls[idDesktopMute], layout[1] > 0)
	u.mixerLayout = layout
}

func (u *windowUI) mixerCapacity() int {
	var bounds rect
	call("GetClientRect", u.controls[idMixerScroll], uintptr(unsafe.Pointer(&bounds)))
	return max(1, min(maxPeers+2, (int(bounds.Right-bounds.Left)+int(u.px(mixerStripStep-140)))/max(1, int(u.px(mixerStripStep)))))
}

func (u *windowUI) moveMixerControl(hwnd uintptr, x, y int) {
	if u.theme.enabled {
		y += 32
	}
	call("SetWindowPos", hwnd, 0, u.px(x), u.px(y), 0, 0, 0x15) // keep size, z order and focus
}

func (u *windowUI) scrollMixer(command, position int) {
	switch command {
	case 0:
		u.mixerOffset--
	case 1:
		u.mixerOffset++
	case 2:
		u.mixerOffset -= u.mixerCapacity()
	case 3:
		u.mixerOffset += u.mixerCapacity()
	case 4, 5:
		u.mixerOffset = position
	case 6:
		u.mixerOffset = 0
	case 7:
		u.mixerOffset = max(0, u.mixerCount-u.mixerCapacity())
	default:
		return
	}
	u.layoutMixer()
}

func (u *windowUI) mixerScroll(hwnd uintptr) {
	for i, c := range u.mixerChannels {
		id := idMixerFirst + i*4
		if c != nil && hwnd != 0 && hwnd == u.controls[id+1] {
			position, _, _ := sendMessage.Call(hwnd, 0x400, 0, 0)
			volume := mixerGain(int32(position))
			c.volume.Store(volume)
			setText(u.controls[id+3], fmt.Sprintf("%d%%", volume))
			return
		}
	}
}
