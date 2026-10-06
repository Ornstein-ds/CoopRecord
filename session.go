package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type settings struct {
	PadFiles                             [padCount]string
	PadKeys                              [padCount]uint16
	PadKeysSet                           bool
	Name, DeviceID, Address, Folder, Key string
	CorrectionMS                         int
	Role                                 int
}
type peer struct {
	padBytes   int64
	padsReady  bool
	w          *wire
	name       string
	correction int
	ready      bool
	samples    []clockSample
	pending    int64
	number     int
}
type recording struct {
	padEvents     *os.File
	padEventCount int
	dir, id       string
	m             manifest
	local         *trackWriter
	tracks        map[*peer]*trackWriter
	ended         map[*peer]chan struct{}
	stopping      bool
	done          chan struct{}
}
type audioEvent struct {
	packet  packet
	session string
	done    chan struct{}
}
type captureFunc func(context.Context, string, chan<- error, func(packet) error, *atomic.Int32) error
type engine struct {
	padTriggerMu             sync.Mutex
	pads                     [padCount]padClip
	padDir                   string
	padBytes, padTotal       int64
	padsReady, padReceiving  bool
	padVoices                int
	padIndex                 int
	padUntil, padCommandTime int64
	padCommands              chan padPlayback
	playPad                  func(context.Context, []byte, int64) error
	mu                       sync.Mutex
	// Capture never takes mu: disk writes/flushes hold it and may stall for >100 ms.
	inputMu                             sync.Mutex
	audioID                             string
	mode, status, lastError, lastFolder string
	cfg                                 settings
	ctx                                 context.Context
	cancel                              context.CancelFunc
	listener                            net.Listener
	client                              *wire
	peers                               map[*peer]bool
	rec                                 *recording
	liveID                              string
	inputReady, exporting               bool
	level                               atomic.Int32
	audio                               chan audioEvent
	capture                             captureFunc
	nextPeer                            int
}
type viewState struct {
	Pads                                [padCount]padInfo
	PadProgress                         int
	PadStatus                           string
	PadsReady                           bool
	Mode, Status, Error, Folder, People string
	Recording, CanRecord, Exporting     bool
	Level                               int
	Elapsed                             time.Duration
}

func newEngine(capture captureFunc) *engine {
	return &engine{capture: capture, playPad: playPadPCM, status: "Выберите микрофон и роль сессии."}
}
func (e *engine) snapshot() viewState {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := viewState{Mode: e.mode, Status: e.status, Error: e.lastError, Folder: e.lastFolder, Level: int(e.level.Load()), Exporting: e.exporting}
	for i, p := range e.pads {
		v.Pads[i] = p.Info
	}
	v.PadsReady = e.padsReadyLocked()
	if e.mode == "guest" {
		v.PadsReady = e.padsReady
	}
	percent := func(n int64, ready bool) int {
		if ready {
			return 100
		}
		if e.padTotal == 0 {
			return 0
		}
		return min(99, int(n*100/e.padTotal))
	}
	v.PadProgress = percent(e.padBytes, e.padsReady)
	v.PadStatus = fmt.Sprintf("Ваш ПК: %d%%", v.PadProgress)
	if e.mode != "" {
		v.People = e.cfg.Name + " — вы\r\n"
	}
	v.CanRecord = e.mode == "host" && e.inputReady && !e.exporting && e.padsReadyLocked() && e.padVoices == 0
	if e.padVoices > 0 {
		v.PadStatus += "\r\nПеред началом записи дождитесь окончания звуков."
	}
	peers := e.sortedPeers()
	for _, p := range peers {
		progress := percent(p.padBytes, p.padsReady)
		v.PadProgress = min(v.PadProgress, progress)
		v.PadStatus += fmt.Sprintf("\r\n%s: %d%%", p.name, progress)
		state := "проверка микрофона и часов"
		if p.ready && len(p.samples) >= 8 {
			best := p.samples[max(0, len(p.samples)-8):]
			rtt := best[0].RTT
			for _, s := range best {
				rtt = min(rtt, s.RTT)
			}
			state = fmt.Sprintf("готов · RTT %.1f мс", float64(rtt)/1e6)
		} else {
			v.CanRecord = false
		}
		v.People += p.name + " — " + state + "\r\n"
	}
	if e.rec != nil {
		v.Recording = true
		v.Elapsed = max(0, time.Duration(clockNow()-e.rec.m.Start))
		v.CanRecord = false
	}
	if e.liveID != "" {
		v.Recording = true
	}
	return v
}
func (e *engine) sortedPeers() []*peer {
	ps := make([]*peer, 0, len(e.peers))
	for p := range e.peers {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].number < ps[j].number })
	return ps
}

