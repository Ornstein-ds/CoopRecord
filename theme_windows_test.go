//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestComboBoundsAndIdleRepaint(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release := activateVisualStyles()
	defer release()
	common.NewProc("InitCommonControls").Call()
	previousUI := appUI
	defer func() { appUI = previousUI }()
	for _, scale := range []float64{0.75, 0.958333, 1, 1.25, 1.5, 2} {
		parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 1440, 1200, 0, 0, 0, 0)
		if parent == 0 {
			t.Fatal("test window")
		}
		u := &windowUI{hwnd: parent, scale: scale, controls: map[int]uintptr{}, e: newTestEngine(syntheticCapture)}
		u.theme = uiTheme{enabled: true, children: map[uintptr]string{}}
		u.font = gdi("CreateFontW", uintptr(int32(-13*scale)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
		appUI = u
		u.combo(idMic, []string{"Микрофон (USB Audio CODEC)", "Другой микрофон"}, 145, 137, 423)
		u.combo(idRole, []string{"Хост", "Участник"}, 145, 254, 310)
		u.button(idConnect, "Создать сессию", 20, 561, 175)
		u.control(idLevel, "msctls_progress32", "", 0, 300, 183, 230, 18)
		u.createMixer()
		var thumb rect
		sendMessage.Call(u.controls[idMixerFirst+1], 0x419, 0, uintptr(unsafe.Pointer(&thumb)))
		if thumb.Left < 10 {
			t.Fatal("no room for scale to the left of the fader", scale, thumb)
		}
		foundUnity := false
		for index := uintptr(0); index < 10; index++ {
			pos, _, _ := sendMessage.Call(u.controls[idMixerFirst+1], 0x40f, index, 0)
			value, _, _ := sendMessage.Call(u.controls[idMixerFirst+1], 0x403, index, 0)
			if value == mixerUnityPosition {
				foundUnity = true
				if int32(pos) != (thumb.Top+thumb.Bottom)/2 {
					t.Fatal("unity mark does not align with the fader", scale, pos, thumb)
				}
			}
		}
		if !foundUnity {
			t.Fatal("missing unity mark")
		}
		for _, theme := range []string{"light", "dark"} {
			u.theme.preference = theme
			u.applyTheme()
			for _, id := range []int{idMic, idRole} {
				hwnd := u.controls[id]
				var bounds rect
				call("GetWindowRect", hwnd, uintptr(unsafe.Pointer(&bounds)))
				call("MapWindowPoints", 0, parent, uintptr(unsafe.Pointer(&bounds)), 2)
				field := u.theme.fields[hwnd]
				if bounds.Top <= field.Top || bounds.Bottom >= field.Bottom || bounds.Left <= field.Left || bounds.Right >= field.Right {
					t.Errorf("scale %.3f %s: combo %v outside field %v", scale, theme, bounds, field)
				}
				sendMessage.Call(hwnd, 0x14e, 1, 0)
				if selected(hwnd) != 1 {
					t.Fatal("cannot select combo item")
				}
			}
		}
		u.update() // establish the initial UI state
		writes := 0
		probe := syscall.NewCallback(func(hwnd uintptr, msg uint32, w, l, id, data uintptr) uintptr {
			if msg == 0xc || msg == 0xa || msg == 0x402 || msg == 0x405 {
				writes++
			}
			r, _, _ := common.NewProc("DefSubclassProc").Call(hwnd, uintptr(msg), w, l)
			return r
		})
		for _, id := range []int{idConnect, idLevel, idMixerFirst + 1, idMixerFirst + 2} {
			common.NewProc("SetWindowSubclass").Call(u.controls[id], probe, 2, 0)
		}
		for i := 0; i < 12; i++ {
			u.update()
		}
		if writes != 0 {
			t.Errorf("idle timer rewrote unchanged controls %d times", writes)
		}
		setProgress(u.controls[idLevel], 50)
		if writes != 1 {
			t.Fatal("changed level did not reach the control", writes)
		}
		call("DestroyWindow", parent)
		u.theme.close()
		gdi("DeleteObject", u.font)
	}
}

