package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

// WASAPI QPC timestamps can jitter even when its PCM sample stream is continuous.
// Keep both the stream and interpolation phase continuous across every packet seam.
func TestContinuousAudioWithNoisyPacketTimestamps(t *testing.T) {
	for _, noisy := range []bool{false, true} {
		t.Run(fmt.Sprintf("jitter=%v", noisy), func(t *testing.T) {
			dir := t.TempDir()
			const start = int64(100 * time.Second)
			const phase = 0.25
			const scale = 1.001
			track := trackInfo{ID: "track-00", Name: "Test"}
			m := manifest{Version: 1, Start: start, End: start + 2e9, Tracks: []trackInfo{track}}
			w, err := newTrack(dir, track.ID)
			if err != nil {
				t.Fatal(err)
			}
			value := func(i int) float64 { return math.Round(12000 + 6000*math.Sin(2*math.Pi*997*float64(i)/sampleRate)) }
			jitter := []int64{600000, -600000, -600000, 600000}
			for n := 0; n < 200; n++ {
				pcm := make([]byte, 960)
				for j := 0; j < 480; j++ {
					binary.LittleEndian.PutUint16(pcm[j*2:], uint16(int16(value(n*480+j))))
				}
				stamp := start + int64(math.Round((phase+float64(n*480)*scale)*1e9/sampleRate))
				if noisy {
					stamp += jitter[n%len(jitter)]
				}
				if err = w.append(packet{Time: stamp, PCM: pcm}); err != nil {
					t.Fatal(err)
				}
			}
			if err = w.close(); err != nil {
				t.Fatal(err)
			}
			if err = saveManifest(dir, m); err != nil {
				t.Fatal(err)
			}
			result, err := exportSession(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Warnings) != 0 {
				t.Fatalf("timestamp noise was treated as missing audio: %v", result.Warnings)
			}
			b, err := os.ReadFile(filepath.Join(dir, "mix.wav"))
			if err != nil {
				t.Fatal(err)
			}
			maxError := 0.0
			for i := 1; i < 2*sampleRate; i++ {
				x := (float64(i) - phase) / scale
				a := int(x)
				want := value(a) + (value(a+1)-value(a))*(x-float64(a))
				got := float64(int16(binary.LittleEndian.Uint16(b[44+i*2:])))
				maxError = math.Max(maxError, math.Abs(got-want))
			}
			if maxError > 2 {
				t.Fatalf("packet seams changed continuous audio: max error %.1f PCM units", maxError)
			}
		})
	}
}

