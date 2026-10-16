package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runTestTranscription(t *testing.T, s *transcription, cancel context.CancelFunc) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.run(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("channel workers did not shut down")
		}
	})
}

func feedTestPhrase(s *transcription, source int, start int64, frames int) {
	for i := 0; i < frames; i++ {
		s.feed(source, "Одинаковое имя", speechPacket(start+int64(i)*int64(20*time.Millisecond), 1000))
	}
}

func TestSpeechChannelsHaveIndependentRecognizers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newTranscription(ctx, nil, nil)
	started := make(chan int, 2)
	release := make(chan struct{})
	var factories, closed atomic.Int32
	s.factory = func(context.Context) (speechRecognizer, func(), error) {
		id := int(factories.Add(1))
		return func(ctx context.Context, _ []byte) ([]speechSegment, error) {
			started <- id
			select {
			case <-release:
				return []speechSegment{{Text: "Готово"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, func() { closed.Add(1) }, nil
	}
	runTestTranscription(t, s, cancel)
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "parallel", start)
	if err != nil {
		t.Fatal(err)
	}
	feedTestPhrase(s, 0, start, 20)
	feedTestPhrase(s, 1, start, 20)
	s.dropSource(0)
	s.dropSource(1)
	seen := map[int]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(2 * time.Second):
			t.Fatal("one speaker blocked another speaker's recognizer")
		}
	}
	if len(seen) != 2 {
		t.Fatal("recognizer was shared between channels", seen)
	}
	close(release)
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
	if len(r.lines) != 2 || r.lines[0].Source == r.lines[1].Source {
		t.Fatal("lost channel identities", r.lines)
	}
	waitFor(t, func() bool { return closed.Load() == 2 })
}

func TestSpeechPreviewReplacedByFinalAndNotSaved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan transcriptLine, 8)
	var calls atomic.Int32
	s := newTranscription(ctx, func(context.Context, []byte) ([]speechSegment, error) {
		if calls.Add(1) == 1 {
			return []speechSegment{{Text: "Предварительный текст"}}, nil
		}
		return []speechSegment{{Start: 0.1, End: 2.3, Text: "Окончательный текст"}}, nil
	}, func(l transcriptLine) { updates <- l })
	runTestTranscription(t, s, cancel)
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "stream", start)
	if err != nil {
		t.Fatal(err)
	}
	feedTestPhrase(s, 0, start, 110)
	var preview transcriptLine
	select {
	case preview = <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("no text while speech was still active")
	}
	if !preview.Partial || preview.ID == 0 {
		t.Fatal("missing preview identity", preview)
	}
	b, err := os.ReadFile(filepath.Join(r.dir, "transcript.md"))
	if err != nil || strings.Contains(string(b), "Предварительный") {
		t.Fatal("preview was written to final transcript", err, string(b))
	}
	s.endRecord(r, start+int64(3*time.Second))
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
	final := <-updates
	if final.Partial || final.ID != preview.ID || final.EndAt <= final.At {
		t.Fatal("final did not replace preview", preview, final)
	}
	_, _, lines := s.snapshot(^uint64(0))
	if len(lines) != 1 || lines[0].Text != "Окончательный текст" {
		t.Fatal("duplicate utterance in history", lines)
	}
	// A delayed preview from the wire cannot roll a final result back.
	s.addLine(preview)
	_, _, lines = s.snapshot(^uint64(0))
	if lines[0].Partial {
		t.Fatal("late preview replaced final")
	}
	b, err = os.ReadFile(filepath.Join(r.dir, "transcript.md"))
	if err != nil || strings.Contains(string(b), "Предварительный") || strings.Count(string(b), "Окончательный текст") != 1 {
		t.Fatal("incorrect final transcript", err, string(b))
	}
}

