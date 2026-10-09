package main

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newTestEngine(capture captureFunc) *engine {
	e := newEngine(capture)
	e.voiceOutput = func(ctx context.Context, ready chan<- error, fill func([]byte)) error {
		ready <- nil
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		out := make([]byte, voiceFrames*2)
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
				fill(out)
			}
		}
	}
	return e
}

func TestVoiceSessionAllParticipants(t *testing.T) {
	var heard [3]atomic.Int32
	var ticks [3]atomic.Int32
	engines := make([]*engine, 3)
	for i, amplitude := range []uint16{1000, 2000, 4000} {
		source := func(ctx context.Context, id string, ready chan<- error, emit func(packet) error, level *atomic.Int32) error {
			return syntheticCapture(ctx, id, ready, func(p packet) error {
				for j := 0; j < len(p.PCM); j += 2 {
					binary.LittleEndian.PutUint16(p.PCM[j:], amplitude)
				}
				return emit(p)
			}, level)
		}
		e := newTestEngine(source)
		output := e.voiceOutput
		e.voiceOutput = func(ctx context.Context, ready chan<- error, fill func([]byte)) error {
			return output(ctx, ready, func(b []byte) {
				fill(b)
				heard[i].Store(int32(int16(binary.LittleEndian.Uint16(b[len(b)/2:]))))
				ticks[i].Add(1)
			})
		}
		engines[i] = e
		defer e.disconnect()
	}
	host := engines[0]
	folder := t.TempDir()
	c := settings{Name: "Host", DeviceID: "test", Key: "voice-test-key", Address: "127.0.0.1:0", Folder: folder}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	c.Address = host.listener.Addr().String()
	host.mu.Unlock()
	for _, e := range engines[1:] {
		c.Name = "Guest"
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
	checkHeard([3]int32{6000, 5000, 3000}) // Each recipient hears everyone except themselves.
	waitFor(t, func() bool {
		return len(host.mixerStrips()) == 4 && len(engines[1].mixerStrips()) == 4 && len(engines[2].mixerStrips()) == 4
	})
	strips := engines[1].mixerStrips()[1:]
	if strips[1].Name != "Host" || strips[2].Name != "Guest" {
		t.Fatal("wrong mixer names", strips)
	}
	strips[1].Channel.volume.Store(50)
	strips[2].Channel.volume.Store(0)
	checkHeard([3]int32{6000, 500, 3000}) // Only this listener's mix changes, with no self-monitoring.
	if strips[1].Channel.peak.Load() != 3 || strips[2].Channel.peak.Load() != 12 {
		t.Fatal("input meters must remain active at half volume and mute")
	}
	strips[1].Channel.volume.Store(200)
	checkHeard([3]int32{6000, 2000, 3000})
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) != 0 {
		t.Fatal("conversation created recording files", err)
	}
	for _, e := range engines {
		e.voice.mu.Lock()
		_, self := e.voice.streams[e.voice.source]
		e.voice.mu.Unlock()
		if self {
			t.Fatal("microphone echoed back to its owner")
		}
	}
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	// Disk operations use engine.mu. Voice must continue even while that lock is stalled.
	host.mu.Lock()
	before := [3]int32{ticks[0].Load(), ticks[1].Load(), ticks[2].Load()}
	time.Sleep(250 * time.Millisecond)
	for i := range engines {
		if ticks[i].Load()-before[i] < 5 {
			t.Errorf("voice blocked by recording lock: %d", i)
		}
	}
	host.mu.Unlock()
	checkHeard([3]int32{6000, 2000, 3000})
	time.Sleep(150 * time.Millisecond)
	if err := host.stopRecording(); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"track-00.wav", "track-01.wav", "track-02.wav"} {
		b, err := os.ReadFile(filepath.Join(host.snapshot().Folder, name))
		if err != nil {
			t.Fatal(err)
		}
		want := []uint16{1000, 2000, 4000}[i]
		if len(b) < 44+sampleRate/2 || binary.LittleEndian.Uint16(b[44+sampleRate/2:]) != want {
			t.Fatal("recording mixed in conversation output", name)
		}
	}
	// Check fresh output after stop, not a value left over from before it.
	strips[1].Channel.volume.Store(100)
	strips[2].Channel.volume.Store(100)
	for i := range heard {
		heard[i].Store(-1)
	}
	checkHeard([3]int32{6000, 5000, 3000})
	old := engines[2].voice
	engines[2].disconnect()
	waitFor(t, func() bool { return heard[0].Load() == 2000 && heard[1].Load() == 1000 })
	waitFor(t, func() bool { return len(host.mixerStrips()) == 3 && len(engines[1].mixerStrips()) == 3 })
	if engines[1].mixerStrips()[2].Channel != strips[1].Channel {
		t.Fatal("surviving channel lost its controls")
	}
	if old.ctx.Err() == nil {
		t.Fatal("voice survived disconnect")
	}
	if err := engines[2].join(c); err != nil {
		t.Fatal(err)
	}
	checkHeard([3]int32{6000, 5000, 3000})
	waitFor(t, func() bool { return len(engines[1].mixerStrips()) == 4 })
	if engines[1].mixerStrips()[3].Channel == strips[2].Channel {
		t.Fatal("reconnected guest retained old channel")
	}
	v := engines[2].voice
	bad := make([]byte, voicePacketSize)
	copy(bad, "CRV7")
	binary.LittleEndian.PutUint32(bad[4:], v.source)
	binary.LittleEndian.PutUint64(bad[8:], 1e9)
	copy(bad[16:], old.token[:]) // Old session token must not authorize a reconnected peer.
	if _, err := v.conn.WriteToUDP(bad, v.server); err != nil {
		t.Fatal(err)
	}
	if _, err := v.conn.WriteToUDP(make([]byte, 3000), v.server); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	host.voice.mu.Lock()
	latest := host.voice.streams[v.source].latest
	host.voice.mu.Unlock()
	if latest >= 1e9 {
		t.Fatal("accepted old/invalid voice token")
	}
	for i := range heard {
		heard[i].Store(-1)
	}
	checkHeard([3]int32{6000, 5000, 3000})
}

