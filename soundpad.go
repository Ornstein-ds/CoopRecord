package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const padCount = 9
const maxPadBytes = sampleRate * 2 * 120
const padChunkSize = 48 * 1024

type padInfo struct {
	Name   string
	Size   int
	SHA256 string
}
type padClip struct {
	Info padInfo
	PCM  []byte
}

// Decode ordinary PCM/float WAV, normalize once on the host to the recording format.
func decodePadWAV(b []byte) ([]byte, error) {
	bad := fmt.Errorf("нужен WAV PCM 8/16/24/32 бит или float32, моно/стерео, не длиннее 2 минут")
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, bad
	}
	var format, data []byte
	for pos := 12; pos+8 <= len(b); {
		n := int(binary.LittleEndian.Uint32(b[pos+4:]))
		if n > len(b)-pos-8 {
			return nil, bad
		}
		switch string(b[pos : pos+4]) {
		case "fmt ":
			format = b[pos+8 : pos+8+n]
		case "data":
			data = b[pos+8 : pos+8+n]
		}
		pos += 8 + n + (n & 1)
	}
	if len(format) < 16 || len(data) == 0 {
		return nil, bad
	}
	kind := binary.LittleEndian.Uint16(format)
	channels := int(binary.LittleEndian.Uint16(format[2:]))
	rate := int(binary.LittleEndian.Uint32(format[4:]))
	align := int(binary.LittleEndian.Uint16(format[12:]))
	bits := int(binary.LittleEndian.Uint16(format[14:]))
	if kind == 0xfffe && len(format) >= 40 {
		kind = binary.LittleEndian.Uint16(format[24:])
	}
	if channels < 1 || channels > 2 || rate < 8000 || rate > 192000 || (kind != 1 && kind != 3) || (bits != 8 && bits != 16 && bits != 24 && bits != 32) || (kind == 3 && bits != 32) || align != channels*bits/8 || len(data)%align != 0 {
		return nil, bad
	}
	frames := len(data) / align
	outFrames := int(int64(frames) * sampleRate / int64(rate))
	if outFrames < 1 || outFrames > maxPadBytes/2 {
		return nil, bad
	}
	value := func(frame int) float64 {
		sum := 0.0
		for ch := 0; ch < channels; ch++ {
			p := data[frame*align+ch*bits/8:]
			v := 0.0
			switch {
			case kind == 3:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(p))) * 32768
			case bits == 8:
				v = float64(int(p[0])-128) * 256
			case bits == 16:
				v = float64(int16(binary.LittleEndian.Uint16(p)))
			case bits == 24:
				v = float64(int32(uint32(p[0])<<8|uint32(p[1])<<16|uint32(p[2])<<24)) / 65536
			case bits == 32:
				v = float64(int32(binary.LittleEndian.Uint32(p))) / 65536
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			sum += v
		}
		return sum / float64(channels)
	}
	pcm := make([]byte, outFrames*2)
	for i := 0; i < outFrames; i++ {
		x := float64(i) * float64(rate) / sampleRate
		a := min(int(x), frames-1)
		v := value(a) + (value(min(a+1, frames-1))-value(a))*(x-float64(a))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(math.Round(max(-32768, min(32767, v))))))
	}
	return pcm, nil
}

func padPath(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf("pad-%02d.pcm", index+1))
}
func padHash(pcm []byte) string { h := sha256.Sum256(pcm); return hex.EncodeToString(h[:]) }