func checkSettings(c settings) error {
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 40 || strings.ContainsAny(c.Name, "\x00\r\n") {
		return fmt.Errorf("введите имя: от 1 до 40 символов")
	}
	if c.DeviceID == "" {
		return fmt.Errorf("выберите микрофон")
	}
	if len(c.Key) < 8 || len(c.Key) > 128 {
		return fmt.Errorf("ключ сессии должен содержать 8–128 символов")
	}
	if c.CorrectionMS < -2000 || c.CorrectionMS > 2000 {
		return fmt.Errorf("поправка микрофона: от −2000 до 2000 мс")
	}
	return nil
}
func sessionKey() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (e *engine) begin(c settings, mode string) error {
	if err := checkSettings(c); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mode != "" || e.exporting {
		return fmt.Errorf("сначала завершите текущую сессию")
	}
	dir, err := os.MkdirTemp("", "CoopRecord-sounds-")
	if err != nil {
		return err
	}
	e.padDir = dir
	e.pads = [padCount]padClip{}
	e.padsReady = false
	e.padReceiving = false
	e.padBytes = 0
	e.padTotal = 0
	e.cfg = c
	e.mode = mode
	e.lastError = ""
	e.inputReady = false
	e.peers = make(map[*peer]bool)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.padVoices, e.padIndex = 0, -1
	e.padUntil, e.padCommandTime = 0, 0
	e.padCommands = make(chan padPlayback, 32)
	go e.runPads(e.ctx, e.padCommands)
	e.audio = make(chan audioEvent, 1024)
	e.inputMu.Lock()
	e.audioID = ""
	e.inputMu.Unlock()
	e.status = "Подготовка микрофона…"
	return nil
}

func (e *engine) startInput() error {
	e.mu.Lock()
	ctx, device, audio := e.ctx, e.cfg.DeviceID, e.audio
	e.mu.Unlock()
	ready := make(chan error, 1)
	go e.processAudio(ctx, audio)
	go func() {
		err := e.capture(ctx, device, ready, func(p packet) error {
			e.inputMu.Lock()
			defer e.inputMu.Unlock()
			if ctx.Err() != nil {
				return nil
			}
			id := e.audioID
			if id == "" {
				return nil
			}
			select {
			case audio <- audioEvent{packet: p, session: id}:
				return nil
			default:
				return fmt.Errorf("сеть или диск не успевает принимать звук; запись остановлена")
			}
		}, &e.level)
		if err != nil && ctx.Err() == nil {
			e.fail(err)
			e.mu.Lock()
			e.inputReady = false
			w := e.client
			e.mu.Unlock()
			if w != nil {
				_ = w.send(message{Type: "error", Error: err.Error()})
				w.Close()
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.inputReady = true
		e.mu.Unlock()
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("микрофон не отвечает")
	}
}

func (e *engine) processAudio(ctx context.Context, audio <-chan audioEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-audio:
			if ctx.Err() != nil {
				return
			}
			e.mu.Lock()
			w := e.client
			var err error
			if e.mode == "host" && e.rec != nil && item.session == e.rec.id && item.done == nil {
				err = e.rec.local.append(item.packet)
			}
			if item.done != nil {
				close(item.done)
			}
			e.mu.Unlock()
			if w != nil && item.session != "" {
				if item.done == nil {
					err = w.send(message{Type: "audio", Session: item.session, Time: item.packet.Time, PCM: item.packet.PCM, Discontinuity: item.packet.Discontinuity})
				} else {
					err = w.send(message{Type: "end", Session: item.session})
				}
			}
			if err != nil {
				e.fail(err)
				if w != nil {
					w.Close()
				}
				return
			}
		}
	}
}

