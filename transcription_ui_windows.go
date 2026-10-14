package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"
)

const (
	idSpeechMode = 500 + iota
	idSpeechModel
	idSpeechChoose
	idSpeechStatus
	idTranscript
)

func (u *windowUI) createTranscript(c settings) {
	u.speechModel = c.SpeechModel
	if u.speechModel == "" {
		u.speechModel = defaultSpeechModel()
	}
	u.label("Расшифровка", 1436, 22, 200)
	u.label("Локально · русский язык", 1436, 49, 200)
	u.combo(idSpeechMode, []string{"GPU · Vulkan", "CPU", "Выключена"}, 1436, 85, 200)
	mode := 0
	if c.SpeechCPU {
		mode = 1
	}
	if c.SpeechModel == "" || c.SpeechOff {
		mode = 2
	}
	sendMessage.Call(u.controls[idSpeechMode], 0x14e, uintptr(mode), 0)
	u.control(idSpeechModel, "EDIT", filepath.Base(u.speechModel), 0x804, 1436, 122, 200, 52)
	u.button(idSpeechChoose, "Выбрать модель…", 1436, 181, 200)
	u.control(idSpeechStatus, "EDIT", "", 0x804, 1436, 220, 200, 95)
	u.control(idTranscript, "EDIT", "", 0x210844, 1436, 325, 200, 673)
	sendMessage.Call(u.controls[idTranscript], 0xC5, 8<<20, 0)
}
func (u *windowUI) transcriptCommand(id int) bool {
	if id != idSpeechChoose {
		return false
	}
	if u.e.snapshot().Mode != "" {
		return true
	}
	if path := u.chooseFile("Локальная модель Whisper (GGML)", "Whisper (*.bin)\x00*.bin\x00\x00"); path != "" {
		u.speechModel = path
		setText(u.controls[idSpeechModel], filepath.Base(path))
	}
	return true
}
func (u *windowUI) updateTranscript(v viewState) {
	if u.controls[idTranscript] == 0 {
		return
	}
	host := selected(u.controls[idRole]) == 0
	for _, id := range []int{idSpeechMode, idSpeechChoose} {
		enable(u.controls[id], host && v.Mode == "" && !u.busy)
	}
	u.e.mu.Lock()
	s := u.e.transcript
	u.e.mu.Unlock()
	if s == nil {
		text := "Выберите модель до создания сессии. Текст сохраняется в transcript.md рядом с WAV."
		if !host {
			text = "Модель нужна только хосту. Здесь появится текст разговора."
		}
		if v.Mode == "host" {
			text = "Расшифровка выключена для этой сессии."
		}
		setText(u.controls[idSpeechStatus], text)
		return
	}
	if s != u.transcriptView {
		u.transcriptView = s
		u.transcriptRevision = ^uint64(0)
	}
	revision, status, lines := s.snapshot(u.transcriptRevision)
	setText(u.controls[idSpeechStatus], status)
	if revision == u.transcriptRevision {
		return
	}
	u.transcriptRevision = revision
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At < lines[j].At })
	var text strings.Builder
	lastRecord := "\x00"
	for _, line := range lines {
		if line.Recording != lastRecord {
			label := "Разговор вне записи"
			if line.Recording != "" {
				label = "Запись: " + line.Recording
			}
			fmt.Fprintf(&text, "%s\r\n\r\n", label)
			lastRecord = line.Recording
		}
		fmt.Fprintf(&text, "[%s] %s\r\n%s\r\n\r\n", transcriptTimestamp(line.Time), line.Name, line.Text)
	}
	u.replaceTranscriptText(text.String())
}

// Updating the transcript must not pull the reader back down while reviewing a pause.
func (u *windowUI) replaceTranscriptText(text string) {
	h := u.controls[idTranscript]
	first, _, _ := sendMessage.Call(h, 0xce, 0, 0)
	count, _, _ := sendMessage.Call(h, 0xba, 0, 0)
	var area rect
	sendMessage.Call(h, 0xb2, 0, uintptr(unsafe.Pointer(&area)))
	dc := call("GetDC", h)
	old := gdi("SelectObject", dc, u.font)
	var metrics [16]int32 // TEXTMETRICW; tmHeight is its first field.
	gdi("GetTextMetricsW", dc, uintptr(unsafe.Pointer(&metrics[0])))
	gdi("SelectObject", dc, old)
	call("ReleaseDC", h, dc)
	visible := max(1, (area.Bottom-area.Top)/max(1, metrics[0]))
	bottom := call("GetWindowTextLengthW", h) == 0 || int(first)+int(visible) >= int(count)-1
	var start, end uint32
	sendMessage.Call(h, 0xb0, uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&end)))
	sendMessage.Call(h, 0xb, 0, 0)
	setText(h, text)
	sendMessage.Call(h, 0xb, 1, 0)
	if bottom {
		length := call("GetWindowTextLengthW", h)
		sendMessage.Call(h, 0xb1, length, length)
		count, _, _ := sendMessage.Call(h, 0xba, 0, 0)
		sendMessage.Call(h, 0xb6, 0, count)
	} else {
		sendMessage.Call(h, 0xb1, uintptr(start), uintptr(end))
		now, _, _ := sendMessage.Call(h, 0xce, 0, 0)
		sendMessage.Call(h, 0xb6, 0, first-now)
	}
	call("RedrawWindow", h, 0, 0, 0x405)
}
