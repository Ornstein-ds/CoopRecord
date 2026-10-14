package main

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"unsafe"
)

func TestDynamicMixerLayoutAndScroll(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release := activateVisualStyles()
	defer release()
	common.NewProc("InitCommonControls").Call()
	previous := appUI
	defer func() { appUI = previous }()
	for _, scale := range []float64{0.75, 1, 1.5, 2} {
		parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 3400, 2200, 0, 0, 0, 0)
		if parent == 0 {
			t.Fatal("hidden window")
		}
		u := &windowUI{hwnd: parent, scale: scale, controls: map[int]uintptr{}, e: newTestEngine(syntheticCapture)}
		u.theme = uiTheme{enabled: true, children: map[uintptr]string{}, fields: map[uintptr]rect{}}
		u.font = gdi("CreateFontW", uintptr(int32(-13*scale)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
		appUI = u
		u.createControls(settings{})
		for hwnd := range u.theme.children {
			var bounds rect
			call("GetWindowRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
			call("MapWindowPoints", 0, parent, uintptr(unsafe.Pointer(&bounds)), 2)
			if bounds.Left < 0 || bounds.Top < 0 || bounds.Right > int32(u.px(windowWidth)) || bounds.Bottom > int32(u.px(windowHeight)) {
				t.Fatal("control outside compact window", getText(hwnd), bounds)
			}
			if getText(hwnd) == "1. Ваш звук" {
				for _, id := range []int{idName, idCorrection} {
					field := u.theme.fields[u.controls[id]]
					if bounds.Left < field.Right && bounds.Right > field.Left && bounds.Top < field.Bottom && bounds.Bottom > field.Top {
						t.Fatal("section heading clips the top of an input", scale, id, bounds, field)
					}
				}
			}
		}
		shown := func(id int) bool { return call("GetWindowLongPtrW", u.controls[id], ^uintptr(15))&0x10000000 != 0 }
		ctx, cancel := context.WithCancel(context.Background())
		v := newVoiceSession(ctx, nil, nil, 0, [32]byte{})
		u.e.voice = v
		var members []rosterMember
		for count := 1; count <= maxPeers+1; count++ {
			members = append(members, rosterMember{Source: uint32(count - 1), Name: fmt.Sprintf("Участник %d", count)})
			if err := v.setRoster(members); err != nil {
				t.Fatal(err)
			}
			u.updateMixer()
			if u.mixerCount != count+1 {
				t.Fatalf("%d people: got %d channels", count, u.mixerCount)
			}
			if shown(idMixerScroll) != (count+1 > u.mixerCapacity()) {
				t.Fatalf("%d people: incorrect scrollbar", count)
			}
			visible := 0
			for i := range u.mixerChannels {
				if shown(idMixerFirst + i*4 + 1) {
					visible++
				}
			}
			if visible != min(count+1, u.mixerCapacity()) {
				t.Fatalf("placeholder/hidden channel: %d", visible)
			}
		}
		windowProc(parent, 0x114, 7, u.controls[idMixerScroll]) // far right: reveal the final participant
		if u.mixerOffset != 9-u.mixerCapacity() || shown(idDesktopMute) || !shown(idMixerFirst+8*4+1) {
			t.Fatal("right edge", u.mixerOffset)
		}
		last := u.mixerChannels[8]
		sendMessage.Call(u.controls[idMixerFirst+8*4+1], 0x405, 1, 0)
		u.mixerScroll(u.controls[idMixerFirst+8*4+1])
		if last.volume.Load() != 200 || u.e.padMixer.volume.Load() != 100 {
			t.Fatal("scrolled fader controls wrong channel")
		}
		for _, preference := range []string{"light", "dark"} {
			u.theme.preference = preference
			u.applyTheme()
			for i, position := range u.mixerLayout {
				if position == 0 {
					continue
				}
				for part := 0; part < 4; part++ {
					var r rect
					call("GetWindowRect", u.controls[idMixerFirst+i*4+part], uintptr(unsafe.Pointer(&r)))
					call("MapWindowPoints", 0, parent, uintptr(unsafe.Pointer(&r)), 2)
					if r.Left < int32(u.px(mixerLeft)) || r.Right > int32(u.px(mixerLeft+mixerWidth)) || r.Bottom > int32(u.px(mixerTop+278)) {
						t.Fatal("fader overlaps transcript or scrollbar", r)
					}
				}
			}
		}
		// Removing channels while scrolled clamps the range and retains surviving gains.
		capture := u.controls[idMixerFirst+8*4+1]
		call("SetCapture", capture)
		before := u.mixerLayout
		members = append(members[:1], members[4:]...)
		if err := v.setRoster(members); err != nil {
			t.Fatal(err)
		}
		u.updateMixer()
		if before != u.mixerLayout || u.mixerChannels[8] != last {
			t.Fatal("participant moved during drag")
		}
		call("ReleaseCapture")
		u.updateMixer()
		if u.mixerOffset != 0 || shown(idMixerScroll) || !shown(idDesktopMute) || last.volume.Load() != 200 {
			t.Fatal("leave reset gain or left stale scroll")
		}
		// The scrollbar threshold follows the actual available width, not a fixed participant count.
		call("SetWindowPos", u.controls[idMixerScroll], 0, 0, 0, u.px(5*mixerStripStep-14), u.px(14), 0x16)
		u.updateMixer()
		if u.mixerCapacity() != 5 || !shown(idMixerScroll) {
			t.Fatal("narrower mixer failed to scroll")
		}
		call("SetWindowPos", u.controls[idMixerScroll], 0, 0, 0, u.px(mixerWidth), u.px(14), 0x16)
		u.updateMixer()
		if u.mixerCapacity() != 6 || shown(idMixerScroll) {
			t.Fatal("restored width left stale scrolling")
		}
		// A roster change must not move the control currently being dragged.
		drag := u.controls[idMixerFirst+8*4+1]
		call("SetCapture", drag)
		layout := u.mixerLayout
		if err := v.setRoster(members[0:1]); err != nil {
			t.Fatal(err)
		}
		// Removing the dragged participant releases capture instead of reusing its slider mid-drag.
		u.updateMixer()
		if call("GetCapture") == drag || u.mixerCount != 2 || layout == u.mixerLayout {
			t.Fatal("removed drag remained active")
		}
		chat := u.theme.fields[u.controls[idTranscript]]
		if chat.Right-chat.Left != int32(u.px(transcriptLeft+transcriptWidth))-int32(u.px(transcriptLeft)) {
			t.Fatal("chat width", chat)
		}
		gap := int32(u.px(transcriptLeft)) - int32(u.px(soundpadLeft+soundpadWidth))
		wantGap := int32(u.px(soundpadLeft)) - int32(u.px(516))
		if abs64(int64(gap-wantGap)) > 1 {
			t.Fatal("unequal column gaps")
		}
		status := u.theme.fields[u.controls[idSpeechStatus]]
		if status.Bottom-status.Top > int32(u.px(53)) || chat.Top != int32(u.px(314)) {
			t.Fatal("STT status not compact", status, chat)
		}
		cancel()
		call("DestroyWindow", parent)
		u.theme.close()
		gdi("DeleteObject", u.font)
	}
}
