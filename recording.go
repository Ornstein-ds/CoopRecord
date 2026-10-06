package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type trackInfo struct {
	ID           string
	Name         string
	CorrectionMS int
	Remote       bool
}
type manifest struct {
	Version    int
	Created    string
	Start, End int64
	Tracks     []trackInfo
	Warnings   []string
}
type trackWriter struct {
	audio, clocks *os.File
	lastTime      int64
	lastSync      time.Time
}

func newTrack(dir, id string) (*trackWriter, error) {
	f, err := os.OpenFile(filepath.Join(dir, id+".capture"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	c, err := os.OpenFile(filepath.Join(dir, id+".clock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &trackWriter{audio: f, clocks: c}, nil
}
func (w *trackWriter) append(p packet) error {
	if p.Time <= w.lastTime || len(p.PCM) == 0 || len(p.PCM)%2 != 0 || len(p.PCM) > sampleRate*2 {
		return fmt.Errorf("неверный порядок или размер аудиоблока")
	}
	var h [12]byte
	binary.LittleEndian.PutUint64(h[:8], uint64(p.Time))
	binary.LittleEndian.PutUint32(h[8:], uint32(len(p.PCM)))
	if _, err := w.audio.Write(h[:]); err != nil {
		return err
	}
	if _, err := w.audio.Write(p.PCM); err != nil {
		return err
	}
	w.lastTime = p.Time
	if time.Since(w.lastSync) >= time.Second {
		if err := w.audio.Sync(); err != nil {
			return err
		}
		w.lastSync = time.Now()
	}
	return nil
}
func (w *trackWriter) clock(s clockSample) error { return json.NewEncoder(w.clocks).Encode(s) }
func (w *trackWriter) close() error {
	return errors.Join(w.audio.Sync(), w.clocks.Sync(), w.audio.Close(), w.clocks.Close())
}

func saveManifest(dir string, m manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "session.json.partial")
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "session.json"))
}

func readPacket(r io.Reader) (packet, error) {
	var p packet
	var h [12]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return p, err
	}
	p.Time = int64(binary.LittleEndian.Uint64(h[:8]))
	n := binary.LittleEndian.Uint32(h[8:])
	if p.Time <= 0 || n == 0 || n%2 != 0 || n > sampleRate*2 {
		return p, fmt.Errorf("повреждён исходный аудиоблок")
	}
	p.PCM = make([]byte, n)
	_, err := io.ReadFull(r, p.PCM)
	return p, err
}

func readClocks(dir string, t trackInfo) ([]clockSample, error) {
	if !t.Remote {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, t.ID+".clock"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var samples []clockSample
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var s clockSample
		if json.Unmarshal(scanner.Bytes(), &s) != nil {
			break
		}
		if s.Remote <= 0 || s.Host <= 0 || s.RTT < 0 {
			return nil, fmt.Errorf("повреждены метки часов")
		}
		if len(samples) > 0 && s.Remote <= samples[len(samples)-1].Remote {
			return nil, fmt.Errorf("нарушен порядок меток часов")
		}
		samples = append(samples, s)
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("нет данных синхронизации для %s", t.Name)
	}
	return clockAnchors(samples), nil
}

func wavHeader(samples int64, channels uint16) ([]byte, error) {
	size := samples * int64(channels) * 2
	if samples < 0 || channels == 0 || size > math.MaxUint32-36 {
		return nil, fmt.Errorf("превышен размер WAV RIFF (4 ГБ)")
	}
	b := make([]byte, 44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(size+36))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], channels)
	binary.LittleEndian.PutUint32(b[24:], sampleRate)
	binary.LittleEndian.PutUint32(b[28:], sampleRate*uint32(channels)*2)
	binary.LittleEndian.PutUint16(b[32:], channels*2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(size))
	return b, nil
}

func writeWAV(path string, samples int64, write func(io.Writer) error) (err error) {
	header, err := wavHeader(samples, 1)
	if err != nil {
		return err
	}
	tmp := path + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err == nil {
			err = os.Rename(tmp, path)
		}
	}()
	w := bufio.NewWriterSize(f, 128*1024)
	if _, err = w.Write(header); err != nil {
		return err
	}
	if err = write(w); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

func writeSilence(w io.Writer, n int64) error {
	var zero [8192]byte
	for n > 0 {
		count := min(n, int64(len(zero)/2))
		if _, err := w.Write(zero[:count*2]); err != nil {
			return err
		}
		n -= count
	}
	return nil
}

type captureSpan struct {
	from, to, time int64
	offset, rate   float64
}

