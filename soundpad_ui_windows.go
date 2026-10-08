//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const (
	idPadFirst      = 200
	idPadLoadFirst  = 220
	idPadClearFirst = 240
	idPadKeyFirst   = 260
	idPadApply      = 280
	idPadProgress   = 281
	idPadStatus     = 282
)

func (u *windowUI) chooseSound() string {
	type openFileName struct {
		Size                         uint32
		Owner, Instance              uintptr
		Filter, CustomFilter         *uint16
		MaxCustomFilter, FilterIndex uint32
		File                         *uint16
		MaxFile                      uint32
		FileTitle                    *uint16
		MaxFileTitle                 uint32
		InitialDir, Title            *uint16
		Flags                        uint32
		FileOffset, FileExtension    uint16
		DefaultExt                   *uint16
		CustomData, Hook             uintptr
		TemplateName                 *uint16
		Reserved                     uintptr
		Reserved2, FlagsEx           uint32
	}
	filter := utf16.Encode([]rune("Звуки (*.wav;*.mp3)\x00*.wav;*.mp3\x00WAV (*.wav)\x00*.wav\x00MP3 (*.mp3)\x00*.mp3\x00\x00"))
	var file [32768]uint16
	info := openFileName{Owner: u.hwnd, Filter: &filter[0], File: &file[0], MaxFile: uint32(len(file)), Title: wide("Звук для саундпада (WAV/MP3, до 2 минут)"), Flags: 0x1000 | 0x800 | 0x80000 | 0x8}
	info.Size = uint32(unsafe.Sizeof(info))
	ok, _, _ := syscall.NewLazyDLL("comdlg32.dll").NewProc("GetOpenFileNameW").Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(file[:])
}

func (u *windowUI) createSoundpad(c settings) {
	u.padFiles = c.PadFiles
	u.label("Саундпад · звуки для всей сессии · необязательно", 746, 141, 660)
	u.label("WAV/MP3 выбирает хост. Нажмите звучащий пад ещё раз, чтобы остановить.", 746, 166, 675)
	for i := 0; i < padCount; i++ {
		x, y := 746+(i%3)*220, 194+(i/3)*125
		u.control(idPadFirst+i, "BUTTON", fmt.Sprintf("%d\r\nПусто", i+1), 0x10000|0x2000, x, y, 96, 96)
		u.button(idPadLoadFirst+i, "Файл…", x+101, y, 64)
		u.button(idPadClearFirst+i, "Убрать", x+101, y+35, 64)
		u.label("Клавиша:", x+101, y+76, 66)
		u.control(idPadKeyFirst+i, "msctls_hotkey32", "", 0x10000, x, y+99, 165, 23)
		key := c.PadKeys[i]
		if !c.PadKeysSet {
			key = uint16('1'+i) | 6<<8
		}
		sendMessage.Call(u.controls[idPadKeyFirst+i], 0x401, uintptr(key), 0) // HKM_SETHOTKEY
	}
	u.button(idPadApply, "Применить горячие клавиши", 746, 619, 270)
	u.control(idPadProgress, "msctls_progress32", "Загрузка саундпада", 0, 1030, 624, 382, 20)
	u.control(idPadStatus, "EDIT", "", 0x200844, 746, 654, 666, 32)
}

func (u *windowUI) readPadKeys() [padCount]uint16 {
	var keys [padCount]uint16
	for i := range keys {
		key, _, _ := sendMessage.Call(u.controls[idPadKeyFirst+i], 0x402, 0, 0)
		keys[i] = uint16(key)
	}
	return keys
}

func (u *windowUI) applyPadKeys() error {
	keys := u.readPadKeys()
	seen := map[uint16]bool{}
	for i, key := range keys {
		if key == 0 {
			continue
		}
		vk := key & 255
		mods := key >> 8
		if seen[key] || mods&6 == 0 {
			return fmt.Errorf("пад %d: используйте уникальное сочетание с Ctrl или Alt", i+1)
		}
		if vk == 0 {
			return fmt.Errorf("пад %d: выберите клавишу", i+1)
		}
		seen[key] = true
	}
	for i := 0; i < padCount; i++ {
		call("UnregisterHotKey", u.hwnd, uintptr(idPadFirst+i))
	}
	for i, key := range keys {
		if key == 0 {
			continue
		}
		mods := uintptr(0x4000) // MOD_NOREPEAT
		if key&0x100 != 0 {
			mods |= 4
		}
		if key&0x200 != 0 {
			mods |= 2
		}
		if key&0x400 != 0 {
			mods |= 1
		}
		if call("RegisterHotKey", u.hwnd, uintptr(idPadFirst+i), mods, uintptr(key&255)) == 0 {
			for j := 0; j < padCount; j++ {
				call("UnregisterHotKey", u.hwnd, uintptr(idPadFirst+j))
			}
			return fmt.Errorf("пад %d: сочетание занято другой программой. Выберите другое и примените заново", i+1)
		}
	}
	return nil
}

