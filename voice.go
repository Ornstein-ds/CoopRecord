package main

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

const voiceFrames = sampleRate / 100 // 10 ms; the whole UDP datagram fits below a 1280-byte MTU.
const voiceHeader = 48
const voicePacketSize = voiceHeader + voiceFrames*2
const voiceSlots = 32
const desktopSource uint32 = 1 << 31

type voicePeer struct {
	token [32]byte
	ip    net.IP
	addr  *net.UDPAddr
	last  time.Time
}

type voiceSlot struct {
	seq uint64
	pcm [voiceFrames * 2]byte
}

type voiceStream struct {
	slots       [voiceSlots]voiceSlot
	latest      uint64
	cursor      float64
	started     bool
	played      bool
	first, last time.Time
}

// Voice owns its socket, queues, and lock: neither disk I/O nor pad transfers can block capture/playout.
type voiceSession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	conn        *net.UDPConn
	server      *net.UDPAddr // nil on the host
	source      uint32
	token       [32]byte
	mic         chan packet
	mu          sync.Mutex
	peers       map[uint32]*voicePeer
	streams     map[uint32]*voiceStream
	members     []rosterMember
	channels    map[uint32]*mixerChannel
	lastReceive time.Time
	problem     string
}

func newVoiceSession(ctx context.Context, conn *net.UDPConn, server *net.UDPAddr, source uint32, token [32]byte) *voiceSession {
	ctx, cancel := context.WithCancel(ctx)
	return &voiceSession{ctx: ctx, cancel: cancel, conn: conn, server: server, source: source, token: token, mic: make(chan packet, 8), peers: make(map[uint32]*voicePeer), streams: make(map[uint32]*voiceStream)}
}

func (v *voiceSession) close() { v.cancel(); v.conn.Close() }

