package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type padEvent struct {
	Pad  int
	Time int64
}

func (e *engine) preparePadRecording(r *recording) error {
	if e.padTotal == 0 {
		return nil
	}
	r.m.Pads = make([]padInfo, padCount)
	for i, p := range e.pads {
		r.m.Pads[i] = p.Info
		if len(p.PCM) > 0 {
			if err := os.WriteFile(padPath(r.dir, i), p.PCM, 0600); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "soundpad.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	r.padEvents = f
	return err
}

func (e *engine) logPadLocked(index int, start int64) error {
	r := e.rec
	if r == nil || r.stopping || r.padEvents == nil {
		return nil
	}
	if r.padEventCount >= 20000 {
		return fmt.Errorf("достигнут лимит 20000 запусков саундпада в одной записи")
	}
	if err := json.NewEncoder(r.padEvents).Encode(padEvent{index, start}); err != nil {
		return err
	}
	if err := r.padEvents.Sync(); err != nil {
		return err
	}
	r.padEventCount++
	return nil
}

func renderSoundpad(dir string, m manifest, total int64) error {
	if len(m.Pads) == 0 {
		return nil
	}
	if len(m.Pads) != padCount {
		return fmt.Errorf("неверный список звуков в записи")
	}
	var clips [padCount][]byte
	for i, p := range m.Pads {
		if p.Size == 0 {
			continue
		}
		b, err := readPadPCM(dir, i, p)
		if err != nil {
			return err
		}
		clips[i] = b
	}
	f, err := os.Open(filepath.Join(dir, "soundpad.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 4<<20))
	var events []padEvent
	for {
		var event padEvent
		err := decoder.Decode(&event)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		if len(events) >= 20000 || event.Pad < 0 || event.Pad >= padCount || len(clips[event.Pad]) == 0 || event.Time <= 0 || event.Time < m.Start-120e9 || event.Time > m.End+10e9 {
			return fmt.Errorf("повреждён журнал саундпада")
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time < events[j].Time })
	return writeWAV(filepath.Join(dir, "soundpad.wav"), total, func(w io.Writer) error {
		next := 0
		var active []padEvent
		for cursor := int64(0); cursor < total; {
			count := min(int64(4096), total-cursor)
			for next < len(events) && (events[next].Time-m.Start)*sampleRate/1e9 < cursor+count {
				active = append(active, events[next])
				next++
			}
			sums := make([]int64, count)
			keep := active[:0]
			for _, event := range active {
				start := (event.Time - m.Start) * sampleRate / 1e9
				pcm := clips[event.Pad]
				end := start + int64(len(pcm)/2)
				for pos := max(cursor, start); pos < min(cursor+count, end); pos++ {
					sums[pos-cursor] += int64(int16(binary.LittleEndian.Uint16(pcm[(pos-start)*2:])))
				}
				if end > cursor+count {
					keep = append(keep, event)
				}
			}
			active = keep
			out := make([]byte, count*2)
			for i, v := range sums {
				binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(max(-32768, min(32767, v)))))
			}
			if _, err := w.Write(out); err != nil {
				return err
			}
			cursor += count
		}
		return nil
	})
}