func (u *windowUI) savePadSettings() error {
	c := loadSettings()
	c.PadFiles = u.padFiles
	c.PadKeys = u.readPadKeys()
	c.PadKeysSet = true
	if err := saveSettings(c); err != nil {
		return err
	}
	u.savedPadFiles = u.padFiles
	return nil
}

func (u *windowUI) soundpadCommand(id int) bool {
	if id >= idPadFirst && id < idPadFirst+padCount {
		index := id - idPadFirst
		go func() {
			if err := u.e.triggerPad(index); err != nil {
				u.e.mu.Lock()
				u.e.lastError = err.Error()
				u.e.mu.Unlock()
			}
		}()
		return true
	}
	if id == idPadApply {
		err := u.applyPadKeys()
		if err == nil {
			err = u.savePadSettings()
		}
		if err != nil {
			messageBox(u.hwnd, err.Error(), 0x30)
		} else {
			u.savedPadKeys = u.readPadKeys()
			messageBox(u.hwnd, "Горячие клавиши применены и сохранены на этом компьютере.", 0x40)
		}
		return true
	}
	index := -1
	clear := false
	if id >= idPadLoadFirst && id < idPadLoadFirst+padCount {
		index = id - idPadLoadFirst
	}
	if id >= idPadClearFirst && id < idPadClearFirst+padCount {
		index = id - idPadClearFirst
		clear = true
	}
	if index < 0 {
		return false
	}
	if u.e.snapshot().Mode != "" || selected(u.controls[idRole]) != 0 {
		return true
	}
	if clear {
		u.padFiles[index] = ""
	} else {
		path := u.chooseSound()
		if path == "" {
			return true
		}
		u.padFiles[index] = path
	}
	if err := u.savePadSettings(); err != nil {
		messageBox(u.hwnd, err.Error(), 0x10)
	}
	u.update()
	return true
}

func (u *windowUI) updateSoundpad(v viewState) {
	for i := 0; i < padCount; i++ {
		name := v.Pads[i].Name
		if v.Mode == "" && selected(u.controls[idRole]) == 0 && u.padFiles[i] != "" {
			name = strings.TrimSuffix(filepath.Base(u.padFiles[i]), filepath.Ext(u.padFiles[i]))
		}
		if name == "" {
			name = "Нет звука"
		}
		text := fmt.Sprintf("%d\r\n%s", i+1, name)
		if text != u.padLabels[i] {
			setText(u.controls[idPadFirst+i], text)
			u.padLabels[i] = text
		}
		enable(u.controls[idPadFirst+i], v.Mode != "" && v.PadsReady && v.Pads[i].Size > 0 && !u.busy && !v.Exporting)
		edit := v.Mode == "" && selected(u.controls[idRole]) == 0 && !u.busy
		enable(u.controls[idPadLoadFirst+i], edit)
		enable(u.controls[idPadClearFirst+i], edit && u.padFiles[i] != "")
		enable(u.controls[idPadKeyFirst+i], !u.busy)
	}
	enable(u.controls[idPadApply], !u.busy && u.readPadKeys() != u.savedPadKeys)
	sendMessage.Call(u.controls[idPadProgress], 0x402, uintptr(v.PadProgress), 0)
	status := v.PadStatus
	if v.Mode == "" {
		status = "Хост может добавить звуки кнопкой «Файл…». Кнопки станут доступны в сессии."
		if selected(u.controls[idRole]) == 1 {
			status = "Звуки автоматически загрузятся после подключения к хосту."
		}
	}
	if status != u.lastPadStatus {
		setText(u.controls[idPadStatus], status)
		u.lastPadStatus = status
	}
}
