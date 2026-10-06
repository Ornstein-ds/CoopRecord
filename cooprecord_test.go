package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClockIgnoresWallTimeAndTracksDrift(t *testing.T) {
	h := int64(100 * time.Second)
	offset := int64(13 * time.Hour)
	s, err := sampleClock(h, h+offset+5e6, h+offset+6e6, h+11e6)
	if err != nil || s.RTT != 10e6 || mapClock(h+offset, []clockSample{s}) != h {
		t.Fatalf("clock sample: %+v %v", s, err)
	}
	anchors := []clockSample{{Remote: h + offset, Host: h, RTT: 1}, {Remote: h + offset + 10e9 + 1e6, Host: h + 10e9, RTT: 1}}
	if got := mapClock(h+offset+5e9+500000, anchors); got != h+5e9 {
		t.Fatalf("clock drift: %d", got)
	}
	if _, err := sampleClock(10, 20, 19, 30); err == nil {
		t.Fatal("accepted reversed timestamps")
	}
	best := clockAnchors([]clockSample{{Remote: 1, Host: 1, RTT: 100}, {Remote: 2, Host: 2, RTT: 10}, {Remote: 3, Host: 3, RTT: 200}})
	if len(best) != 1 || best[0].RTT != 10 {
		t.Fatal("did not select least jittery sample")
	}
}

func TestWAVSynchronizationRecoveryAndMix(t *testing.T) {
	dir := t.TempDir()
	start := int64(100 * time.Second)
	remoteStart := int64(13*time.Hour) + start
	m := manifest{Version: 1, Start: start, End: start + 2e9, Tracks: []trackInfo{{ID: "track-00", Name: "Хост"}, {ID: "track-01", Name: "Друг", Remote: true}}}
	for i, track := range m.Tracks {
		w, err := newTrack(dir, track.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			for _, s := range []clockSample{{Remote: remoteStart, Host: start, RTT: 1e6}, {Remote: remoteStart + int64(21e9*1.00005), Host: start + 21e9, RTT: 1e6}} {
				if err = w.clock(s); err != nil {
					t.Fatal(err)
				}
			}
		}
		deviceScale := 1.0
		clockScale := 1.0
		origin := start
		if i == 1 {
			deviceScale = 1.001
			clockScale = 1.00005
			origin = remoteStart
		}
		for n := 0; n < 200; n++ {
			pcm := make([]byte, 960)
			for j := 0; j < 480; j++ {
				physical := float64(n*480+j) / sampleRate * deviceScale
				if math.Abs(physical-1) < 1.5/sampleRate {
					binary.LittleEndian.PutUint16(pcm[j*2:], 12000)
				}
			}
			stamp := origin + int64(float64(n)*1e7*deviceScale*clockScale)
			if err = w.append(packet{Time: stamp, PCM: pcm}); err != nil {
				t.Fatal(err)
			}
		}
		if err = w.close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if _, err := exportSession(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"track-00.wav", "track-01.wav", "mix.wav"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != 44+2*2*sampleRate || string(b[:4]) != "RIFF" || binary.LittleEndian.Uint32(b[40:]) != 2*2*sampleRate {
			t.Fatalf("invalid WAV %s", name)
		}
		peak, index := int16(0), 0
		for j := 44; j < len(b); j += 2 {
			n := int16(binary.LittleEndian.Uint16(b[j:]))
			if n > peak {
				peak = n
				index = (j - 44) / 2
			}
		}
		if math.Abs(float64(index-sampleRate)) > 2 || peak < 6000 {
			t.Fatalf("misaligned %s: peak=%d at %d", name, peak, index)
		}
	}
	// An interrupted recording has no end marker and may have a torn final block.
	m.End = 0
	if err := saveManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "track-01.capture"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{1, 2, 3})
	f.Close()
	recovered, err := exportSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Warnings) < 2 || recovered.End < start+2e9 {
		t.Fatalf("recovery: %+v", recovered)
	}
	m.Tracks[0].ID = "../escape"
	_ = saveManifest(dir, m)
	if _, err := exportSession(dir); err == nil {
		t.Fatal("accepted path traversal")
	}
}

