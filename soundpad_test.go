package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPadMP3(t *testing.T) {
	for _, name := range []string{"tone-stereo", "tone-mono"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name+".mp3"))
			if err != nil {
				t.Fatal(err)
			}
			// Empty ID3v2 tag plus uppercase extension exercises the host import path.
			b = append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}, b...)
			path := filepath.Join(t.TempDir(), name+".MP3")
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			e := newEngine(syntheticCapture)
			e.padDir = t.TempDir()
			if err := e.preparePads([padCount]string{path}); err != nil {
				t.Fatal(err)
			}
			clip := e.pads[0]
			pcm, err := readPadPCM(e.padDir, 0, clip.Info)
			if err != nil || !bytes.Equal(pcm, clip.PCM) || len(pcm) < sampleRate/2 || len(pcm) > sampleRate*7/10 {
				t.Fatalf("bad MP3 import: %v, %d bytes", err, len(pcm))
			}
			// Check an interior 100 ms window, past encoder delay: frequency and mono gain.
			energy, crossings := 0.0, 0
			previous := int16(0)
			for i := sampleRate / 10; i < sampleRate/5; i++ {
				v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
				energy += float64(v) * float64(v)
				if previous < 0 && v >= 0 {
					crossings++
				}
				previous = v
			}
			expectedRMS := 12000 / math.Sqrt2
			if name == "tone-stereo" {
				expectedRMS /= 2
			}
			rms := math.Sqrt(energy / (sampleRate / 10))
			if crossings < 43 || crossings > 45 || math.Abs(rms-expectedRMS) > expectedRMS*0.15 {
				t.Fatalf("bad tone: %d crossings, RMS %.0f want %.0f", crossings, rms, expectedRMS)
			}
			for _, bad := range [][]byte{nil, []byte("not MP3"), []byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7f"), b[:len(b)/2], bytes.Repeat(b[10:], 500)} {
				if _, err := decodePadMP3(bad); err == nil {
					t.Fatal("accepted invalid or overlong MP3")
				}
			}
		})
	}
}

func testPadFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "test.wav")
	err := writeWAV(path, sampleRate/4, func(w io.Writer) error {
		pcm := make([]byte, sampleRate/2)
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], 2000)
		}
		_, err := w.Write(pcm)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSoundpadSessionTransferPlaybackAndExport(t *testing.T) {
	dir := t.TempDir()
	path := testPadFile(t, dir)
	host, guest := newEngine(syntheticCapture), newEngine(syntheticCapture)
	defer host.disconnect()
	defer guest.disconnect()
	hostPlayed, guestPlayed := make(chan int64, 4), make(chan int64, 4)
	host.playPad = func(_ context.Context, b []byte, start int64) error { hostPlayed <- start; return nil }
	guest.playPad = func(_ context.Context, b []byte, start int64) error { guestPlayed <- start; return nil }
	c := settings{Name: "Host", DeviceID: "test", Key: "test-pad-key", Address: "127.0.0.1:0", Folder: dir}
	c.PadFiles[0] = path
	c.PadFiles[8] = path
	if err := host.host(c); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	address := host.listener.Addr().String()
	hostDir := host.padDir
	host.mu.Unlock()
	c.Name = "Guest"
	c.Address = address
	if err := guest.join(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return host.snapshot().CanRecord })
	guest.mu.Lock()
	guestDir := guest.padDir
	clip := guest.pads[8]
	guest.mu.Unlock()
	if clip.Info.Size != sampleRate/2 || padHash(clip.PCM) != clip.Info.SHA256 {
		t.Fatal("bad transfer")
	}
	if _, err := readPadPCM(guestDir, 8, clip.Info); err != nil {
		t.Fatal(err)
	}
	// Server-side gate must reject record even if a UI caller bypasses disabled button.
	host.mu.Lock()
	var remote *peer
	for p := range host.peers {
		remote = p
	}
	remote.padsReady = false
	host.mu.Unlock()
	if err := host.startRecording(); err == nil {
		t.Fatal("started before all sounds ready")
	}
	host.mu.Lock()
	remote.padsReady = true
	host.mu.Unlock()
	if err := host.startRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := guest.triggerPad(8); err != nil {
		t.Fatal(err)
	}
	var hs, gs int64
	select {
	case hs = <-hostPlayed:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not play guest request")
	}
	select {
	case gs = <-guestPlayed:
	case <-time.After(3 * time.Second):
		t.Fatal("guest did not receive playback")
	}
	if abs64(hs-gs) > int64(20*time.Millisecond) {
		t.Fatalf("playback clock mismatch %d", hs-gs)
	}
	// Wait until the whole scheduled clip lies within the recorded interval.
	time.Sleep(time.Duration(max(0, hs+400e6-clockNow())))
	if err := host.stopRecording(); err != nil {
		t.Fatal(err)
	}
	folder := host.snapshot().Folder
	b, err := os.ReadFile(filepath.Join(folder, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	sound, err := os.ReadFile(filepath.Join(folder, "soundpad.wav"))
	if err != nil {
		t.Fatal(err)
	}
	mix, err := os.ReadFile(filepath.Join(folder, "mix.wav"))
	if err != nil {
		t.Fatal(err)
	}
	track, err := os.ReadFile(filepath.Join(folder, "track-01.wav"))
	if err != nil {
		t.Fatal(err)
	}
	start := (hs - m.Start) * sampleRate / 1e9
	for i := int64(0); i < int64((len(sound)-44)/2); i++ {
		want := uint16(0)
		if i >= start && i < start+sampleRate/4 {
			want = 2000
		}
		if got := binary.LittleEndian.Uint16(sound[44+i*2:]); got != want {
			t.Fatalf("soundpad position %d: %d != %d", i, got, want)
		}
		if i >= start && i < start+sampleRate/4 {
			if binary.LittleEndian.Uint16(mix[44+i*2:]) != 3000 || binary.LittleEndian.Uint16(track[44+i*2:]) != 1000 {
				t.Fatal("soundpad not mixed separately from microphone")
			}
		}
	}
	guest.disconnect()
	host.disconnect()
	for _, path := range []string{hostDir, guestDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("session sound cache not removed")
		}
	}
	// Recording owns its sound copies; recovery works after temporary caches are gone.
	if _, err := exportSession(folder); err != nil {
		t.Fatal(err)
	}
}

