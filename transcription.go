package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type transcriptLine struct {
	At                    int64 // host monotonic clock, used only to order late recognition results
	Time                  int64 // elapsed nanoseconds, never a wall-clock timestamp
	Name, Text, Recording string
}
type transcriptRecord struct {
	dropped    int
	start, end int64
	id, dir    string
	file       *os.File
	lines      []transcriptLine
	err        error
	pending    sync.WaitGroup
}
type speechStream struct {
	name                  string
	start, next           int64
	pcm                   []byte // 16 kHz mono, signed 16-bit
	active, voiced, quiet int
	record                *transcriptRecord
}
type speechJob struct{ stream speechStream }
type transcription struct {
	mu        sync.Mutex // segmentation only; never held during inference, file or network I/O
	ctx       context.Context
	streams   map[int]*speechStream
	jobs      chan speechJob
	record    *transcriptRecord
	origin    int64
	status    string
	history   []transcriptLine
	revision  uint64
	recognize speechRecognizer
	publish   func(transcriptLine)
}

func newTranscription(ctx context.Context, recognize speechRecognizer, publish func(transcriptLine)) *transcription {
	s := &transcription{ctx: ctx, streams: make(map[int]*speechStream), jobs: make(chan speechJob, 128), origin: clockNow(), status: "Загрузка локальной модели…", recognize: recognize, publish: publish}
	return s
}
func cleanSpeech(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\x00", "")), " ")
}
func transcriptTimestamp(t int64) string {
	ms := max(0, t/int64(time.Millisecond))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
func transcriptMarkdown(l transcriptLine) string {
	escape := strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "`", "\\`")
	return fmt.Sprintf("[%s] **%s:** %s\n", transcriptTimestamp(l.Time), escape.Replace(cleanSpeech(l.Name)), escape.Replace(cleanSpeech(l.Text)))
}
func (s *transcription) setStatus(text string) { s.mu.Lock(); s.status = text; s.mu.Unlock() }
func (s *transcription) addLine(l transcriptLine) {
	s.mu.Lock()
	s.history = append(s.history, l)
	// Keep an entire six-hour conversation at normal speaking rates, with a hard memory bound.
	if len(s.history) > 30000 {
		s.history = s.history[len(s.history)-30000:]
	}
	s.revision++
	s.mu.Unlock()
}
func (s *transcription) snapshot(after uint64) (uint64, string, []transcriptLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if len(s.jobs) > 0 {
		status += fmt.Sprintf(" · в очереди: %d", len(s.jobs))
	}
	var lines []transcriptLine
	if s.revision != after {
		lines = append(lines, s.history...)
	}
	return s.revision, status, lines
}
func (s *transcription) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%s · в очереди: %d", s.status, len(s.jobs))
}
func (s *transcription) dropSource(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.streams[id]; v != nil {
		s.flush(v)
		delete(s.streams, id)
	}
}

