package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func speechPacket(stamp int64, value int16) packet {
	p := packet{Time: stamp, PCM: make([]byte, 1920)}
	for i := 0; i < len(p.PCM); i += 2 {
		binary.LittleEndian.PutUint16(p.PCM[i:], uint16(value))
	}
	return p
}
func TestTranscriptBoundariesAndRoles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTranscription(ctx, func(context.Context, []byte) ([]speechSegment, error) {
		return []speechSegment{{Start: 0, Text: "Тестовая фраза\nна русском"}}, nil
	}, nil)
	go s.run()
	start := clockNow() + int64(time.Second)
	r, err := s.beginRecord(t.TempDir(), "first", start)
	if err != nil {
		t.Fatal(err)
	}
	s.endRecord(r, start+int64(time.Second))
	// Both speakers overlap. The utterance also crosses both recording boundaries.
	for i := -20; i < 90; i++ {
		for id, name := range []string{"Влад", "Друг"} {
			s.feed(id, name, speechPacket(start+int64(i)*int64(20*time.Millisecond), 1000))
		}
	}
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.dir, "transcript.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Влад", "Друг"} {
		if strings.Count(string(b), "[00:00:00.000] **"+name+":** Тестовая фраза на русском") != 1 {
			t.Fatalf("missing/duplicate role or shifted timestamp: %s", b)
		}
	}
	if len(r.lines) != 2 {
		t.Fatalf("outside audio leaked into recording: %+v", r.lines)
	}
	r2, err := s.beginRecord(t.TempDir(), "second", start+int64(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		s.feed(0, "Влад", speechPacket(r2.start+int64(i)*int64(20*time.Millisecond), 1000))
	}
	s.endRecord(r2, r2.start+int64(time.Second))
	if err = s.finishRecord(r2); err != nil {
		t.Fatal(err)
	}
	if len(r2.lines) != 1 || r2.lines[0].Recording != "second" || r2.lines[0].Time != 0 {
		t.Fatalf("recording boundary: %+v", r2.lines)
	}
}

func TestTranscriptSilenceAndNonblockingCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := make(chan struct{})
	s := newTranscription(ctx, func(context.Context, []byte) ([]speechSegment, error) { <-gate; return nil, nil }, nil)
	go s.run()
	start := clockNow()
	for i := 0; i < 100; i++ {
		s.feed(0, "Хост", speechPacket(start+int64(i)*20e6, 0))
	}
	s.mu.Lock()
	queued := s.queueSize()
	s.mu.Unlock()
	if queued != 0 {
		t.Fatal("silence queued")
	}
	r, err := s.beginRecord(t.TempDir(), "test", start+2e9)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for i := 100; i < 240; i++ {
		value := int16(1000)
		if i > 150 {
			value = 0
		}
		s.feed(0, "Хост", speechPacket(start+int64(i)*20e6, value))
	}
	if time.Since(before) > time.Second {
		t.Fatal("inference blocked capture")
	}
	s.endRecord(r, start+5e9)
	close(gate)
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
}

func TestWhisperRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" {
			t.Error(r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("language") != "ru" || r.FormValue("translate") != "false" {
			t.Error("wrong task/language")
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		b := make([]byte, 44)
		f.Read(b)
		if string(b[:4]) != "RIFF" || binary.LittleEndian.Uint32(b[24:]) != 16000 || binary.LittleEndian.Uint16(b[22:]) != 1 {
			t.Error("wrong WAV format")
		}
		json.NewEncoder(w).Encode(map[string]any{"segments": []speechSegment{{Start: 0.32, Text: "Привет"}}})
	}))
	defer server.Close()
	segments, err := whisperRecognize(server.URL)(context.Background(), make([]byte, 32000))
	if err != nil || len(segments) != 1 || segments[0].Text != "Привет" || segments[0].Start != 0.32 {
		t.Fatal(segments, err)
	}
}

func TestTranscriptCancellationAndFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newTranscription(ctx, func(ctx context.Context, _ []byte) ([]speechSegment, error) { <-ctx.Done(); return nil, ctx.Err() }, nil)
	go s.run()
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "cancel", start)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel with a partially accumulated phrase, before it can be queued.
	for i := 0; i < 20; i++ {
		s.feed(0, "Хост", speechPacket(start+int64(i)*20e6, 1000))
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.finishRecord(r) }()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("missing cancellation warning")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled inference blocked stop")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newTranscription(ctx2, func(context.Context, []byte) ([]speechSegment, error) { return nil, os.ErrNotExist }, nil)
	go s2.run()
	r2, err := s2.beginRecord(t.TempDir(), "failed", start)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		s2.feed(0, "Хост", speechPacket(start+int64(i)*20e6, 1000))
	}
	s2.endRecord(r2, start+1e9)
	if err = s2.finishRecord(r2); err == nil {
		t.Fatal("missing model failure warning")
	}
	if _, err = os.Stat(filepath.Join(r2.dir, "transcript.md")); err != nil {
		t.Fatal("lost partial transcript", err)
	}
}

func TestTranscriptHostGuestRecording(t *testing.T) {
	host, guest := newTestEngine(syntheticCapture), newTestEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	var jobs atomic.Int32
	host.speechFactory = func(context.Context, settings) (speechRecognizer, func(), error) {
		return func(context.Context, []byte) ([]speechSegment, error) {
			jobs.Add(1)
			time.Sleep(30 * time.Millisecond)
			return []speechSegment{{Text: "Проверка разговора"}}, nil
		}, func() {}, nil
	}
	c := settings{Name: "Влад", DeviceID: "test", Address: "127.0.0.1:0", Folder: t.TempDir(), Key: "test-session-key", SpeechModel: "fake"}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	c.Address = host.listener.Addr().String()
	host.mu.Unlock()
	c.Name = "Друг"
	if err := guest.join(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := host.stopRecording(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(host.snapshot().Folder, "transcript.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Влад", "Друг"} {
		if !strings.Contains(string(b), "**"+name+":** Проверка разговора") {
			t.Fatalf("missing speaker: %s", b)
		}
	}
	waitFor(t, func() bool {
		guest.mu.Lock()
		s := guest.transcript
		guest.mu.Unlock()
		_, _, lines := s.snapshot(^uint64(0))
		count := 0
		for _, l := range lines {
			if l.Recording != "" {
				count++
			}
		}
		return count >= 2
	})
	if jobs.Load() < 2 {
		t.Fatal("no recognition")
	}
}

func TestWhisperLocalSmoke(t *testing.T) {
	model := os.Getenv("COOPRECORD_STT_MODEL")
	sample := os.Getenv("COOPRECORD_STT_SAMPLE")
	if model == "" || sample == "" {
		t.Skip("set COOPRECORD_STT_MODEL and COOPRECORD_STT_SAMPLE to run local Vulkan recognition")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	recognize, closeModel, err := startWhisper(ctx, model, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeModel()
	b, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := decodePadWAV(b)
	if err != nil {
		t.Fatal(err)
	}
	var mono16 []byte
	for i := 0; i+5 < len(pcm); i += 6 {
		mono16 = append(mono16, pcm[i:i+2]...)
	}
	before := time.Now()
	segments, err := recognize(ctx, mono16)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, s := range segments {
		text.WriteString(s.Text)
	}
	if !strings.Contains(strings.ToLower(text.String()), "родион") {
		t.Fatalf("unexpected Russian result: %s", text.String())
	}
	t.Logf("%.2f seconds audio recognized in %s: %s", float64(len(mono16))/32000, time.Since(before), text.String())
	s := newTranscription(ctx, recognize, nil)
	go s.run()
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "smoke", start)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(pcm); offset += 1920 {
		s.feed(0, "Тестовый участник", packet{Time: start + int64(offset/2)*1e9/sampleRate, PCM: pcm[offset:min(offset+1920, len(pcm))]})
	}
	s.endRecord(r, start+int64(len(pcm)/2)*1e9/sampleRate)
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(r.dir, "transcript.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(md)), "родион") {
		t.Fatalf("segmented recognition failed: %s", md)
	}
	t.Log(string(md))
}