func (e *engine) preparePads(paths [padCount]string) error {
	var pads [padCount]padClip
	total := int64(0)
	for i, path := range paths {
		if path == "" {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("пад %d: %w", i+1, err)
		}
		b, err := io.ReadAll(io.LimitReader(f, 64<<20+1))
		f.Close()
		if err != nil {
			return err
		}
		if len(b) > 64<<20 {
			return fmt.Errorf("пад %d: файл больше 64 МБ", i+1)
		}
		pcm, err := decodePadWAV(b)
		if err != nil {
			return fmt.Errorf("пад %d: %w", i+1, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if len([]rune(name)) > 40 {
			name = string([]rune(name)[:40])
		}
		pads[i] = padClip{padInfo{name, len(pcm), padHash(pcm)}, pcm}
		total += int64(len(pcm))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, p := range pads {
		if len(p.PCM) > 0 {
			if err := os.WriteFile(padPath(e.padDir, i), p.PCM, 0600); err != nil {
				return err
			}
		}
	}
	e.pads = pads
	e.padTotal = total
	e.padBytes = total
	e.padsReady = true
	return nil
}

func (e *engine) sendPads(ctx context.Context, p *peer) {
	e.mu.Lock()
	var infos [padCount]padInfo
	clips := e.pads
	for i := range clips {
		infos[i] = clips[i].Info
	}
	e.mu.Unlock()
	err := p.w.send(message{Type: "pads_begin", Pads: infos[:]})
	for i, clip := range clips {
		for offset := 0; offset < len(clip.PCM) && err == nil; offset += padChunkSize {
			if ctx.Err() != nil {
				return
			}
			end := min(offset+padChunkSize, len(clip.PCM))
			err = p.w.send(message{Type: "pad_chunk", Pad: i, Offset: int64(offset), PCM: clip.PCM[offset:end]})
		}
	}
	if err == nil {
		err = p.w.send(message{Type: "pads_end"})
	}
	if err != nil {
		p.w.Close()
	}
}

func (e *engine) receivePads(ctx context.Context, w *wire, m message) error {
	e.mu.Lock()
	if ctx.Err() != nil {
		e.mu.Unlock()
		return ctx.Err()
	}
	err := e.acceptPadMessage(m)
	count := e.padBytes
	ready := e.padsReady
	e.mu.Unlock()
	if err != nil {
		return err
	}
	typ := "pads_progress"
	if ready {
		typ = "pads_ready"
	}
	return w.send(message{Type: typ, Offset: count})
}

// Called with the engine mutex held. Names from the network are never filesystem paths.
func (e *engine) acceptPadMessage(m message) error {
	switch m.Type {
	case "pads_begin":
		if e.padReceiving || e.padsReady || len(m.Pads) != padCount {
			return fmt.Errorf("неверный список саундпада")
		}
		e.padTotal = 0
		e.padBytes = 0
		for i, p := range m.Pads {
			if p.Size < 0 || p.Size > maxPadBytes || p.Size%2 != 0 || len(p.Name) > 200 || strings.ContainsAny(p.Name, "\x00\r\n") {
				return fmt.Errorf("неверный звук саундпада")
			}
			if p.Size > 0 {
				h, err := hex.DecodeString(p.SHA256)
				if err != nil || len(h) != sha256.Size {
					return fmt.Errorf("неверная сумма звука")
				}
			}
			e.pads[i] = padClip{Info: p}
			e.padTotal += int64(p.Size)
		}
		e.padReceiving = true
	case "pad_chunk":
		if !e.padReceiving || m.Pad < 0 || m.Pad >= padCount || len(m.PCM) == 0 || len(m.PCM) > padChunkSize {
			return fmt.Errorf("неверный блок саундпада")
		}
		p := &e.pads[m.Pad]
		if m.Offset != int64(len(p.PCM)) || len(p.PCM)+len(m.PCM) > p.Info.Size {
			return fmt.Errorf("нарушен порядок загрузки звука")
		}
		p.PCM = append(p.PCM, m.PCM...)
		f, err := os.OpenFile(padPath(e.padDir, m.Pad), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(m.PCM)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		e.padBytes += int64(len(m.PCM))
	case "pads_end":
		if !e.padReceiving {
			return fmt.Errorf("загрузка саундпада не начата")
		}
		for i, p := range e.pads {
			if len(p.PCM) != p.Info.Size || (p.Info.Size > 0 && padHash(p.PCM) != p.Info.SHA256) {
				return fmt.Errorf("звук загружен не полностью или повреждён")
			}
			if p.Info.Size > 0 {
				if _, err := readPadPCM(e.padDir, i, p.Info); err != nil {
					return err
				}
			}
		}
		e.padsReady = true
		e.padReceiving = false
	default:
		return fmt.Errorf("неизвестная команда саундпада")
	}
	return nil
}

func (e *engine) padsReadyLocked() bool {
	if !e.padsReady {
		return false
	}
	for p := range e.peers {
		if !p.padsReady || !p.ready || len(p.samples) < 8 {
			return false
		}
	}
	return true
}

func (e *engine) triggerPad(index int) error {
	e.padTriggerMu.Lock()
	defer e.padTriggerMu.Unlock()
	e.mu.Lock()
	if index < 0 || index >= padCount || !e.padsReady || len(e.pads[index].PCM) == 0 || e.ctx == nil || e.ctx.Err() != nil {
		e.mu.Unlock()
		return fmt.Errorf("пад пока не готов")
	}
	if e.mode == "guest" {
		w := e.client
		e.mu.Unlock()
		return w.send(message{Type: "pad_request", Pad: index})
	}
	if e.mode != "host" || !e.padsReadyLocked() || e.exporting {
		e.mu.Unlock()
		return fmt.Errorf("дождитесь готовности саундпада у всех участников")
	}
	if e.padVoices >= 32 {
		e.mu.Unlock()
		return fmt.Errorf("уже звучат 32 пада; дождитесь их завершения")
	}
	now := clockNow()
	lead := int64(500 * time.Millisecond)
	peers := e.sortedPeers()
	for _, p := range peers {
		lead = max(lead, 2*p.samples[len(p.samples)-1].RTT+200e6)
	}
	start := now + lead
	if err := e.logPadLocked(index, start); err != nil {
		e.mu.Unlock()
		return err
	}
	starts := make([]int64, len(peers))
	for i, p := range peers {
		anchors := clockAnchors(p.samples)
		s := anchors[len(anchors)-1]
		starts[i] = start + s.Remote - s.Host
	}
	ctx, pcm := e.ctx, e.pads[index].PCM
	e.mu.Unlock()
	for i, p := range peers {
		if err := p.w.send(message{Type: "pad_play", Pad: index, Time: starts[i]}); err != nil {
			p.w.Close()
			return err
		}
	}
	e.schedulePad(ctx, pcm, start)
	return nil
}

func (e *engine) schedulePad(ctx context.Context, pcm []byte, start int64) {
	e.mu.Lock()
	if e.padVoices >= 32 {
		e.lastError = "Слишком много одновременно звучащих падов"
		e.mu.Unlock()
		return
	}
	e.padVoices++
	play := e.playPad
	e.mu.Unlock()
	go func() {
		err := play(ctx, pcm, start)
		e.mu.Lock()
		defer e.mu.Unlock()
		e.padVoices--
		if err != nil && ctx.Err() == nil {
			e.lastError = "Саундпад: " + err.Error()
		}
	}()
}

// Used by export too; validates cached audio instead of trusting a mutable source path.
func readPadPCM(dir string, index int, info padInfo) ([]byte, error) {
	if index < 0 || index >= padCount || info.Size <= 0 || info.Size > maxPadBytes || info.Size%2 != 0 {
		return nil, fmt.Errorf("неверные данные саундпада")
	}
	f, err := os.Open(padPath(dir, index))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(info.Size)+1))
	if err != nil {
		return nil, err
	}
	if len(b) != info.Size || padHash(b) != info.SHA256 {
		return nil, fmt.Errorf("повреждён файл саундпада %d", index+1)
	}
	return b, nil
}
