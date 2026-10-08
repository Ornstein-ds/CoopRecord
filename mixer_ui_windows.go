//go:build windows

package main

import "fmt"

const idMixerFirst = 400

func (u *windowUI) createMixer() {
	u.control(0, "BUTTON", "Микшер — громкость в ваших наушниках", 7, 20, 700, 1400, 319)
	u.label("На WAV-записи не влияет. Индикаторы показывают входной уровень дорожек, до фейдера. 100% — исходная громкость, 0% — без звука.", 34, 724, 1365)
	for i := range u.mixerChannels {
		x, id := 34+i*173, idMixerFirst+i*4
		u.control(id, "STATIC", "Нет участника", 0x1|0x4000, x, 753, 160, 38) // centered, no mnemonic processing
		u.control(id+1, "msctls_trackbar32", "Громкость", 0x10000|0x2|0x10, x+38, 795, 44, 180)
		sendMessage.Call(u.controls[id+1], 0x406, 1, 100<<16) // range 0..100, top = loud
		u.control(id+2, "msctls_progress32", "Уровень сигнала", 0x4|0x1, x+88, 795, 22, 180)
		u.control(id+3, "STATIC", "", 1, x, 985, 160, 22)
	}
}

func (u *windowUI) updateMixer() {
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
			sendMessage.Call(u.controls[id+1], 0x405, 1, uintptr(100-volume))
		}
		enable(u.controls[id+1], c != nil)
		if getText(u.controls[id]) != name {
			setText(u.controls[id], name)
		}
		if getText(u.controls[id+3]) != percent {
			setText(u.controls[id+3], percent)
		}
		sendMessage.Call(u.controls[id+2], 0x402, uintptr(level), 0)
	}
}

func (u *windowUI) mixerScroll(hwnd uintptr) {
	for i, c := range u.mixerChannels {
		id := idMixerFirst + i*4
		if c != nil && hwnd != 0 && hwnd == u.controls[id+1] {
			position, _, _ := sendMessage.Call(hwnd, 0x400, 0, 0)
			volume := int32(100 - min(position, 100))
			c.volume.Store(volume)
			setText(u.controls[id+3], fmt.Sprintf("%d%%", volume))
			return
		}
	}
}
