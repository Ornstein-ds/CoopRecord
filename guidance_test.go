package main

import (
	"strings"
	"testing"
)

func TestSessionGuidance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		v             viewState
		host, busy    bool
		action, title string
	}{
		{"first host", viewState{}, true, false, "connect", "Создайте"},
		{"first guest", viewState{}, false, false, "connect", "Подключитесь"},
		{"connection pending", viewState{}, true, true, "", "Выполняем"},
		{"guest downloading", viewState{Mode: "guest"}, false, false, "", "Загружаем"},
		{"guest waiting", viewState{Mode: "guest", PadsReady: true}, false, false, "", "ожидаем хоста"},
		{"host syncing", viewState{Mode: "host", RecordBlocked: "Синхронизация"}, true, false, "", "Готовим"},
		{"host alone", viewState{Mode: "host", CanRecord: true}, true, false, "record", "Пригласите"},
		{"ready", viewState{Mode: "host", CanRecord: true, PeerCount: 2}, true, false, "record", "Все готовы"},
		{"recording host", viewState{Mode: "host", Recording: true}, true, false, "stop", "Идёт запись"},
		{"recording guest", viewState{Mode: "guest", Recording: true}, false, false, "", "Хост записывает"},
		{"export takes priority", viewState{Mode: "host", Recording: true, Exporting: true}, true, true, "", "Сохраняем"},
		{"saved", viewState{Mode: "host", CanRecord: true, Folder: "take", Status: "Сохранено: take"}, true, false, "open", "сохранена"},
		{"old folder is not new result", viewState{Mode: "host", CanRecord: true, Folder: "old"}, true, false, "record", "Пригласите"},
		{"export error is not success", viewState{Mode: "host", CanRecord: true, Folder: "take", Error: "disk full"}, true, false, "", "ошибке"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := sessionGuidance(tc.v, tc.host, tc.busy)
			if g.action != tc.action || !strings.Contains(g.title, tc.title) || g.detail == "" {
				t.Fatalf("unexpected guidance: %+v", g)
			}
		})
	}
}

func TestSnapshotExplainsRecordingGate(t *testing.T) {
	e := newTestEngine(syntheticCapture)
	e.mode = "host"
	if v := e.snapshot(); v.CanRecord || !strings.Contains(v.RecordBlocked, "Микрофон") {
		t.Fatalf("missing microphone reason: %+v", v)
	}
	e.inputReady = true
	e.padsReady = true
	e.padVoices = 1
	if v := e.snapshot(); v.CanRecord || !strings.Contains(v.RecordBlocked, "пад") {
		t.Fatalf("missing active pad reason: %+v", v)
	}
	e.padVoices = 0
	e.peers = map[*peer]bool{&peer{name: "Гость", padsReady: true}: true}
	if v := e.snapshot(); v.CanRecord || v.PeerCount != 1 || !strings.Contains(v.RecordBlocked, "часы") {
		t.Fatalf("missing peer sync reason: %+v", v)
	}
}