func TestCaptureTimelinePreservesRealGap(t *testing.T) {
	dir := t.TempDir()
	start := int64(100 * time.Second)
	track := trackInfo{ID: "track-00", Name: "Test"}
	m := manifest{Version: 1, Start: start, End: start + 600e6, Tracks: []trackInfo{track}, Warnings: []string{"capture warning", "Test: разрывы временных меток: 535"}}
	w, err := newTrack(dir, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		stamp := start + int64(i)*1e7
		if i >= 20 {
			stamp += 200e6
		}
		pcm := make([]byte, 960)
		for j := 0; j < len(pcm); j += 2 {
			binary.LittleEndian.PutUint16(pcm[j:], 10000)
		}
		if err = w.append(packet{Time: stamp, PCM: pcm}); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.close(); err != nil {
		t.Fatal(err)
	}
	if err = saveManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	result, err := exportSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 2 || result.Warnings[0] != "capture warning" || result.Warnings[1] != "Test: разрывы временных меток: 1" {
		t.Fatalf("warnings: %v", result.Warnings)
	}
	b, err := os.ReadFile(filepath.Join(dir, "track-00.wav"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < sampleRate*6/10; i++ {
		want := uint16(10000)
		if i >= sampleRate/5 && i < sampleRate*2/5 {
			want = 0
		}
		if got := binary.LittleEndian.Uint16(b[44+i*2:]); got != want {
			t.Fatalf("real gap changed at sample %d: got %d, want %d", i, got, want)
		}
	}
}

// Optional read-only diagnostics: never renders or modifies the supplied session.
func TestRecordedCaptureClock(t *testing.T) {
	dir := os.Getenv("COOPRECORD_DIAGNOSE_SESSION")
	if dir == "" {
		t.Skip("opt-in read-only clock diagnostics")
	}
	for _, id := range []string{"track-00", "track-01"} {
		f, err := os.Open(filepath.Join(dir, id+".capture"))
		if err != nil {
			t.Fatal(err)
		}
		spans, err := captureTimeline(bufio.NewReader(f))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(spans) != 1 {
			t.Fatalf("%s: expected continuous recording, got %d spans", id, len(spans))
		}
		t.Logf("%s: %d continuous samples, clock rate %.9f, no false gaps", id, spans[0].to, spans[0].rate)
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
	host := newTestEngine(syntheticCapture)
	guest := newTestEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	c := settings{Name: "Хост", DeviceID: "test", Address: "127.0.0.1:0", Folder: t.TempDir(), Key: "test-session-key"}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	address := host.listener.Addr().String()
	host.mu.Unlock()
	secondHost := newTestEngine(syntheticCapture)
	c2 := c
	c2.Address = address
	if err := secondHost.host(c2); err == nil {
		secondHost.disconnect()
		t.Fatal("two hosts bound the same address")
	}
	bad := newTestEngine(syntheticCapture)
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
		// Wait for captured audio, not an arbitrary timer: slow Windows disk flushes
		// can delay the synthetic source while the test process is being scheduled.
		waitFor(t, func() bool {
			host.mu.Lock()
			defer host.mu.Unlock()
			r := host.rec
			if r == nil || r.local.lastTime < r.m.Start+250e6 || clockNow()-r.local.lastTime > 30e6 {
				return false
			}
			for _, w := range r.tracks {
				if clockNow()-w.lastTime > 30e6 {
					return false
				}
			}
			return true
		})
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

func TestCaptureDoesNotWaitForDiskLock(t *testing.T) {
	trigger := make(chan struct{})
	emitted := make(chan error, 1)
	source := func(ctx context.Context, _ string, ready chan<- error, emit func(packet) error, _ *atomic.Int32) error {
		ready <- nil
		select {
		case <-ctx.Done():
			return nil
		case <-trigger:
		}
		err := emit(packet{Time: clockNow(), PCM: []byte{0, 0}})
		emitted <- err
		return err
	}
	e := newEngine(source)
	if err := e.begin(settings{Name: "test", DeviceID: "test", Key: "test-key"}, "host"); err != nil {
		t.Fatal(err)
	}
	defer e.disconnect()
	if err := e.startInput(); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock() // A slow Write/Sync holds this lock in processAudio/servePeer.
	e.inputMu.Lock()
	e.audioID = "test-session"
	e.inputMu.Unlock()
	close(trigger)
	var err error
	select {
	case err = <-emitted:
	case <-time.After(time.Second):
		err = fmt.Errorf("microphone callback blocked on disk mutex")
	}
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// Opt-in: force a WASAPI overrun without saving or transmitting any audio.
func TestMicrophoneRecoversAfterReaderDelay(t *testing.T) {
	if os.Getenv("COOPRECORD_AUDIO_GLITCH_SMOKE") != "1" {
		t.Skip("opt-in real microphone gap test")
	}
	devices, err := inputDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) == 0 {
		t.Skip("no input devices")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	var level atomic.Int32
	packets, gaps, after := 0, 0, 0
	err = captureAudio(ctx, devices[0].ID, ready, func(p packet) error {
		packets++
		if p.Discontinuity {
			gaps++
		}
		if gaps > 0 {
			after++
		}
		if packets == 5 {
			time.Sleep(350 * time.Millisecond)
		}
		return nil
	}, &level)
	if err != nil {
		t.Fatal(err)
	}
	if gaps == 0 || after < 10 {
		t.Fatalf("gap not exercised/recovered: packets=%d gaps=%d after=%d", packets, gaps, after)
	}
	t.Logf("%s: %d gap(s), %d packets after gap; capture continued", devices[0].Name, gaps, after)
}

func TestShortDriverGapSurvivesNetworkAndExport(t *testing.T) {
	dir := t.TempDir()
	const start = int64(100 * time.Second)
	const offset = int64(13 * time.Hour)
	m := manifest{Version: 2, Start: start, End: start + 600e6, Tracks: []trackInfo{{ID: "track-00", Name: "Host"}, {ID: "track-01", Name: "Guest", Remote: true}}}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	for trackIndex, track := range m.Tracks {
		w, err := newTrack(dir, track.ID)
		if err != nil {
			t.Fatal(err)
		}
		origin := start
		if track.Remote {
			origin += offset
			if err := w.clock(clockSample{Remote: origin, Host: start, RTT: 1}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 60; i++ {
			if track.Remote && (i == 20 || i == 21) {
				continue
			}
			pcm := make([]byte, 960)
			for j := 0; j < len(pcm); j += 2 {
				binary.LittleEndian.PutUint16(pcm[j:], 1000)
			}
			p := packet{Time: origin + int64(i)*1e7, PCM: pcm, Discontinuity: trackIndex == 1 && (i == 0 || i == 22)}
			if track.Remote {
				sent := make(chan error, 1)
				go func() {
					sent <- (&wire{Conn: a}).send(message{Type: "audio", Time: p.Time, PCM: p.PCM, Discontinuity: p.Discontinuity})
				}()
				msg, err := (&wire{Conn: b}).receive()
				if err != nil {
					t.Fatal(err)
				}
				if err = <-sent; err != nil {
					t.Fatal(err)
				}
				p = packet{Time: msg.Time, PCM: msg.PCM, Discontinuity: msg.Discontinuity}
			}
			if err = w.append(p); err != nil {
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
	result, err := exportSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "Guest: разрывы временных меток: 1" {
		t.Fatalf("warnings: %v", result.Warnings)
	}
	for _, name := range []string{"track-00.wav", "track-01.wav", "mix.wav"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < sampleRate*6/10; i++ {
			want := uint16(1000)
			if i >= sampleRate/5 && i < sampleRate*22/100 {
				if name == "track-01.wav" {
					want = 0
				}
				if name == "mix.wav" {
					want = 500
				}
			}
			if got := binary.LittleEndian.Uint16(data[44+i*2:]); got != want {
				t.Fatalf("%s sample %d: got %d want %d", name, i, got, want)
			}
		}
	}
}
