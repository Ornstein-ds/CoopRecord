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
	"sync/atomic"
	"time"
)

type transcriptLine struct {
	ID                    uint64
	Source                int
	At                    int64 // host monotonic clock, used only to order late recognition results
	EndAt                 int64 // same clock as At; shared by every timeline column
	Time                  int64 // elapsed nanoseconds, never a wall-clock timestamp
	Name, Text, Recording string
	Partial               bool
}
type transcriptRecord struct {
	mu         sync.Mutex // independent channel workers append to the same recording
	dropped    int
	start, end int64
	id, dir    string
	file       *os.File
	lines      []transcriptLine
	err        error
	pending    sync.WaitGroup
}
type speechStream struct {
	source                int
	serial                uint64
	previewSamples        int
	worker                *speechWorker
	name                  string
	start, next           int64
	pcm                   []byte // 16 kHz mono, signed 16-bit
	active, voiced, quiet int
	record                *transcriptRecord
}
type speechJob struct {
	stream  speechStream
	partial bool
}
type speechWorker struct {
	jobs           chan speechJob
	previews       chan speechJob
	previewPending atomic.Bool
	previewID      atomic.Uint64
	abandoned      map[uint64]speechJob // protected by transcription.mu
	status         string               // protected by transcription.mu
}
type transcription struct {
	mu        sync.Mutex // segmentation only; never held during inference, file or network I/O
	ctx       context.Context
	streams   map[int]*speechStream
	ready     chan struct{}
	workers   map[*speechWorker]bool
	workerWG  sync.WaitGroup
	nextID    uint64
	factory   func(context.Context) (speechRecognizer, func(), error)
	record    *transcriptRecord
	origin    int64
	status    string
	history   []transcriptLine
	revision  uint64
	recognize speechRecognizer
	publish   func(transcriptLine)
}

