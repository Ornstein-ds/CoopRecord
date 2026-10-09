package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

func (e *engine) setDesktopMuted(muted bool) error {
	e.desktopMu.Lock()
	defer e.desktopMu.Unlock()
	if muted {
		e.inputMu.Lock()
		e.desktopMuted.Store(true)
		e.inputMu.Unlock()
		if e.desktopCancel != nil {
			e.desktopCancel()
			<-e.desktopDone
			e.desktopCancel = nil
		}
		e.desktopMixer.peak.Store(0)
		return nil
	}
	if !e.desktopMuted.Load() {
		return nil
	}
	if e.desktopCancel != nil {
		e.desktopCancel()
		<-e.desktopDone
	}
	e.mu.Lock()
	if e.ctx == nil || e.ctx.Err() != nil || e.voice == nil || !e.inputReady {
		e.mu.Unlock()
		return fmt.Errorf("сначала подключитесь к сессии")
	}
	ctx, cancel := context.WithCancel(e.ctx)
	voice, audio, capture := e.voice, e.audio, e.desktopCapture
	e.mu.Unlock()
	done, ready := make(chan struct{}), make(chan error, 1)
	e.desktopCancel, e.desktopDone = cancel, done
	go func() {
		defer close(done)
		defer e.desktopMuted.Store(true)
		defer cancel()
		var level atomic.Int32
		first := true
		err := capture(ctx, "", ready, func(p packet) error {
			e.inputMu.Lock()
			defer e.inputMu.Unlock()
			if ctx.Err() != nil || e.desktopMuted.Load() {
				return nil
			}
			e.desktopMixer.meter(pcmPeak(p.PCM))
			pcm := make([]byte, len(p.PCM))
			applyMixerGain(pcm, p.PCM, e.desktopMixer.volume.Load())
			p.PCM, p.Desktop = pcm, true
			p.Discontinuity = p.Discontinuity || first
			first = false
			voice.enqueue(p)
			if e.audioID == "" {
				return nil
			}
			select {
			case audio <- audioEvent{packet: p, session: e.audioID}:
				return nil
			default:
				return fmt.Errorf("сеть или диск не успевает принимать звук компьютера")
			}
		}, &level)
		if err != nil && ctx.Err() == nil && !e.desktopMuted.Load() {
			e.fail(fmt.Errorf("звук компьютера выключен: %w", err))
			e.mu.Lock()
			w, recording := e.client, e.liveID != ""
			e.mu.Unlock()
			if w != nil && recording {
				_ = w.send(message{Type: "desktop_error", Error: err.Error()})
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-done
			return err
		}
	case <-ctx.Done():
		select {
		case err := <-ready:
			if err != nil {
				return err
			}
		default:
		}
		return ctx.Err()
	case <-time.After(10 * time.Second):
		cancel()
		return fmt.Errorf("захват звука компьютера не отвечает")
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	select {
	case <-done:
		return fmt.Errorf("захват звука компьютера завершился")
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	e.desktopMuted.Store(false)
	return nil
}

func (r *recording) prepareDesktopTracks(host string, peers []*peer) (err error) {
	r.localDesktop, err = newTrack(r.dir, "desktop-00")
	if err != nil {
		return err
	}
	r.m.Tracks = append(r.m.Tracks, trackInfo{ID: "desktop-00", Name: host + " — компьютер", Desktop: true})
	for i, p := range peers {
		id := fmt.Sprintf("desktop-%02d", i+1)
		w, err := newTrack(r.dir, id)
		if err != nil {
			return err
		}
		r.desktopTracks[p] = w
		r.m.Tracks = append(r.m.Tracks, trackInfo{ID: id, Name: p.name + " — компьютер", Desktop: true, Remote: true})
		for _, s := range p.samples[max(0, len(p.samples)-8):] {
			if err := w.clock(s); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *recording) closeDesktopTracks() (err error) {
	if r.localDesktop != nil {
		err = r.localDesktop.close()
	}
	for _, w := range r.desktopTracks {
		err = errors.Join(err, w.close())
	}
	return err
}