func syntheticCapture(ctx context.Context, _ string, ready chan<- error, emit func(packet) error, level *atomic.Int32) error {
	ready <- nil
	start := clockNow()
	i := int64(0)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			for start+(i+1)*1e7 <= clockNow() {
				pcm := make([]byte, 960)
				for j := 0; j < len(pcm); j += 2 {
					binary.LittleEndian.PutUint16(pcm[j:], 1000)
				}
				if err := emit(packet{Time: start + i*1e7, PCM: pcm}); err != nil {
					return err
				}
				i++
				level.Store(3)
			}
		}
	}
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostGuestEndToEndAndReconnect(t *testing.T) {
	host := newEngine(syntheticCapture)
	guest := newEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	c := settings{Name: "Хост", DeviceID: "test", Address: "127.0.0.1:0", Folder: t.TempDir(), Key: "test-session-key"}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	address := host.listener.Addr().String()
	host.mu.Unlock()
	secondHost := newEngine(syntheticCapture)
	c2 := c
	c2.Address = address
	if err := secondHost.host(c2); err == nil {
		secondHost.disconnect()
		t.Fatal("two hosts bound the same address")
	}
	bad := newEngine(syntheticCapture)
	badCfg := c2
	badCfg.Key = "incorrect-key"
	if err := bad.join(badCfg); err == nil {
		bad.disconnect()
		t.Fatal("accepted wrong key")
	}
	c2.Name = "Участник"
	if err := guest.join(c2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	var previous string
	for i := 0; i < 2; i++ {
		if err := host.startRecording(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1250 * time.Millisecond)
		if err := host.stopRecording(); err != nil {
			t.Fatal(err)
		}
		v := host.snapshot()
		if v.Error != "" {
			t.Fatal(v.Error)
		}
		if previous == v.Folder {
			t.Fatal("session folder reused")
		}
		previous = v.Folder
		for _, name := range []string{"track-00.wav", "track-01.wav", "mix.wav"} {
			b, err := os.ReadFile(filepath.Join(v.Folder, name))
			if err != nil {
				t.Fatal(err)
			}
			heard := false
			for j := 44; j < len(b); j += 2 {
				if binary.LittleEndian.Uint16(b[j:]) != 0 {
					heard = true
					break
				}
			}
			if !heard {
				t.Fatalf("silent %s", name)
			}
		}
	}
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	guest.disconnect()
	waitFor(t, func() bool { v := host.snapshot(); return !v.Recording && !v.Exporting })
	if !strings.Contains(host.snapshot().Error, "отключился") {
		t.Fatal("disconnect was not reported")
	}
}

func TestBoundedWireAndTimestampValidation(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = b.Write([]byte{0xff, 0xff, 0xff, 0xff}) }()
	if _, err := (&wire{Conn: a}).receive(); err == nil {
		t.Fatal("accepted oversized message")
	}
	dir := t.TempDir()
	w, err := newTrack(dir, "track-00")
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err = w.append(packet{Time: 1, PCM: []byte{0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err = w.append(packet{Time: 1, PCM: []byte{0, 0}}); err == nil {
		t.Fatal("accepted duplicate timestamp")
	}
	if _, err = wavHeader(1<<32, 1); err == nil {
		t.Fatal("accepted RIFF overflow")
	}
	if _, err = readPacket(strings.NewReader("123")); err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
}

func TestMicrophoneSmoke(t *testing.T) {
	if os.Getenv("COOPRECORD_AUDIO_SMOKE") != "1" {
		t.Skip("opt-in real WASAPI device test")
	}
	d, err := inputDevices()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	t.Log(string(b))
	if len(d) == 0 {
		t.Skip("no microphones available")
	}
	for _, device := range d {
		t.Run(device.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ready := make(chan error, 1)
			var level atomic.Int32
			count := 0
			last := int64(0)
			err := captureAudio(ctx, device.ID, ready, func(p packet) error {
				if p.Time <= last || abs64(p.Time-clockNow()) > int64(time.Second) {
					t.Error("invalid QPC timestamp")
				}
				last = p.Time
				count++
				return nil
			}, &level)
			if err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				t.Fatal("no captured packets")
			}
			t.Logf("%d packets, selected endpoint opened successfully", count)
		})
	}
}