// Fit the audio clock to cumulative sample positions, never to adjacent packets.
// ponytail: one constant sample rate per uninterrupted span; use slow clock tracking
// if long recordings demonstrate nonlinear device-clock drift.
func captureTimeline(r io.Reader) ([]captureSpan, error) {
	var spans []captureSpan
	var span captureSpan
	var samples, lastTime, lastFrames int64
	var count, meanX, meanY, xx, xy float64
	finish := func() error {
		span.to = samples
		span.rate = 1
		if xx > 0 {
			span.rate = xy / xx
		}
		if span.rate < 0.98 || span.rate > 1.02 {
			return fmt.Errorf("недостоверная частота аудиочасов")
		}
		span.offset = meanY - span.rate*meanX
		spans = append(spans, span)
		return nil
	}
	for {
		p, err := readPacket(r)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if lastTime != 0 && p.Time <= lastTime {
			return nil, fmt.Errorf("неупорядоченные аудиоблоки")
		}
		// Preserve large discontinuities instead of stretching audio across missing time.
		// Capture normally stops on a WASAPI discontinuity; sub-ms QPC noise is not one.
		if count > 0 && abs64(p.Time-lastTime-lastFrames*1e9/sampleRate) > int64(50*time.Millisecond) {
			if err = finish(); err != nil {
				return nil, err
			}
			count, meanX, meanY, xx, xy = 0, 0, 0, 0, 0
		}
		if count == 0 {
			span = captureSpan{from: samples, time: p.Time}
		}
		x := float64(samples - span.from)
		y := float64(p.Time-span.time) * sampleRate / 1e9
		count++
		dx, dy := x-meanX, y-meanY
		meanX += dx / count
		meanY += dy / count
		xx += dx * (x - meanX)
		xy += dx * (y - meanY)
		lastTime, lastFrames = p.Time, int64(len(p.PCM)/2)
		samples += lastFrames
	}
	if count > 0 {
		if err := finish(); err != nil {
			return nil, err
		}
	}
	return spans, nil
}

func renderTrack(dir string, t trackInfo, m manifest, total int64) (warnings []string, err error) {
	anchors, err := readClocks(dir, t)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, t.ID+".capture"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 128*1024)
	spans, err := captureTimeline(r)
	if err != nil {
		return nil, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	r.Reset(f)
	position := func(sample int64, span captureSpan) float64 {
		stamp := span.time + int64(math.Round((span.offset+float64(sample-span.from)*span.rate)*1e9/sampleRate))
		return float64(mapClock(stamp, anchors)+int64(t.CorrectionMS)*int64(time.Millisecond)-m.Start) * sampleRate / 1e9
	}
	err = writeWAV(filepath.Join(dir, t.ID+".wav"), total, func(w io.Writer) error {
		p, e := readPacket(r)
		cursor := int64(0)
		count := 0
		spanIndex := 0
		inputSample := int64(0)
		firstAudio := int64(-1)
		lastAudio := int64(0)
		for e == nil {
			next, nextErr := readPacket(r)
			if nextErr == nil && next.Time <= p.Time {
				return fmt.Errorf("неупорядоченные аудиоблоки")
			}
			frames := len(p.PCM) / 2
			for spanIndex+1 < len(spans) && inputSample >= spans[spanIndex].to {
				spanIndex++
			}
			span := spans[spanIndex]
			start := position(inputSample, span)
			end := position(inputSample+int64(frames), span)
			if end <= start {
				return fmt.Errorf("неупорядоченные метки синхронизации")
			}
			// Ceil assigns each output sample to exactly one packet and keeps its
			// interpolation phase. Round used to clamp/repeat a sample at each seam.
			first := max(cursor, max(int64(0), int64(math.Ceil(start))))
			last := min(total, int64(math.Ceil(end)))
			if first > total {
				first = total
			}
			if first > cursor {
				if e := writeSilence(w, first-cursor); e != nil {
					return e
				}
				cursor = first
			}
			if last > first {
				if firstAudio < 0 {
					firstAudio = first
				}
				out := make([]byte, (last-first)*2)
				for i := first; i < last; i++ {
					x := max(0, (float64(i)-start)*float64(frames)/(end-start))
					a := min(int(x), frames-1)
					fraction := x - float64(a)
					v := float64(int16(binary.LittleEndian.Uint16(p.PCM[a*2:])))
					v2 := v
					if a+1 < frames {
						v2 = float64(int16(binary.LittleEndian.Uint16(p.PCM[(a+1)*2:])))
					} else if nextErr == nil && inputSample+int64(frames) < span.to {
						v2 = float64(int16(binary.LittleEndian.Uint16(next.PCM)))
					}
					value := int16(math.Round(max(-32768, min(32767, v+(v2-v)*fraction))))
					binary.LittleEndian.PutUint16(out[(i-first)*2:], uint16(value))
				}
				if _, e := w.Write(out); e != nil {
					return e
				}
				cursor = last
				lastAudio = last
			}
			count++
			inputSample += int64(frames)
			p = next
			e = nextErr
		}
		if e != io.EOF && e != io.ErrUnexpectedEOF {
			return e
		}
		if e == io.ErrUnexpectedEOF {
			warnings = append(warnings, t.Name+": неполный последний блок отброшен")
		}
		if count == 0 || firstAudio < 0 {
			warnings = append(warnings, t.Name+": аудио не получено, дорожка содержит тишину")
		}
		if firstAudio > sampleRate/10 {
			warnings = append(warnings, t.Name+": в начале дорожки есть участок тишины более 100 мс")
		}
		if total-lastAudio > sampleRate/10 {
			warnings = append(warnings, t.Name+": в конце дорожки не хватает более 100 мс аудио")
		}
		if len(spans) > 1 {
			warnings = append(warnings, fmt.Sprintf("%s: разрывы временных меток: %d", t.Name, len(spans)-1))
		}
		return writeSilence(w, total-cursor)
	})
	return warnings, err
}