func TestThemePreferenceAndNativeControls(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	if err := saveSettings(settings{Name: "Keep me", DeviceID: "microphone", Role: 1, PadKeysSet: true}); err != nil {
		t.Fatal(err)
	}
	for _, preference := range []string{"dark", "light", ""} {
		if err := saveThemePreference(preference); err != nil {
			t.Fatal(err)
		}
		c := loadSettings()
		if c.Theme != preference || c.Name != "Keep me" || c.DeviceID != "microphone" || c.Role != 1 || !c.PadKeysSet {
			t.Fatal("theme changed audio/session settings", c)
		}
		for _, system := range []bool{false, true} {
			want := preference == "dark" || preference == "" && system
			if darkTheme(preference, system) != want {
				t.Fatal("theme selection")
			}
		}
	}
	file := filepath.Join(os.Getenv("APPDATA"), "CoopRecord", "settings.json")
	if err := os.WriteFile(file, []byte("bad json"), 0600); err != nil {
		t.Fatal(err)
	}
	if saveThemePreference("dark") == nil {
		t.Fatal("overwrote malformed settings")
	}
	b, _ := os.ReadFile(file)
	if json.Valid(b) {
		t.Fatal("settings changed on error")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	common.NewProc("InitCommonControls").Call()
	if hotkeyLabel(0) != "Нет" || !strings.Contains(hotkeyLabel(uintptr('A')|6<<8), "Ctrl + Alt + A") {
		t.Fatal("hotkey label changed key combination")
	}
	parent := call("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide("STATIC"))), 0, 0, 0, 0, 500, 500, 0, 0, 0, 0)
	if parent == 0 {
		t.Fatal("test window")
	}
	u := &windowUI{hwnd: parent, scale: 1, controls: map[int]uintptr{}}
	u.theme = uiTheme{enabled: true, children: make(map[uintptr]string)}
	defer u.theme.close()
	defer call("DestroyWindow", parent)
	u.button(idTheme, "Theme", 5, 5, 200)
	u.button(idSystemTheme, "System", 5, 40, 200)
	u.control(idPadFirst, "BUTTON", "1\r\nSound", 0x10000|0x2000, 5, 80, 96, 96)
	u.control(idLevel, "msctls_progress32", "", 0, 5, 190, 200, 20)
	u.edit(idName, "Keyboard", 5, 220, 200)
	u.control(idPadKeyFirst, "msctls_hotkey32", "", 0x10000, 5, 260, 200, 24)
	sendMessage.Call(u.controls[idPadKeyFirst], 0x401, uintptr('A')|6<<8, 0)
	for _, mode := range []string{"light", "dark", ""} {
		u.theme.preference = mode
		u.applyTheme()
		if u.theme.bgBrush == 0 || u.theme.fieldBrush == 0 {
			t.Fatal("theme brushes")
		}
		if call("GetWindowLongPtrW", u.controls[idPadFirst], ^uintptr(15))&15 != 11 {
			t.Fatal("pad lost rounded button style")
		}
		dc := call("GetDC", parent)
		u.themeMessage(parent, 0x14, dc, 0)
		for _, state := range []uint32{0, 1, 4, 0x10} {
			u.drawButton(&drawItem{Type: 4, ID: idTheme, State: state, Window: u.controls[idTheme], DC: dc, Rect: rect{0, 0, 200, 28}})
			u.drawButton(&drawItem{Type: 4, ID: idPadFirst, State: state, Window: u.controls[idPadFirst], DC: dc, Rect: rect{0, 0, 96, 96}})
		}
		call("ReleaseDC", parent, dc)
		if getText(u.controls[idName]) != "Keyboard" {
			t.Fatal("theme replaced input text")
		}
		key, _, _ := sendMessage.Call(u.controls[idPadKeyFirst], 0x402, 0, 0)
		if key != uintptr('A')|6<<8 {
			t.Fatal("theme changed hotkey")
		}
		if call("GetWindowLongPtrW", u.controls[idName], ^uintptr(19))&0x200 != 0 {
			t.Fatal("old sunken edit border remains")
		}
		if _, handled := u.themeMessage(parent, 0x83, 1, 0); !handled {
			t.Fatal("custom title area is not enabled")
		}
	}
}