func (e *engine) host(c settings) error {
	if c.Folder == "" {
		return fmt.Errorf("выберите папку записей")
	}
	if err := os.MkdirAll(c.Folder, 0700); err != nil {
		return err
	}
	if err := e.begin(c, "host"); err != nil {
		return err
	}
	if err := e.preparePads(c.PadFiles); err != nil {
		e.disconnect()
		return err
	}
	ln, err := net.Listen("tcp", c.Address)
	if err != nil {
		e.disconnect()
		return err
	}
	e.mu.Lock()
	e.listener = ln
	ctx := e.ctx
	e.mu.Unlock()
	if err = e.startInput(); err != nil {
		e.disconnect()
		return err
	}
	e.mu.Lock()
	e.status = "Хост готов. Передайте другу IP RadminVPN, порт и ключ сессии."
	e.mu.Unlock()
	go func() {
		// Limit unauthenticated sockets as well as admitted participants.
		slots := make(chan struct{}, 16)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				go func() { defer func() { <-slots }(); e.servePeer(ctx, &wire{Conn: conn}) }()
			default:
				conn.Close()
			}
		}
	}()
	return nil
}

func (e *engine) join(c settings) error {
	if err := e.begin(c, "guest"); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", c.Address, 5*time.Second)
	if err != nil {
		e.disconnect()
		return err
	}
	w := &wire{Conn: conn}
	e.mu.Lock()
	e.client = w
	ctx := e.ctx
	e.mu.Unlock()
	if err = w.send(message{Type: "hello", Version: protocolVersion, Name: c.Name, Key: c.Key, CorrectionMS: c.CorrectionMS}); err == nil {
		var reply message
		reply, err = w.receive()
		if err == nil && reply.Type != "welcome" {
			err = fmt.Errorf("хост: %s", reply.Error)
		}
		if err == nil && reply.Version != protocolVersion {
			err = fmt.Errorf("несовместимая версия хоста: обновите CoopRecord на обоих ПК")
		}
	}
	if err != nil {
		e.disconnect()
		return err
	}
	if err = e.startInput(); err != nil {
		e.disconnect()
		return err
	}
	if err = w.send(message{Type: "ready"}); err != nil {
		e.disconnect()
		return err
	}
	e.mu.Lock()
	e.status = "Подключено. Хост управляет началом и остановкой записи."
	e.mu.Unlock()
	go e.readHost(ctx, w)
	return nil
}