func renderMix(dir string, m manifest, total int64) error {
	readers := make([]*bufio.Reader, 0, len(m.Tracks))
	buffers := make([][]byte, 0, len(m.Tracks))
	for _, t := range m.Tracks {
		f, err := os.Open(filepath.Join(dir, t.ID+".wav"))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = f.Seek(44, 0); err != nil {
			return err
		}
		readers = append(readers, bufio.NewReaderSize(f, 128*1024))
		buffers = append(buffers, make([]byte, 8192))
	}
	return writeWAV(filepath.Join(dir, "mix.wav"), total, func(w io.Writer) error {
		out := make([]byte, 8192)
		for left := total; left > 0; {
			n := int(min(left, 4096)) * 2
			for i, r := range readers {
				if _, err := io.ReadFull(r, buffers[i][:n]); err != nil {
					return err
				}
			}
			for j := 0; j < n; j += 2 {
				sum := int32(0)
				for _, b := range buffers {
					sum += int32(int16(binary.LittleEndian.Uint16(b[j:])))
				}
				binary.LittleEndian.PutUint16(out[j:], uint16(int16(sum/int32(len(readers)))))
			}
			if _, err := w.Write(out[:n]); err != nil {
				return err
			}
			left -= int64(n / 2)
		}
		return nil
	})
}

// Recovery and normal Stop use exactly the same exporter. Originals are retained.
func exportSession(dir string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Version != 1 || m.Start <= 0 || len(m.Tracks) == 0 || len(m.Tracks) > maxPeers+1 {
		return m, fmt.Errorf("неподдерживаемая сессия")
	}
	ids := map[string]bool{}
	for _, t := range m.Tracks {
		if !strings.HasPrefix(t.ID, "track-") || len(t.ID) != 8 || t.ID[6] < '0' || t.ID[6] > '9' || t.ID[7] < '0' || t.ID[7] > '9' || ids[t.ID] || t.CorrectionMS < -2000 || t.CorrectionMS > 2000 {
			return m, fmt.Errorf("неверный идентификатор дорожки")
		}
		ids[t.ID] = true
	}
	if m.End == 0 {
		for _, t := range m.Tracks {
			anchors, e := readClocks(dir, t)
			if e != nil {
				return m, e
			}
			f, e := os.Open(filepath.Join(dir, t.ID+".capture"))
			if e != nil {
				return m, e
			}
			r := bufio.NewReaderSize(f, 128*1024)
			for {
				p, e := readPacket(r)
				if e == io.EOF || e == io.ErrUnexpectedEOF {
					break
				}
				if e != nil {
					f.Close()
					return m, e
				}
				end := mapClock(p.Time, anchors) + int64(len(p.PCM)/2)*1e9/sampleRate
				m.End = max(m.End, end)
			}
			f.Close()
		}
		m.Warnings = append(m.Warnings, "Сессия восстановлена из принятых исходных блоков; конец определён по последнему блоку.")
	}
	if m.End <= m.Start || m.End-m.Start > int64(maxDuration)+int64(time.Second) {
		return m, fmt.Errorf("недопустимая длительность записи (максимум 6 часов)")
	}
	total := (m.End - m.Start) * sampleRate / 1e9
	for _, t := range m.Tracks {
		// Recompute this export diagnostic; old versions mistook timestamp noise for gaps.
		kept := m.Warnings[:0]
		for _, warning := range m.Warnings {
			if !strings.HasPrefix(warning, t.Name+": разрывы временных меток:") {
				kept = append(kept, warning)
			}
		}
		m.Warnings = kept
		warnings, e := renderTrack(dir, t, m, total)
		if e != nil {
			return m, e
		}
		m.Warnings = append(m.Warnings, warnings...)
	}
	if err = renderMix(dir, m, total); err != nil {
		return m, err
	}
	seenWarnings := map[string]bool{}
	uniqueWarnings := m.Warnings[:0]
	for _, warning := range m.Warnings {
		if !seenWarnings[warning] {
			seenWarnings[warning] = true
			uniqueWarnings = append(uniqueWarnings, warning)
		}
	}
	m.Warnings = uniqueWarnings
	return m, saveManifest(dir, m)
}