func TestSoundpadValidation(t *testing.T) {
	data, err := os.ReadFile(testPadFile(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := decodePadWAV(data)
	if err != nil || len(pcm) != sampleRate/2 {
		t.Fatalf("WAV: %v", err)
	}
	if _, err := decodePadWAV(data[:len(data)-1]); err == nil {
		t.Fatal("accepted truncated WAV")
	}
	for _, bad := range [][]byte{nil, []byte("not a WAV")} {
		if _, err := decodePadWAV(bad); err == nil {
			t.Fatal("accepted bad WAV")
		}
	}
	e := newEngine(syntheticCapture)
	e.padDir = t.TempDir()
	infos := make([]padInfo, padCount)
	infos[0] = padInfo{"sound", len(pcm), padHash(pcm)}
	if err := e.acceptPadMessage(message{Type: "pads_begin", Pads: infos}); err != nil {
		t.Fatal(err)
	}
	if err := e.acceptPadMessage(message{Type: "pad_chunk", Pad: 0, Offset: 1, PCM: pcm}); err == nil {
		t.Fatal("accepted out-of-order chunk")
	}
	if err := e.acceptPadMessage(message{Type: "pad_chunk", Pad: 9, PCM: pcm}); err == nil {
		t.Fatal("accepted invalid index")
	}
	if err := e.acceptPadMessage(message{Type: "pads_end"}); err == nil || e.padsReady {
		t.Fatal("accepted incomplete sounds")
	}
	pcm[0] ^= 1
	if err := e.acceptPadMessage(message{Type: "pad_chunk", Pad: 0, PCM: pcm}); err != nil {
		t.Fatal(err)
	}
	if err := e.acceptPadMessage(message{Type: "pads_end"}); err == nil || e.padsReady {
		t.Fatal("accepted checksum mismatch")
	}
}

func TestSoundpadPlaybackSmoke(t *testing.T) {
	if os.Getenv("COOPRECORD_PLAYBACK_SMOKE") != "1" {
		t.Skip("opt-in native output test with silence")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := playPadPCM(ctx, make([]byte, sampleRate/5), clockNow()+300e6); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := playPadPCM(ctx, make([]byte, sampleRate/5), clockNow()+300e6); err != nil {
		t.Fatal(err)
	}
}

func TestPadWAVFormats(t *testing.T) {
	for _, bits := range []int{8, 16, 24, 32} {
		for _, kind := range []uint16{1, 3} {
			if kind == 3 && bits != 32 {
				continue
			}
			// Stereo, 24 kHz: normalization doubles sample count and averages channels.
			b := make([]byte, 44+20*2*bits/8)
			copy(b, "RIFF")
			binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
			copy(b[8:], "WAVEfmt ")
			binary.LittleEndian.PutUint32(b[16:], 16)
			binary.LittleEndian.PutUint16(b[20:], kind)
			binary.LittleEndian.PutUint16(b[22:], 2)
			binary.LittleEndian.PutUint32(b[24:], 24000)
			binary.LittleEndian.PutUint32(b[28:], uint32(24000*2*bits/8))
			binary.LittleEndian.PutUint16(b[32:], uint16(2*bits/8))
			binary.LittleEndian.PutUint16(b[34:], uint16(bits))
			copy(b[36:], "data")
			binary.LittleEndian.PutUint32(b[40:], uint32(len(b)-44))
			for pos := 44; pos < len(b); pos += 2 * bits / 8 {
				p := b[pos:]
				if kind == 3 {
					binary.LittleEndian.PutUint32(p, math.Float32bits(0.5))
				} else {
					switch bits {
					case 8:
						p[0] = 192
						p[1] = 128
					case 16:
						binary.LittleEndian.PutUint16(p, 16384)
					case 24:
						p[2] = 64
					case 32:
						binary.LittleEndian.PutUint32(p, 1<<30)
					}
				}
			}
			pcm, err := decodePadWAV(b)
			if err != nil || len(pcm) != 80 {
				t.Fatalf("format %d/%d: %v len=%d", kind, bits, err, len(pcm))
			}
			for pos := 0; pos < len(pcm); pos += 2 {
				if binary.LittleEndian.Uint16(pcm[pos:]) != 8192 {
					t.Fatalf("bad conversion %d/%d", kind, bits)
				}
			}
		}
	}
}
