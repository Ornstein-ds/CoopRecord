package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

type mixerChannel struct {
	volume atomic.Int32
	peak   atomic.Int32
}

func newMixerChannel() *mixerChannel {
	c := &mixerChannel{}
	c.volume.Store(100)
	return c
}

// Keep the peak until the UI consumes it so short sounds are visible between refreshes.
func (c *mixerChannel) meter(peak int32) {
	for old := c.peak.Load(); peak > old; old = c.peak.Load() {
		if c.peak.CompareAndSwap(old, peak) {
			break
		}
	}
}

func pcmPeak(pcm []byte) int32 {
	var peak int32
	for i := 0; i+1 < len(pcm); i += 2 {
		n := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if n < 0 {
			n = -n
		}
		peak = max(peak, n)
	}
	return min(100, peak*100/32768)
}

type rosterMember struct {
	Source uint32 `json:"source"`
	Name   string `json:"name"`
}

type mixerStrip struct {
	Name    string
	Channel *mixerChannel
}

func (v *voiceSession) setRoster(members []rosterMember) error {
	if len(members) == 0 || len(members) > maxPeers+1 {
		return fmt.Errorf("неверный список участников")
	}
	seen := make(map[uint32]bool)
	for _, m := range members {
		if seen[m.Source] || strings.TrimSpace(m.Name) == "" || utf8.RuneCountInString(m.Name) > 40 || strings.ContainsAny(m.Name, "\x00\r\n") {
			return fmt.Errorf("неверный участник микшера")
		}
		seen[m.Source] = true
	}
	if !seen[0] || !seen[v.source] {
		return fmt.Errorf("неполный список участников")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	channels := make(map[uint32]*mixerChannel)
	for _, m := range members {
		if m.Source == v.source {
			continue
		}
		c := v.channels[m.Source]
		if c == nil {
			c = newMixerChannel()
		}
		channels[m.Source] = c
	}
	v.members = append([]rosterMember(nil), members...)
	v.channels = channels
	for id := range v.streams {
		if !seen[id] {
			delete(v.streams, id)
		}
	}
	return nil
}

func (e *engine) mixerStrips() []mixerStrip {
	e.mu.Lock()
	v := e.voice
	e.mu.Unlock()
	strips := []mixerStrip{{"Саундпад", e.padMixer}}
	if v == nil || v.ctx.Err() != nil {
		return strips
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, m := range v.members {
		if m.Source != v.source {
			strips = append(strips, mixerStrip{m.Name, v.channels[m.Source]})
		}
	}
	return strips
}

// Serialize roster snapshots and sends so simultaneous joins/leaves cannot publish an older list last.
func (e *engine) broadcastRoster(ctx context.Context) {
	e.rosterMu.Lock()
	defer e.rosterMu.Unlock()
	e.mu.Lock()
	if ctx.Err() != nil || ctx != e.ctx || e.voice == nil {
		e.mu.Unlock()
		return
	}
	v := e.voice
	members := []rosterMember{{0, e.cfg.Name}}
	var targets []*wire
	for _, p := range e.sortedPeers() {
		if p.ready {
			members = append(members, rosterMember{uint32(p.number), p.name})
			targets = append(targets, p.w)
		}
	}
	e.mu.Unlock()
	_ = v.setRoster(members)
	for _, w := range targets {
		if w.send(message{Type: "roster", Members: members}) != nil {
			w.Close()
		}
	}
}