func (v *voiceSession) status() string {
	if v == nil {
		return "Голосовая связь включится после подключения к сессии."
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.problem != "" {
		return "Голос: " + v.problem
	}
	if v.ctx.Err() != nil {
		return "Голосовая связь отключена."
	}
	if v.server != nil {
		if time.Since(v.lastReceive) < 3*time.Second {
			return "Голос: приём работает. Микрофон передаётся участникам."
		}
		return "Голос: ожидаем UDP от хоста. Если звука нет, проверьте VPN и UDP-порт сессии."
	}
	count := 0
	for _, p := range v.peers {
		if time.Since(p.last) < 3*time.Second {
			count++
		}
	}
	if len(v.peers) == 0 {
		return "Голос включён. Ожидаем участников; используйте наушники."
	}
	return fmt.Sprintf("Голос: приём от %d из %d участников. Если звука нет, проверьте UDP-порт сессии.", count, len(v.peers))
}

func (v *voiceSession) setError(err error) {
	if err == nil || v.ctx.Err() != nil {
		return
	}
	v.mu.Lock()
	v.problem = err.Error()
	v.mu.Unlock()
}

func (v *voiceSession) enqueue(p packet) {
	if v == nil || v.ctx.Err() != nil {
		return
	}
	select {
	case v.mic <- p:
		return
	default:
	}
	// Live conversation drops stale audio; the independent recording queue never does.
	select {
	case <-v.mic:
	default:
	}
	p.Discontinuity = true
	select {
	case v.mic <- p:
	default:
	}
}

func (v *voiceSession) sendMicrophone() {
	var pending [2][]byte
	var seq [2]uint64
	for {
		select {
		case <-v.ctx.Done():
			return
		case p := <-v.mic:
			kind, source := 0, v.source
			if p.Desktop {
				kind, source = 1, source|desktopSource
			}
			if p.Discontinuity {
				pending[kind] = nil
				seq[kind]++
			}
			pending[kind] = append(pending[kind], p.PCM...)
			for len(pending[kind]) >= voiceFrames*2 {
				seq[kind]++
				v.send(source, seq[kind], pending[kind][:voiceFrames*2])
				pending[kind] = pending[kind][voiceFrames*2:]
			}
		}
	}
}

func (v *voiceSession) send(source uint32, seq uint64, pcm []byte) {
	b := make([]byte, voicePacketSize)
	copy(b, "CRV7")
	binary.LittleEndian.PutUint32(b[4:], source)
	binary.LittleEndian.PutUint64(b[8:], seq)
	copy(b[voiceHeader:], pcm)
	if v.server != nil {
		copy(b[16:], v.token[:])
		_, err := v.conn.WriteToUDP(b, v.server)
		v.setError(err)
		return
	}
	// Copy destinations before sending; never hold the mixer lock during socket I/O.
	v.mu.Lock()
	var targets []voicePeer
	for id, p := range v.peers {
		if id != source&^desktopSource && p.addr != nil {
			targets = append(targets, *p)
		}
	}
	v.mu.Unlock()
	for _, p := range targets {
		copy(b[16:], p.token[:])
		_, err := v.conn.WriteToUDP(b, p.addr)
		v.setError(err)
	}
}

func (v *voiceSession) receive() {
	b := make([]byte, voicePacketSize+1)
	for {
		n, addr, err := v.conn.ReadFromUDP(b)
		if err != nil {
			if ignoreVoiceReadError(err) {
				continue
			}
			v.setError(err)
			return
		}
		if n != voicePacketSize || string(b[:4]) != "CRV7" {
			continue
		}
		source, seq := binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint64(b[8:])
		if source&^desktopSource == v.source || seq == 0 || seq >= 1<<40 {
			continue
		}
		now := time.Now()
		v.mu.Lock()
		valid := false
		if v.server == nil {
			p := v.peers[source&^desktopSource]
			if p != nil && p.ip.Equal(addr.IP) && subtle.ConstantTimeCompare(b[16:48], p.token[:]) == 1 {
				p.addr, p.last = addr, now
				valid = true
			}
		} else {
			valid = addr.IP.Equal(v.server.IP) && addr.Port == v.server.Port && subtle.ConstantTimeCompare(b[16:48], v.token[:]) == 1
		}
		if valid {
			valid = v.pushLocked(source, seq, b[voiceHeader:voicePacketSize], now)
			v.lastReceive = now
		}
		v.mu.Unlock()
		if valid && v.server == nil {
			v.send(source, seq, b[voiceHeader:voicePacketSize])
		}
	}
}

func (v *voiceSession) pushLocked(source uint32, seq uint64, pcm []byte, now time.Time) bool {
	s := v.streams[source]
	if s == nil {
		for id, old := range v.streams {
			if now.Sub(old.last) > time.Second {
				delete(v.streams, id)
			}
		}
		if len(v.streams) >= 2*maxPeers {
			return false
		}
		s = &voiceStream{}
		v.streams[source] = s
	}
	if seq+voiceSlots <= s.latest || s.slots[seq%voiceSlots].seq == seq {
		return false
	}
	if s.latest == 0 || seq >= s.latest+voiceSlots {
		s.cursor, s.first, s.started = float64(seq*voiceFrames), now, false
		s.played = false
	} else if !s.played {
		s.cursor = min(s.cursor, float64(seq*voiceFrames))
	}
	s.latest, s.last = max(s.latest, seq), now
	slot := &s.slots[seq%voiceSlots]
	slot.seq = seq
	copy(slot.pcm[:], pcm)
	return true
}

func (s *voiceStream) sample(frame int64) float64 {
	seq := uint64(frame / voiceFrames)
	slot := &s.slots[seq%voiceSlots]
	if slot.seq != seq {
		return 0
	} // A missing packet is silence, not a shift of subsequent audio.
	return float64(int16(binary.LittleEndian.Uint16(slot.pcm[(frame%voiceFrames)*2:])))
}

func (v *voiceSession) fill(out []byte) {
	clear(out)
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	sums := make([]float64, len(out)/2)
	for id, s := range v.streams {
		channel := v.channels[id&^desktopSource]
		gain := 1.0
		if channel != nil {
			gain = float64(channel.volume.Load()) / 100
		}
		peak := 0.0
		if now.Sub(s.last) > time.Second {
			delete(v.streams, id)
			continue
		}
		end := float64((s.latest + 1) * voiceFrames)
		if !s.started {
			if now.Sub(s.first) < 60*time.Millisecond {
				continue
			}
			s.cursor = max(s.cursor, end-6*voiceFrames)
			s.started = true
			s.played = true
		}
		if end-s.cursor > 16*voiceFrames {
			s.cursor = end - 6*voiceFrames
		}
		// Gently follow microphone/output clock drift instead of cutting a block periodically.
		step := 1 + max(-0.002, min(0.002, (end-s.cursor-6*voiceFrames)/(sampleRate*5)))
		for i := range sums {
			if s.cursor >= end {
				s.started, s.first = false, now
				break
			}
			frame := int64(s.cursor)
			a, b := s.sample(frame), s.sample(frame+1)
			value := a + (b-a)*(s.cursor-float64(frame))
			peak = max(peak, math.Abs(value))
			sums[i] += value * gain
			s.cursor += step
		}
		if channel != nil {
			channel.meter(int32(peak * 100 / 32768))
		}
	}
	for i, value := range sums {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(max(-32768, min(32767, value))))))
	}
}

func (e *engine) startVoice(conn *net.UDPConn, server *net.UDPAddr, source uint32, token [32]byte) error {
	e.mu.Lock()
	v := newVoiceSession(e.ctx, conn, server, source, token)
	e.voice = v
	output := e.voiceOutput
	e.mu.Unlock()
	go func() { <-v.ctx.Done(); conn.Close() }()
	go v.receive()
	go v.sendMicrophone()
	ready := make(chan error, 1)
	go func() { v.setError(output(v.ctx, ready, v.fill)) }()
	select {
	case err := <-ready:
		if err != nil {
			v.close()
		}
		return err
	case <-time.After(10 * time.Second):
		v.close()
		return fmt.Errorf("устройство вывода не отвечает")
	}
}
