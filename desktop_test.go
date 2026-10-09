package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopSharingAndRecording(t *testing.T) {
	var heard [3]atomic.Int32
	var captures [3]atomic.Int32
	engines := make([]*engine, 3)
	for i, amplitude := range []uint16{2000, 4000, 8000} {
		e := newTestEngine(syntheticCapture)
		e.desktopCapture = func(ctx context.Context, id string, ready chan<- error, emit func(packet) error, level *atomic.Int32) error {
			captures[i].Add(1)
			return syntheticCapture(ctx, id, ready, func(p packet) error {
				for j := 0; j < len(p.PCM); j += 2 {
					binary.LittleEndian.PutUint16(p.PCM[j:], amplitude)
				}
				return emit(p)
			}, level)
		}
		output := e.voiceOutput
		e.voiceOutput = func(ctx context.Context, ready chan<- error, fill func([]byte)) error {
			return output(ctx, ready, func(b []byte) {
				fill(b)
				heard[i].Store(int32(int16(binary.LittleEndian.Uint16(b[len(b)/2:]))))
			})
		}
		engines[i] = e
		defer e.disconnect()
	}
	host, guest := engines[0], engines[1]
	c := settings{Name: "Host", DeviceID: "test", Key: "desktop-test", Address: "127.0.0.1:0", Folder: t.TempDir()}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	c.Address = host.listener.Addr().String()
	host.mu.Unlock()
	for i, e := range engines[1:] {
		c.Name = fmt.Sprintf("Guest%d", i+1)
		if err := e.join(c); err != nil {
			t.Fatal(err)
		}
	}
	checkHeard := func(want [3]int32) {
		t.Helper()
		waitFor(t, func() bool {
			return heard[0].Load() == want[0] && heard[1].Load() == want[1] && heard[2].Load() == want[2]
		})
	}
	checkHeard([3]int32{2000, 2000, 2000})
	for i, e := range engines {
		if !e.desktopMuted.Load() || captures[i].Load() != 0 || e.mixerStrips()[1].Channel != e.desktopMixer {
			t.Fatal("desktop is not the muted second channel", i)
		}
	}
	guest.desktopMixer.volume.Store(50)
	if err := guest.setDesktopMuted(false); err != nil {
		t.Fatal(err)
	}
	checkHeard([3]int32{4000, 2000, 4000}) // Guest's own desktop is never echoed back.
	if err := host.setDesktopMuted(false); err != nil {
		t.Fatal(err)
	}
	checkHeard([3]int32{4000, 4000, 6000})
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	start := host.rec.m.Start
	host.mu.Unlock()
	waitFor(t, func() bool { return clockNow() > start+400e6 })
	if err := guest.setDesktopMuted(true); err != nil {
		t.Fatal(err)
	}
	mutedAt := clockNow()
	checkHeard([3]int32{2000, 4000, 4000})
	time.Sleep(200 * time.Millisecond)
	unmutedAt := clockNow()
	guest.desktopMixer.volume.Store(200)
	if err := guest.setDesktopMuted(false); err != nil {
		t.Fatal(err)
	}
	checkHeard([3]int32{10000, 4000, 12000})
	time.Sleep(250 * time.Millisecond)
	if err := host.stopRecording(); err != nil {
		t.Fatal(err)
	}
	dir := host.snapshot().Folder
	var m manifest
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || json.Unmarshal(b, &m) != nil || len(m.Tracks) != 6 {
		t.Fatal("missing separate desktop tracks", err)
	}
	length := 0
	for i := range engines {
		mic, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("track-%02d.wav", i)))
		if err != nil {
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint16(mic[44+sampleRate/2:]) != 1000 {
			t.Fatal("desktop leaked into microphone WAV", i)
		}
		desktop, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("desktop-%02d.wav", i)))
		if err != nil || len(desktop) != len(mic) {
			t.Fatal("desktop length or file", i, err)
		}
		length = len(mic)
		seen := map[int16]int{}
		for j := 44; j < len(desktop); j += 2 {
			seen[int16(binary.LittleEndian.Uint16(desktop[j:]))]++
		}
		if i == 0 && seen[2000] < sampleRate/2 || i == 1 && (seen[2000] < sampleRate/5 || seen[8000] < sampleRate/10) || i == 2 && len(seen) != 1 {
			t.Fatal("wrong desktop audio or gain", i, seen)
		}
		if i == 1 {
			for frame := (mutedAt + 50e6 - start) * sampleRate / 1e9; frame < (unmutedAt-50e6-start)*sampleRate/1e9; frame++ {
				if binary.LittleEndian.Uint16(desktop[44+frame*2:]) != 0 {
					t.Fatal("audio recorded while muted")
				}
			}
		}
	}
	mix, err := os.ReadFile(filepath.Join(dir, "mix.wav"))
	if err != nil || len(mix) != length || binary.LittleEndian.Uint16(mix[44+sampleRate/2:]) != 5000 {
		t.Fatal("desktop missing from mix or silence attenuated microphones", err)
	}
	for _, warning := range m.Warnings {
		if strings.Contains(warning, "компьютер") {
			t.Fatal("intentional desktop silence reported as lost audio", warning)
		}
	}
	guest.disconnect()
	if !guest.desktopMuted.Load() {
		t.Fatal("disconnect left desktop enabled")
	}
	if err := guest.join(c); err != nil {
		t.Fatal(err)
	}
	if !guest.desktopMuted.Load() || captures[1].Load() != 2 {
		t.Fatal("reconnect silently resumed desktop capture")
	}
}