func (e *engine) servePeer(ctx context.Context, w *wire) {
	defer w.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			w.Close()
		case <-finished:
		}
	}()
	m, err := w.receive()
	if err != nil {
		return
	}
	e.mu.Lock()
	valid := m.Type == "hello" && m.Version == protocolVersion && len(m.Name) > 0 && utf8.RuneCountInString(m.Name) <= 40 && !strings.ContainsAny(m.Name, "\x00\r\n") && m.CorrectionMS >= -2000 && m.CorrectionMS <= 2000 && subtle.ConstantTimeCompare([]byte(m.Key), []byte(e.cfg.Key)) == 1
	if !valid || ctx.Err() != nil || e.rec != nil || e.exporting || len(e.peers) >= maxPeers {
		e.mu.Unlock()
		_ = w.send(message{Type: "error", Error: "Неверный ключ/версия, сессия уже записывается или заполнена. При несовпадении версий обновите CoopRecord на обоих ПК."})
		return
	}
	e.nextPeer++
	p := &peer{w: w, name: m.Name, correction: m.CorrectionMS, number: e.nextPeer}
	e.peers[p] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.peers, p)
		active := e.rec != nil && e.rec.tracks[p] != nil
		if active {
			select {
			case <-e.rec.ended[p]:
			default:
				close(e.rec.ended[p])
			}
		}
		e.mu.Unlock()
		if active && ctx.Err() == nil {
			e.fail(fmt.Errorf("%s отключился; полученный звук сохранён", p.name))
		}
	}()
	if w.send(message{Type: "welcome", Version: protocolVersion}) != nil {
		return
	}
	pingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.pingPeer(pingCtx, p)
	go e.sendPads(pingCtx, p)
	for {
		m, err = w.receive()
		received := clockNow()
		if err != nil {
			return
		}
		if m.Type == "pad_request" {
			if err = e.triggerPad(m.Pad); err != nil {
				_ = w.send(message{Type: "pad_notice", Error: err.Error()})
			}
			continue
		}
		e.mu.Lock()
		r := e.rec
		switch m.Type {
		case "pads_progress", "pads_ready":
			if m.Offset < p.padBytes || m.Offset > e.padTotal || (m.Type == "pads_ready" && m.Offset != e.padTotal) {
				err = fmt.Errorf("неверный прогресс саундпада")
			} else {
				p.padBytes = m.Offset
				p.padsReady = m.Type == "pads_ready"
			}
		case "ready":
			p.ready = true
		case "pong":
			if m.T0 != p.pending {
				err = fmt.Errorf("неожиданный ответ часов")
			} else {
				var s clockSample
				s, err = sampleClock(m.T0, m.T1, m.T2, received)
				p.pending = 0
				if err == nil {
					p.samples = append(p.samples, s)
					if len(p.samples) > 12000 {
						p.samples = p.samples[len(p.samples)-12000:]
					}
					if r != nil && r.tracks[p] != nil {
						err = r.tracks[p].clock(s)
					}
				}
			}
		case "audio":
			if r == nil || r.id != m.Session || r.tracks[p] == nil {
				err = fmt.Errorf("аудио вне активной записи")
			} else {
				select {
				case <-r.ended[p]:
					err = fmt.Errorf("аудио после конца дорожки")
				default:
				}
				if err == nil && len(p.samples) > 0 && abs64(mapClock(m.Time, p.samples[len(p.samples)-1:])-received) > int64(30*time.Second) {
					err = fmt.Errorf("аудиометка за пределами допустимого времени")
				}
				if err == nil {
					err = r.tracks[p].append(packet{Time: m.Time, PCM: m.PCM, Discontinuity: m.Discontinuity})
				}
			}
		case "end":
			if r != nil && r.id == m.Session && r.tracks[p] != nil {
				select {
				case <-r.ended[p]:
				default:
					close(r.ended[p])
				}
			}
		case "error":
			err = fmt.Errorf("%s: %s", p.name, m.Error)
		default:
			err = fmt.Errorf("неизвестная команда участника")
		}
		e.mu.Unlock()
		if err != nil {
			e.fail(err)
			return
		}
	}
}
func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func (e *engine) pingPeer(ctx context.Context, p *peer) {
	for {
		e.mu.Lock()
		delay := 200 * time.Millisecond
		if len(p.samples) >= 8 {
			delay = 2 * time.Second
		}
		t := int64(0)
		if p.pending == 0 {
			t = clockNow()
			p.pending = t
		}
		e.mu.Unlock()
		if t != 0 && p.w.send(message{Type: "ping", T0: t}) != nil {
			p.w.Close()
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (e *engine) readHost(ctx context.Context, w *wire) {
	defer w.Close()
	for {
		m, err := w.receive()
		received := clockNow()
		if err != nil {
			if ctx.Err() == nil {
				e.fail(fmt.Errorf("соединение с хостом потеряно: %w", err))
				e.mu.Lock()
				e.liveID = ""
				e.inputReady = false
				e.cancel()
				e.mu.Unlock()
			}
			return
		}
		switch m.Type {
		case "pads_begin", "pad_chunk", "pads_end":
			err = e.receivePads(ctx, w, m)
		case "pad_notice":
			e.mu.Lock()
			e.lastError = m.Error
			e.mu.Unlock()
		case "pad_play":
			e.mu.Lock()
			if !e.padsReady || m.Pad < -1 || m.Pad >= padCount || (m.Pad >= 0 && len(e.pads[m.Pad].PCM) == 0) || abs64(m.Time-clockNow()) > int64(10*time.Second) {
				err = fmt.Errorf("неверная команда воспроизведения")
			} else {
				err = e.schedulePadLocked(ctx, m.Pad, m.Time)
			}
			e.mu.Unlock()
		case "ping":
			err = w.send(message{Type: "pong", T0: m.T0, T1: received, T2: clockNow()})
		case "start":
			e.mu.Lock()
			if e.liveID != "" || m.Session == "" {
				err = fmt.Errorf("неверная команда начала записи")
			} else {
				e.liveID = m.Session
				e.inputMu.Lock()
				e.audioID = m.Session
				e.inputMu.Unlock()
				e.status = "Идёт запись. Звук сохраняется на компьютере хоста."
			}
			e.mu.Unlock()
		case "stop":
			e.mu.Lock()
			if e.liveID == m.Session && m.Session != "" {
				e.liveID = ""
				e.inputMu.Lock()
				e.audioID = ""
				done := make(chan struct{})
				select {
				case e.audio <- audioEvent{session: m.Session, done: done}:
					e.status = "Запись остановлена. Хост собирает WAV-файлы."
				default:
					err = fmt.Errorf("переполнена очередь аудио")
				}
				e.inputMu.Unlock()
			}
			e.mu.Unlock()
		case "error":
			err = fmt.Errorf("хост: %s", m.Error)
		default:
			err = fmt.Errorf("неизвестная команда хоста")
		}
		if err != nil {
			e.fail(err)
			e.mu.Lock()
			e.liveID = ""
			e.cancel()
			e.inputReady = false
			e.mu.Unlock()
			return
		}
	}
}

func (e *engine) startRecording() error {
	// A pad request must finish scheduling before we test whether playback is idle.
	e.padTriggerMu.Lock()
	defer e.padTriggerMu.Unlock()
	e.mu.Lock()
	if e.mode != "host" || !e.inputReady || e.rec != nil || e.exporting || !e.padsReadyLocked() || e.padVoices > 0 {
		e.mu.Unlock()
		return fmt.Errorf("хост не готов к записи")
	}
	ps := e.sortedPeers()
	for _, p := range ps {
		if !p.ready || len(p.samples) < 8 {
			e.mu.Unlock()
			return fmt.Errorf("дождитесь готовности всех участников")
		}
	}
	dir, err := os.MkdirTemp(e.cfg.Folder, time.Now().Format("2006-01-02_15-04-05")+"_")
	if err != nil {
		e.mu.Unlock()
		return err
	}
	r := &recording{dir: dir, id: filepath.Base(dir), tracks: make(map[*peer]*trackWriter), ended: make(map[*peer]chan struct{}), done: make(chan struct{})}
	r.m = manifest{Version: 4, Created: time.Now().Format(time.RFC3339), Start: clockNow() + int64(time.Second)}
	r.m.Tracks = append(r.m.Tracks, trackInfo{ID: "track-00", Name: e.cfg.Name, CorrectionMS: e.cfg.CorrectionMS})
	r.local, err = newTrack(dir, "track-00")
	if err == nil {
		for i, p := range ps {
			id := fmt.Sprintf("track-%02d", i+1)
			var w *trackWriter
			w, err = newTrack(dir, id)
			if err != nil {
				break
			}
			r.tracks[p] = w
			r.ended[p] = make(chan struct{})
			r.m.Tracks = append(r.m.Tracks, trackInfo{ID: id, Name: p.name, CorrectionMS: p.correction, Remote: true})
			for _, s := range p.samples[max(0, len(p.samples)-8):] {
				if err = w.clock(s); err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = e.preparePadRecording(r)
	}
	if err == nil {
		// Copying the sound bank can take longer than the one-second start lead.
		r.m.Start = clockNow() + int64(time.Second)
		err = saveManifest(dir, r.m)
	}
	if err != nil {
		if r.padEvents != nil {
			r.padEvents.Close()
		}
		if r.local != nil {
			r.local.close()
		}
		for _, w := range r.tracks {
			w.close()
		}
		e.mu.Unlock()
		return err
	}
	e.rec = r
	e.inputMu.Lock()
	e.audioID = r.id
	e.inputMu.Unlock()
	e.lastError = ""
	e.lastFolder = dir
	e.status = "Запись начнётся через 1 секунду…"
	e.mu.Unlock()
	for _, p := range ps {
		if err = p.w.send(message{Type: "start", Session: r.id, Time: r.m.Start}); err != nil {
			e.fail(err)
			return err
		}
	}
	go func() {
		timer := time.NewTimer(maxDuration + time.Second)
		defer timer.Stop()
		select {
		case <-r.done:
			return
		case <-timer.C:
			e.fail(fmt.Errorf("достигнут лимит 6 часов; начните новую запись"))
		}
	}()
	return nil
}

func (e *engine) fail(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	e.lastError = err.Error()
	e.status = "Ошибка: " + err.Error()
	r := e.rec
	if r != nil {
		r.m.Warnings = append(r.m.Warnings, err.Error())
	}
	e.mu.Unlock()
	if r != nil {
		go func() { _ = e.stopRecording() }()
	}
}

func (e *engine) stopRecording() error {
	e.mu.Lock()
	r := e.rec
	if r == nil {
		e.mu.Unlock()
		return nil
	}
	if r.stopping {
		done := r.done
		e.mu.Unlock()
		<-done
		return nil
	}
	r.stopping = true
	r.m.End = max(clockNow(), r.m.Start+int64(time.Millisecond))
	e.status = "Получение последних аудиоблоков…"
	localDone := make(chan struct{})
	e.inputMu.Lock()
	e.audioID = ""
	select {
	case e.audio <- audioEvent{session: r.id, done: localDone}:
	default:
		close(localDone)
		r.m.Warnings = append(r.m.Warnings, "Переполнена локальная очередь аудио.")
	}
	e.inputMu.Unlock()
	e.mu.Unlock()
	for p := range r.tracks {
		if err := p.w.send(message{Type: "stop", Session: r.id}); err != nil {
			p.w.Close()
		}
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	channels := []chan struct{}{localDone}
	for _, ch := range r.ended {
		channels = append(channels, ch)
	}
	timedOut := false
	for _, ch := range channels {
		select {
		case <-ch:
		case <-deadline.C:
			timedOut = true
		}
		if timedOut {
			break
		}
	}
	e.mu.Lock()
	if timedOut {
		r.m.Warnings = append(r.m.Warnings, "Не все последние блоки получены за 10 секунд; дорожки могут оканчиваться тишиной.")
		for p := range r.tracks {
			p.w.Close()
		}
	}
	err := r.local.close()
	if r.padEvents != nil {
		err = errors.Join(err, r.padEvents.Sync(), r.padEvents.Close())
	}
	for _, w := range r.tracks {
		err = errors.Join(err, w.close())
	}
	err = errors.Join(err, saveManifest(r.dir, r.m))
	e.rec = nil
	e.exporting = true
	e.status = "Создание отдельных WAV и общего микса…"
	e.mu.Unlock()
	var result manifest
	if err == nil {
		result, err = exportSession(r.dir)
	}
	e.mu.Lock()
	e.exporting = false
	if err != nil {
		e.lastError = err.Error()
		e.status = "Не удалось собрать WAV. Исходники сохранены: " + r.dir
	} else {
		e.status = "Сохранено: " + r.dir
		if len(result.Warnings) > 0 {
			e.lastError = strings.Join(result.Warnings, "; ")
			e.status += " (есть предупреждения)"
		}
	}
	close(r.done)
	e.mu.Unlock()
	return err
}

func (e *engine) disconnect() {
	_ = e.stopRecording()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
	if e.listener != nil {
		e.listener.Close()
		e.listener = nil
	}
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
	for p := range e.peers {
		p.w.Close()
	}
	e.peers = make(map[*peer]bool)
	e.mode = ""
	e.liveID = ""
	e.inputReady = false
	if e.padDir != "" {
		_ = os.RemoveAll(e.padDir)
		e.padDir = ""
	}
	e.padsReady = false
	e.padReceiving = false
	e.pads = [padCount]padClip{}
	e.padBytes = 0
	e.padTotal = 0
	e.status = "Отключено. Можно создать новую сессию."
	e.level.Store(0)
}