func newTranscription(ctx context.Context, recognize speechRecognizer, publish func(transcriptLine)) *transcription {
	s := &transcription{ctx: ctx, streams: make(map[int]*speechStream), workers: make(map[*speechWorker]bool), ready: make(chan struct{}), origin: clockNow(), status: "Локальное распознавание · ожидание речи", recognize: recognize, publish: publish}
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
	defer s.mu.Unlock()
	if l.ID != 0 {
		for i := len(s.history) - 1; i >= 0; i-- {
			if s.history[i].ID == l.ID {
				// A late preview must never replace an already finalized utterance.
				if !s.history[i].Partial && l.Partial {
					return
				}
				if l.Text == "" {
					s.history = append(s.history[:i], s.history[i+1:]...)
				} else {
					s.history[i] = l
				}
				s.revision++
				return
			}
		}
	}
	if l.Text == "" {
		return
	}
	s.history = append(s.history, l)
	// Keep an entire six-hour conversation at normal speaking rates, with a hard memory bound.
	if len(s.history) > 30000 {
		s.history = s.history[len(s.history)-30000:]
	}
	s.revision++
}
func (s *transcription) queueSize() int {
	n := 0
	for w := range s.workers {
		n += len(w.jobs) + len(w.previews)
	}
	return n
}
func (s *transcription) statusText() string {
	status := s.status
	var statuses []string
	for w := range s.workers {
		if w.status != "" {
			statuses = append(statuses, w.status)
		}
	}
	sort.Strings(statuses)
	for _, text := range statuses {
		status += " · " + text
	}
	if n := s.queueSize(); n > 0 {
		status += fmt.Sprintf(" · в очереди: %d", n)
	}
	return status
}
func (s *transcription) snapshot(after uint64) (uint64, string, []transcriptLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statusText()
	var lines []transcriptLine
	if s.revision != after {
		lines = append(lines, s.history...)
	}
	return s.revision, status, lines
}
func (s *transcription) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusText()
}
func (s *transcription) dropSource(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.streams[id]; v != nil {
		s.flush(v)
		close(v.worker.jobs)
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
		w := &speechWorker{jobs: make(chan speechJob, 32), previews: make(chan speechJob, 1)}
		stream = &speechStream{name: name, source: id, worker: w}
		s.streams[id] = stream
		s.workers[w] = true
		s.workerWG.Add(1)
		go s.runWorker(w, name)
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
			s.nextID++
			stream.serial = s.nextID
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
	} else if v.voiced >= 10 && len(v.pcm)/2-v.previewSamples >= 2*16000 {
		// At most one preview is outstanding per channel. Finals have their own queue.
		if v.worker.previewPending.CompareAndSwap(false, true) {
			v.worker.previewID.Store(v.serial)
			copyStream := *v
			copyStream.pcm = append([]byte(nil), v.pcm...)
			v.worker.previews <- speechJob{stream: copyStream, partial: true}
		}
		v.previewSamples = len(v.pcm) / 2
	}
}
func (s *transcription) flush(v *speechStream) {
	if v.voiced >= 10 && s.ctx.Err() != nil {
		if v.record != nil {
			v.record.mu.Lock()
			v.record.dropped++
			v.record.mu.Unlock()
		}
	} else if v.voiced >= 10 {
		job := speechJob{stream: *v}
		if v.record != nil {
			v.record.pending.Add(1)
		}
		select {
		case v.worker.jobs <- job:
		default:
			// Retire an already displayed/in-flight preview even if its final
			// cannot fit. The worker publishes the tombstone outside feed's lock.
			if v.worker.previewID.Load() == v.serial {
				if v.worker.abandoned == nil {
					v.worker.abandoned = make(map[uint64]speechJob)
				}
				v.worker.abandoned[v.serial] = job
			}
			if v.record != nil {
				v.record.mu.Lock()
				v.record.dropped++
				v.record.mu.Unlock()
				v.record.pending.Done()
			}
			s.status = "Модель не успевает: часть текста пропущена. Аудио продолжает записываться."
		}
	}
	name, record, source, worker := v.name, v.record, v.source, v.worker
	*v = speechStream{name: name, record: record, source: source, worker: worker}
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
	r.mu.Lock()
	defer r.mu.Unlock()
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

// run opens the startup barrier and joins every channel before shutdown returns.
func (s *transcription) run() {
	close(s.ready)
	<-s.ctx.Done()
	s.mu.Lock() // feed cannot add workers after cancellation passes this barrier
	s.mu.Unlock()
	s.workerWG.Wait()
}

func (s *transcription) workerStatus(w *speechWorker, text string) {
	s.mu.Lock()
	w.status = text
	s.mu.Unlock()
}

func (s *transcription) runWorker(w *speechWorker, name string) {
	defer func() {
		s.mu.Lock()
		delete(s.workers, w)
		s.mu.Unlock()
		s.workerWG.Done()
	}()
	select {
	case <-s.ready:
	case <-s.ctx.Done():
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	recognize := s.recognize
	var closeModel func()
	defer func() {
		if closeModel != nil {
			closeModel()
		}
	}()
	var finalized uint64
	for {
		s.mu.Lock()
		abandoned := w.abandoned
		w.abandoned = nil
		s.mu.Unlock()
		for id, lost := range abandoned {
			s.completeLine(lost, nil)
			finalized = max(finalized, id)
		}
		// Prefer final jobs so preview traffic cannot delay recording finalization.
		var job speechJob
		var ok bool
		select {
		case job, ok = <-w.jobs:
		default:
			select {
			case job, ok = <-w.jobs:
			case job = <-w.previews:
				ok = true
			case <-ctx.Done():
				// feed has stopped; drain finals to release all recording waiters.
				s.mu.Lock()
				for {
					select {
					case pending, open := <-w.jobs:
						if !open {
							s.mu.Unlock()
							return
						}
						s.failJob(pending, ctx.Err())
					default:
						s.mu.Unlock()
						return
					}
				}
			}
		}
		if !ok {
			return
		}
		if job.partial && job.stream.serial <= finalized {
			w.previewPending.Store(false)
			continue
		}
		var err error
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if recognize == nil {
			s.workerStatus(w, name+": загрузка модели…")
			recognize, closeModel, err = s.factory(ctx)
		}
		var segments []speechSegment
		if err == nil {
			segments, err = recognize(ctx, job.stream.pcm)
		}
		if err != nil {
			s.workerStatus(w, name+": "+err.Error())
			// A request failure affects this utterance only. The next one can recover.
			if !job.partial {
				s.completeLine(job, nil)
				s.failJob(job, err)
			}
			if closeModel != nil && s.factory != nil {
				closeModel()
				closeModel, recognize = nil, nil
			}
		} else {
			s.workerStatus(w, "")
			s.completeLine(job, segments)
			if !job.partial && job.stream.record != nil {
				job.stream.record.pending.Done()
			}
		}
		if job.partial {
			w.previewPending.Store(false)
		} else {
			finalized = max(finalized, job.stream.serial)
		}
	}
}

// failJob does not acquire transcription.mu, so cancellation can drain under it.
func (s *transcription) failJob(job speechJob, err error) {
	if r := job.stream.record; r != nil && !job.partial {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		r.pending.Done()
	}
}

func (s *transcription) completeLine(job speechJob, segments []speechSegment) {
	v := job.stream
	duration := float64(len(v.pcm)) / 32000
	var words []string
	start, end := duration, 0.0
	for _, segment := range segments {
		text := cleanSpeech(segment.Text)
		if text == "" || len(text) > 8000 || segment.NoSpeech > 0.6 || math.IsNaN(segment.Start) || math.IsInf(segment.Start, 0) || segment.Start < 0 || segment.Start >= duration {
			continue
		}
		words = append(words, text)
		start = min(start, segment.Start)
		if !math.IsNaN(segment.End) && !math.IsInf(segment.End, 0) {
			end = max(end, min(duration, segment.End))
		}
	}
	text := strings.Join(words, " ")
	if len(text) > 8000 {
		text = ""
	}
	if job.partial && text == "" {
		return
	}
	if start == duration {
		start = 0
	}
	if end <= start || job.partial {
		end = duration
	}
	origin, recording := s.origin, ""
	if v.record != nil {
		origin, recording = v.record.start, v.record.id
	}
	line := transcriptLine{ID: v.serial, Source: v.source, At: v.start + int64(start*1e9), EndAt: v.start + int64(end*1e9), Time: max(0, v.start+int64(start*1e9)-origin), Name: v.name, Text: text, Recording: recording, Partial: job.partial}
	s.addLine(line)
	if r := v.record; r != nil && !job.partial && text != "" {
		r.mu.Lock()
		r.lines = append(r.lines, line)
		_, err := r.file.WriteString(transcriptMarkdown(line))
		if err == nil {
			err = r.file.Sync()
		}
		if err != nil {
			r.err = err
		}
		r.mu.Unlock()
		if err != nil {
			s.setStatus("Не удалось сохранить текст: " + err.Error())
		}
	}
	if s.publish != nil {
		s.publish(line)
	}
}

func (e *engine) startTranscription(ctx context.Context, c settings, s *transcription) {
	if s == nil || c.SpeechModel == "" {
		return
	}
	// Each source owns its own server: upstream serializes requests to one server.
	s.factory = func(workerCtx context.Context) (speechRecognizer, func(), error) {
		return e.speechFactory(workerCtx, c)
	}
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
