//go:build windows

package main

import "fmt"

const idMixerFirst = 400
const idDesktopMute = 450

func (u *windowUI) createMixer() {
	u.control(0, "BUTTON", "5. Микшер — прослушивание и передача звука компьютера", 7, 20, 700, 1400, 319)
	u.label("Жирная отметка — 100%, максимум — 200%. Только «Звук компьютера» меняет передачу и запись; остальные фейдеры — ваши наушники.", 34, 724, 1365)
	for i := range u.mixerChannels {
		x, id := 34+i*153, idMixerFirst+i*4
		u.control(id, "STATIC", "Нет участника", 0x1|0x4000, x, 753, 140, 38) // centered, no mnemonic processing
		u.control(id+1, "msctls_trackbar32", "Громкость", 0x10000|0x2|0x4, x+38, 795, 44, 160)
		sendMessage.Call(u.controls[id+1], 0x406, 1, 100<<16) // range 0..100, top = loud
		for _, position := range []uintptr{10, 20, 30, mixerUnityPosition, 40, 50, 60, 70, 80, 90} {
			sendMessage.Call(u.controls[id+1], 0x404, 0, position) // TBM_SETTIC
		}
		sendMessage.Call(u.controls[id+1], 0x405, 1, mixerUnityPosition)
		u.control(id+2, "msctls_progress32", "Уровень сигнала", 0x4|0x1, x+88, 795, 22, 160)
		u.control(id+3, "STATIC", "", 1, x, 958, 140, 22)
	}
	u.control(idDesktopMute, "BUTTON", "Мьют включён", 0x10000, 34+153+5, 984, 130, 24)
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
