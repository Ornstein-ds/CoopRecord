//go:build windows

package main

import (
	"runtime"
	"testing"
	"unicode/utf16"
	"unsafe"
)

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
