package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWhisperParallelLocalSmoke(t *testing.T) {
	model, sample := os.Getenv("COOPRECORD_STT_MODEL"), os.Getenv("COOPRECORD_STT_SAMPLE")
	if model == "" || sample == "" {
		t.Skip("set COOPRECORD_STT_MODEL and COOPRECORD_STT_SAMPLE to test two local Whisper channels")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	s := newTranscription(ctx, nil, nil)
	var loaded, active, peak atomic.Int32
	bothLoaded := make(chan struct{})
	s.factory = func(ctx context.Context) (speechRecognizer, func(), error) {
		recognize, closeModel, err := startWhisper(ctx, model, false)
		if err != nil {
			return nil, nil, err
		}
		if loaded.Add(1) == 2 {
			close(bothLoaded)
		}
		return func(ctx context.Context, pcm []byte) ([]speechSegment, error) {
			select {
			case <-bothLoaded:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			n := active.Add(1)
			defer active.Add(-1)
			for previous := peak.Load(); n > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, n) {
					break
				}
			}
			return recognize(ctx, pcm)
		}, closeModel, nil
	}
	runTestTranscription(t, s, cancel)
	b, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := decodePadWAV(b)
	if err != nil {
		t.Fatal(err)
	}
	start := clockNow()
	r, err := s.beginRecord(t.TempDir(), "two-real-channels", start)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for source := 0; source < 2; source++ {
		for offset := 0; offset < len(pcm); offset += 1920 {
			s.feed(source, fmt.Sprintf("Канал %d", source+1), packet{Time: start + int64(offset/2)*1e9/sampleRate, PCM: pcm[offset:min(offset+1920, len(pcm))]})
		}
	}
	s.endRecord(r, start+int64(len(pcm)/2)*1e9/sampleRate)
	if err = s.finishRecord(r); err != nil {
		t.Fatal(err)
	}
	sources := map[int]bool{}
	for _, line := range r.lines {
		if strings.TrimSpace(line.Text) != "" {
			sources[line.Source] = true
			t.Logf("%s: %s", line.Name, line.Text)
		}
	}
	if len(sources) != 2 || loaded.Load() != 2 || peak.Load() < 2 {
		t.Fatalf("not independently recognized: sources=%v servers=%d concurrent=%d", sources, loaded.Load(), peak.Load())
	}
	t.Logf("Two %.2fs streams finalized in %s including model loading; peak concurrent requests: %d", float64(len(pcm))/96000, time.Since(before), peak.Load())
}