func TestDesktopCaptureSmoke(t *testing.T) {
	if os.Getenv("COOPRECORD_DESKTOP_SMOKE") != "1" {
		t.Skip("opt-in native desktop capture; no audio is saved")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	ready := make(chan error, 1)
	var level atomic.Int32
	var frames, previous int64
	err := captureDesktop(ctx, "", ready, func(p packet) error {
		if p.Time <= previous || abs64(p.Time-clockNow()) > int64(time.Second) || len(p.PCM)%2 != 0 {
			t.Error("invalid native loopback packet", p.Time, previous, len(p.PCM))
		}
		previous = p.Time
		frames += int64(len(p.PCM) / 2)
		return nil
	}, &level)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	t.Log("captured frames", frames)
	if frames < sampleRate/10 {
		t.Fatal("loopback did not deliver audio frames")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	err = captureDesktop(ctx, "", make(chan error, 1), func(packet) error { return nil }, &level)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		empty := true
		desktopActivations.Range(func(_, _ any) bool { empty = false; return false })
		return empty
	})
	runtime.GC() // Also catches leaked runtime.Pinner objects after cancellation.
}

func TestDesktopStartFailureStaysMuted(t *testing.T) {
	e := newTestEngine(syntheticCapture)
	defer e.disconnect()
	if err := e.setDesktopMuted(false); err == nil || !e.desktopMuted.Load() {
		t.Fatal("desktop capture enabled outside a session")
	}
	c := settings{Name: "Host", DeviceID: "test", Key: "desktop-failure", Address: "127.0.0.1:0", Folder: t.TempDir()}
	if err := e.host(c); err != nil {
		t.Fatal(err)
	}
	if err := e.startRecording(); err != nil {
		t.Fatal(err)
	}
	want := errors.New("capture unavailable")
	e.desktopCapture = func(ctx context.Context, _ string, ready chan<- error, _ func(packet) error, _ *atomic.Int32) error {
		ready <- want
		return want
	}
	if err := e.setDesktopMuted(false); !errors.Is(err, want) {
		t.Fatal("capture failure lost", err)
	}
	e.mu.Lock()
	active := e.rec != nil && !e.rec.stopping
	e.mu.Unlock()
	if !active || !e.desktopMuted.Load() {
		t.Fatal("failed unmute stopped microphone recording or enabled desktop")
	}
}
