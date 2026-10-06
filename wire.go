package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const sampleRate = 48000
const maxDuration = 6 * time.Hour
const maxPeers = 7
const defaultPort = "47652"
const protocolVersion = 5

type packet struct {
	Time          int64
	PCM           []byte
	Discontinuity bool
}
type message struct {
	Source        uint32    `json:"source,omitempty"`
	VoiceToken    string    `json:"voice_token,omitempty"`
	Pads          []padInfo `json:"pads,omitempty"`
	Pad           int       `json:"pad,omitempty"`
	Offset        int64     `json:"offset,omitempty"`
	Type          string    `json:"type"`
	Version       int       `json:"version,omitempty"`
	Name          string    `json:"name,omitempty"`
	Key           string    `json:"key,omitempty"`
	Session       string    `json:"session,omitempty"`
	T0            int64     `json:"t0,omitempty"`
	T1            int64     `json:"t1,omitempty"`
	T2            int64     `json:"t2,omitempty"`
	Time          int64     `json:"time,omitempty"`
	PCM           []byte    `json:"pcm,omitempty"`
	Discontinuity bool      `json:"discontinuity,omitempty"`
	Error         string    `json:"error,omitempty"`
	CorrectionMS  int       `json:"correction_ms,omitempty"`
}

type wire struct {
	net.Conn
	mu sync.Mutex
}

func (w *wire) send(m message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(b)))
	// net.Buffers handles short writes; the mutex covers both header and payload.
	buffers := net.Buffers{size[:], b}
	_, err = buffers.WriteTo(w.Conn)
	return err
}

func (w *wire) receive() (message, error) {
	var m message
	if err := w.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return m, err
	}
	var size [4]byte
	if _, err := io.ReadFull(w.Conn, size[:]); err != nil {
		return m, err
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n == 0 || n > 256*1024 {
		return m, fmt.Errorf("недопустимый размер сетевого сообщения: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(w.Conn, b); err != nil {
		return m, err
	}
	err := json.Unmarshal(b, &m)
	return m, err
}

type clockSample struct{ Remote, Host, RTT int64 }

func sampleClock(t0, t1, t2, t3 int64) (clockSample, error) {
	rtt := (t3 - t0) - (t2 - t1)
	if t0 <= 0 || t1 <= 0 || t2 < t1 || t3 < t0 || rtt < 0 || rtt > int64(2*time.Second) {
		return clockSample{}, fmt.Errorf("недостоверный ответ синхронизации")
	}
	return clockSample{Remote: t1 + (t2-t1)/2, Host: t0 + (t3-t0)/2, RTT: rtt}, nil
}

// Pick the lowest RTT in each 20-second window, then interpolate clock drift.
// ponytail: symmetric network latency is assumed; calibrate the residual offset in the UI.
func clockAnchors(samples []clockSample) []clockSample {
	var out []clockSample
	for _, s := range samples {
		if len(out) == 0 || s.Remote-out[len(out)-1].Remote >= int64(20*time.Second) {
			out = append(out, s)
		} else if s.RTT < out[len(out)-1].RTT {
			out[len(out)-1] = s
		}
	}
	return out
}

func mapClock(t int64, anchors []clockSample) int64 {
	if len(anchors) == 0 {
		return t
	}
	a := anchors[0]
	for _, b := range anchors[1:] {
		if t <= b.Remote {
			if t < a.Remote {
				return t + a.Host - a.Remote
			}
			ratio := float64(t-a.Remote) / float64(b.Remote-a.Remote)
			return t + (a.Host - a.Remote) + int64(ratio*float64((b.Host-b.Remote)-(a.Host-a.Remote)))
		}
		a = b
	}
	return t + a.Host - a.Remote
}
