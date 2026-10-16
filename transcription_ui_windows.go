package main

import (
	"path/filepath"
)

const (
	idSpeechMode = 500 + iota
	idSpeechModel
	idSpeechChoose
	idSpeechStatus
	idTranscript
	idTranscriptCopy
)

func (u *windowUI) createTranscript(c settings) {
	u.speechModel = c.SpeechModel
	if u.speechModel == "" {
		u.speechModel = defaultSpeechModel()
	}
	u.label("Расшифровка", transcriptLeft, 22, transcriptWidth)
	u.label("Колонки участников · общая шкала времени · русский язык", transcriptLeft, 49, transcriptWidth)
	u.combo(idSpeechMode, []string{"GPU · Vulkan", "CPU", "Выключена"}, transcriptLeft, 85, transcriptWidth)
	mode := 0
	if c.SpeechCPU {
		mode = 1
	}
	if c.SpeechModel == "" || c.SpeechOff {
		mode = 2
	}
	sendMessage.Call(u.controls[idSpeechMode], 0x14e, uintptr(mode), 0)
	u.control(idSpeechModel, "EDIT", filepath.Base(u.speechModel), 0x804, transcriptLeft, 122, transcriptWidth, 52)
	u.button(idSpeechChoose, "Выбрать модель…", transcriptLeft, 181, transcriptWidth)
	u.control(idSpeechStatus, "EDIT", "", 0x804, transcriptLeft, 220, transcriptWidth-170, 52)
	u.button(idTranscriptCopy, "Копировать текст", transcriptLeft+transcriptWidth-160, 220, 160)
	u.control(idTranscript, "STATIC", "", 0x100|0x30000|0x100000|0x200000, transcriptLeft, 282, transcriptWidth, windowHeight-330)
	u.timeline = newTranscriptTimeline(u, u.controls[idTranscript])
}
func (u *windowUI) transcriptCommand(id int) bool {
	if id == idTranscriptCopy {
		if err := u.timeline.copyText(); err != nil {
			messageBox(u.hwnd, err.Error(), 0x10)
		}
		return true
	}
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
		if u.transcriptView != nil {
			u.transcriptView = nil
			u.timeline.update(nil)
		}
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
	u.timeline.update(lines)
}