// Called under engine.mu, with the same host clock mapping and microphone
// correction used for the WAVs. No mixed/desktop/soundpad audio enters recognition.
func (s *transcription) feed(id int, name string, p packet) {
	if s == nil || p.Desktop || len(p.PCM) < 6 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	stream := s.streams[id]
	if stream == nil {
		stream = &speechStream{name: name}
		s.streams[id] = stream
	}
	if p.Discontinuity || (stream.next != 0 && abs64(p.Time-stream.next) > int64(50*time.Millisecond)) {
		s.flush(stream)
	}
	// A short FIR low-pass before 48 -> 16 kHz decimation; microphones deliver multiples of 3 samples.
	for i := 0; i+5 < len(p.PCM); i += 6 {
		stamp := p.Time + int64(i/2)*int64(time.Second)/sampleRate
		var record *transcriptRecord
		if r := s.record; r != nil && stamp >= r.start && (r.end == 0 || stamp < r.end) {
			record = r
		}
		if stream.record != record {
			s.flush(stream)
			stream.record = record
		}
		if len(stream.pcm) == 0 {
			stream.start = stamp
		}
		a := int32(int16(binary.LittleEndian.Uint16(p.PCM[i:])))
		b := int32(int16(binary.LittleEndian.Uint16(p.PCM[i+2:])))
		c := int32(int16(binary.LittleEndian.Uint16(p.PCM[i+4:])))
		stream.pcm = binary.LittleEndian.AppendUint16(stream.pcm, uint16(int16((a+2*b+c)/4)))
		if len(stream.pcm)%640 == 0 {
			s.segment(stream)
		} // 20 ms frames
	}
	stream.next = p.Time + int64(len(p.PCM)/2)*int64(time.Second)/sampleRate
}
func (s *transcription) segment(v *speechStream) {
	var energy float64
	for i := len(v.pcm) - 640; i < len(v.pcm); i += 2 {
		x := float64(int16(binary.LittleEndian.Uint16(v.pcm[i:])))
		energy += x * x
	}
	if math.Sqrt(energy/320) > 90 {
		v.voiced++
		v.quiet = 0
		v.active = 1
	} else {
		v.quiet++
	}
	if v.active == 0 && len(v.pcm) > 6400 {
		v.pcm = append(v.pcm[:0], v.pcm[len(v.pcm)-6400:]...)
		v.start += int64(20 * time.Millisecond)
	}
	if (v.active != 0 && v.quiet >= 35) || len(v.pcm) >= 12*32000 {
		s.flush(v)
	}
}
func (s *transcription) flush(v *speechStream) {
	if v.voiced >= 10 && s.ctx.Err() != nil {
		if v.record != nil {
			v.record.dropped++
		}
	} else if v.voiced >= 10 {
		job := speechJob{stream: *v}
		if v.record != nil {
			v.record.pending.Add(1)
		}
		select {
		case s.jobs <- job:
		default:
			if v.record != nil {
				v.record.dropped++
				v.record.pending.Done()
			}
			s.status = "Модель не успевает: часть текста пропущена. Аудио продолжает записываться."
		}
	}
	name, record := v.name, v.record
	*v = speechStream{name: name, record: record}
}
func (s *transcription) beginRecord(dir, id string, start int64) (*transcriptRecord, error) {
	f, err := os.OpenFile(filepath.Join(dir, "transcript.md"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = f.WriteString("# Расшифровка\n\nВремя от начала WAV. Автоматическое распознавание, возможны ошибки.\n\n"); err != nil {
		f.Close()
		return nil, err
	}
	r := &transcriptRecord{start: start, id: id, dir: dir, file: f}
	s.mu.Lock()
	s.record = r
	s.mu.Unlock()
	return r, nil
}
func (s *transcription) endRecord(r *transcriptRecord, end int64) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.end = end
}
func (s *transcription) finishRecord(r *transcriptRecord) error {
	if s == nil || r == nil {
		return nil
	}
	s.mu.Lock()
	for _, v := range s.streams {
		if v.record == r {
			s.flush(v)
			v.record = nil
		}
	}
	if s.record == r {
		s.record = nil
	}
	s.mu.Unlock()
	r.pending.Wait()
	closeErr := r.file.Close()
	if r.dropped > 0 {
		return fmt.Errorf("не обработано фрагментов: %d (переполнение очереди или отмена)", r.dropped)
	}
	if r.err != nil {
		return r.err
	}
	if closeErr != nil {
		return closeErr
	}
	sort.SliceStable(r.lines, func(i, j int) bool { return r.lines[i].Time < r.lines[j].Time })
	var text strings.Builder
	text.WriteString("# Расшифровка\n\nВремя от начала WAV. Автоматическое распознавание, возможны ошибки.\n\n")
	for _, line := range r.lines {
		text.WriteString(transcriptMarkdown(line))
	}
	path := filepath.Join(r.dir, "transcript.md")
	if err := os.WriteFile(path+".partial", []byte(text.String()), 0600); err != nil {
		return err
	}
	return os.Rename(path+".partial", path)
}
func (s *transcription) run() {
	var failed error
	for {
		select {
		case <-s.ctx.Done():
			for {
				select {
				case job := <-s.jobs:
					if job.stream.record != nil {
						job.stream.record.err = s.ctx.Err()
						job.stream.record.pending.Done()
					}
				default:
					return
				}
			}
		case job := <-s.jobs:
			v := job.stream
			var segments []speechSegment
			err := failed
			if err == nil {
				segments, err = s.recognize(s.ctx, v.pcm)
			}
			if err != nil {
				failed = err
				s.setStatus("Ошибка распознавания: " + err.Error())
				if v.record != nil {
					v.record.err = err
				}
			} else {
				for _, segment := range segments {
					text := cleanSpeech(segment.Text)
					if text == "" || len(text) > 8000 || segment.NoSpeech > 0.6 || math.IsNaN(segment.Start) || segment.Start < 0 || segment.Start >= float64(len(v.pcm))/32000 {
						continue
					}
					stamp := v.start + int64(segment.Start*1e9)
					origin, id := s.origin, ""
					if v.record != nil {
						origin, id = v.record.start, v.record.id
					}
					line := transcriptLine{At: stamp, Time: max(0, stamp-origin), Name: v.name, Text: text, Recording: id}
					s.addLine(line)
					if v.record != nil {
						v.record.lines = append(v.record.lines, line)
						if _, err = v.record.file.WriteString(transcriptMarkdown(line)); err == nil {
							err = v.record.file.Sync()
						}
						if err != nil {
							v.record.err = err
							s.setStatus("Не удалось сохранить текст: " + err.Error())
						}
					}
					if s.publish != nil {
						s.publish(line)
					}
				}
			}
			if v.record != nil {
				v.record.pending.Done()
			}
		}
	}
}

func (e *engine) startTranscription(ctx context.Context, c settings, s *transcription) {
	if s == nil || c.SpeechModel == "" {
		return
	}
	recognize, closeModel, err := e.speechFactory(ctx, c)
	if err != nil {
		s.setStatus("Расшифровка недоступна: " + err.Error())
		recognize = func(context.Context, []byte) ([]speechSegment, error) { return nil, err }
	} else {
		defer closeModel()
		s.setStatus("Русская речь · локальная модель готова")
	}
	s.recognize = recognize
	s.run()
}
func (e *engine) publishTranscript(ctx context.Context, line transcriptLine) {
	e.mu.Lock()
	if ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	var ps []*peer
	for _, p := range e.sortedPeers() {
		if p.ready {
			ps = append(ps, p)
		}
	}
	e.mu.Unlock()
	for _, p := range ps {
		if err := p.w.send(message{Type: "transcript", Transcript: &line}); err != nil {
			p.w.Close()
		}
	}
}