func TestVoiceJitterBuffer(t *testing.T) {
	v := &voiceSession{streams: make(map[uint32]*voiceStream)}
	pcm := make([]byte, voiceFrames*2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 2000)
	}
	now := time.Now().Add(-100 * time.Millisecond)
	for _, seq := range []uint64{2, 1, 4, 3, 6, 5, 8, 7} {
		if !v.pushLocked(1, seq, pcm, now) {
			t.Fatal("reordered packet rejected")
		}
	}
	if v.pushLocked(1, 3, pcm, now) {
		t.Fatal("duplicate accepted")
	}
	out := make([]byte, voiceFrames*2)
	v.fill(out)
	for i := 0; i < len(out); i += 2 {
		if binary.LittleEndian.Uint16(out[i:]) != 2000 {
			t.Fatal("jitter caused an audible gap")
		}
	}
	for seq := uint64(9); seq < 100; seq++ {
		v.pushLocked(1, seq, pcm, now)
	}
	v.fill(out)
	if v.streams[1].cursor < 90*voiceFrames {
		t.Fatal("stale voice accumulated after a stall")
	}
	if v.pushLocked(1, 1, pcm, now) {
		t.Fatal("stale packet accepted")
	}
	// A lost packet contributes silence, and the following packet stays on its timeline.
	s := &voiceStream{}
	s.slots[3].seq = 3
	copy(s.slots[3].pcm[:], pcm)
	if s.sample(2*voiceFrames) != 0 || s.sample(3*voiceFrames) != 2000 {
		t.Fatal("packet loss shifted the timeline")
	}
	// Simulate two minutes of output versus microphone clocks differing by 200 ppm.
	for _, speed := range []float64{0.9998, 1.0002} {
		v = &voiceSession{streams: make(map[uint32]*voiceStream)}
		for seq := uint64(1); seq <= 6; seq++ {
			v.pushLocked(1, seq, pcm, now)
		}
		produced, seq := 0.0, uint64(6)
		for tick := 0; tick < 12000; tick++ {
			produced += speed
			for produced >= 1 {
				seq++
				v.pushLocked(1, seq, pcm, time.Now())
				produced--
			}
			v.fill(out)
			for pos := 0; pos < len(out); pos += 2 {
				if binary.LittleEndian.Uint16(out[pos:]) != 2000 {
					t.Fatalf("clock drift caused gap: speed %f, tick %d", speed, tick)
				}
			}
		}
	}
}

func TestVoiceOutputSmoke(t *testing.T) {
	if os.Getenv("COOPRECORD_VOICE_SMOKE") != "1" {
		t.Skip("opt-in Windows voice output with silence")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	ready := make(chan error, 1)
	var frames atomic.Int32
	err := renderVoice(ctx, ready, func(b []byte) { clear(b); frames.Add(int32(len(b) / 2)) })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if frames.Load() < sampleRate/10 {
		t.Fatal("output did not stream")
	}
}

func TestVoiceUDPPortReleased(t *testing.T) {
	e := newTestEngine(syntheticCapture)
	c := settings{Name: "test", DeviceID: "test", Key: "voice-key", Address: "127.0.0.1:0", Folder: t.TempDir()}
	if err := e.host(c); err != nil {
		t.Fatal(err)
	}
	addr := e.voice.conn.LocalAddr().(*net.UDPAddr)
	e.disconnect()
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal("voice port leaked", err)
	}
	conn.Close()
}

func TestVoiceRejectJoinDuringDisconnect(t *testing.T) {
	host, guest := newTestEngine(syntheticCapture), newTestEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	c := settings{Name: "test", DeviceID: "test", Key: "voice-key", Address: "127.0.0.1:0", Folder: t.TempDir()}
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	c.Address = host.listener.Addr().String()
	// Disconnect closes voice before recording export and TCP teardown complete.
	host.voice.close()
	host.voice = nil
	host.mu.Unlock()
	if err := guest.join(c); err == nil {
		t.Fatal("joined a closing session")
	}
}