func TestSpeechRequestFailureCanRecover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	s := newTranscription(ctx, func(context.Context, []byte) ([]speechSegment, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("temporary error")
		}
		return []speechSegment{{Text: "Восстановлено"}}, nil
	}, nil)
	runTestTranscription(t, s, cancel)
	start := clockNow()
	feedTestPhrase(s, 0, start, 20)
	s.mu.Lock()
	s.flush(s.streams[0])
	s.mu.Unlock()
	feedTestPhrase(s, 0, start+int64(time.Second), 20)
	s.dropSource(0)
	waitFor(t, func() bool {
		_, _, lines := s.snapshot(^uint64(0))
		return len(lines) == 1 && lines[0].Text == "Восстановлено"
	})
	if calls.Load() != 2 {
		t.Fatal("request failure disabled future recognition")
	}
}

func TestSpeechFinalWaitsForPreviewAndClearsEmptyResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	previewStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := newTranscription(ctx, func(ctx context.Context, _ []byte) ([]speechSegment, error) {
		if calls.Add(1) == 1 {
			close(previewStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []speechSegment{{Text: "Черновик"}}, nil
		}
		return nil, nil
	}, nil)
	runTestTranscription(t, s, cancel)
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "empty-final", start)
	if err != nil {
		t.Fatal(err)
	}
	feedTestPhrase(s, 0, start, 110)
	select {
	case <-previewStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not start")
	}
	s.endRecord(r, start+int64(3*time.Second))
	done := make(chan error, 1)
	go func() { done <- s.finishRecord(r) }()
	select {
	case <-done:
		t.Fatal("record finalized before its recognizer finished")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finalization hung")
	}
	_, _, lines := s.snapshot(^uint64(0))
	if len(lines) != 0 {
		t.Fatal("empty final left stale preview", lines)
	}
}

func TestSpeechQueueOverflowDoesNotBlockOtherChannelsOrStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newTranscription(ctx, func(context.Context, []byte) ([]speechSegment, error) { return nil, nil }, nil)
	// Keep the startup barrier closed while deliberately filling one channel.
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "overflow", start)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 35; i++ {
		feedTestPhrase(s, 0, start+int64(i)*int64(time.Second), 20)
		s.mu.Lock()
		s.flush(s.streams[0])
		s.mu.Unlock()
	}
	feedTestPhrase(s, 1, start, 20)
	s.mu.Lock()
	s.flush(s.streams[1])
	secondQueue := len(s.streams[1].worker.jobs)
	s.mu.Unlock()
	if secondQueue != 1 || r.dropped != 3 {
		t.Fatalf("overflow leaked across channels: dropped=%d other queue=%d", r.dropped, secondQueue)
	}
	// Cancel before inference starts: all accepted finals must release pending.
	cancel()
	runTestTranscription(t, s, cancel)
	done := make(chan error, 1)
	go func() { done <- s.finishRecord(r) }()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("missing overflow/cancellation warning")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overflow blocked recording finalization")
	}
}

func TestSpeechPreviewsReachGuestsAndBecomeFinal(t *testing.T) {
	host, guest := newTestEngine(syntheticCapture), newTestEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	host.speechFactory = func(context.Context, settings) (speechRecognizer, func(), error) {
		return func(context.Context, []byte) ([]speechSegment, error) {
			return []speechSegment{{Text: "Обновляемый текст"}}, nil
		}, func() {}, nil
	}
	c := settings{Name: "Хост", DeviceID: "test", Address: "127.0.0.1:0", Folder: t.TempDir(), Key: "stream-test-key", SpeechModel: "fake"}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	c.Address = host.listener.Addr().String()
	host.mu.Unlock()
	c.Name = "Гость"
	if err := guest.join(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	guest.mu.Lock()
	s := guest.transcript
	guest.mu.Unlock()
	var previewID uint64
	waitFor(t, func() bool {
		_, _, lines := s.snapshot(^uint64(0))
		for _, l := range lines {
			if l.Partial && l.Recording != "" {
				previewID = l.ID
				return true
			}
		}
		return false
	})
	if err := host.stopRecording(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, _, lines := s.snapshot(^uint64(0))
		for _, l := range lines {
			if l.ID == previewID {
				return !l.Partial
			}
		}
		return false
	})
	_, _, lines := s.snapshot(^uint64(0))
	count := 0
	for _, l := range lines {
		if l.ID == previewID {
			count++
		}
	}
	if count != 1 {
		t.Fatal("guest duplicated finalized preview", count)
	}
}
