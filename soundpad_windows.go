//go:build windows

package main

import (
	"context"
	"fmt"
	"github.com/moutend/go-wca/pkg/wca"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var winmm = syscall.NewLazyDLL("winmm.dll")

type waveHeader struct {
	Data             *byte
	Length, Recorded uint32
	User             uintptr
	Flags, Loops     uint32
	Next, Reserved   uintptr
}

func playPadPCM(ctx context.Context, pcm []byte, start int64) error {
	return playPadMixed(ctx, pcm, start, nil)
}

func playPadMixed(ctx context.Context, pcm []byte, start int64, channel *mixerChannel) error {
	if ctx.Err() != nil {
		return nil
	}
	var handle uintptr
	format := wca.WAVEFORMATEX{WFormatTag: 1, NChannels: 1, NSamplesPerSec: sampleRate, NAvgBytesPerSec: sampleRate * 2, NBlockAlign: 2, WBitsPerSample: 16}
	r, _, _ := winmm.NewProc("waveOutOpen").Call(uintptr(unsafe.Pointer(&handle)), 0xffffffff, uintptr(unsafe.Pointer(&format)), 0, 0, 0)
	if r != 0 {
		return fmt.Errorf("не удалось открыть устройство вывода (код %d)", r)
	}
	defer winmm.NewProc("waveOutClose").Call(handle)
	// If a network command was late, keep its timeline instead of shifting the whole clip.
	skip := max(int64(0), (clockNow()-start)*sampleRate/1e9)
	if skip >= int64(len(pcm)/2) {
		return fmt.Errorf("команда воспроизведения пришла слишком поздно")
	}
	pcm = pcm[skip*2:]
	// Short queued buffers let gain exceed unity and change during playback.
	var buffers [3]struct {
		header waveHeader
		data   [sampleRate * 2 / 50]byte // 20 ms
		queued bool
	}
	var pin runtime.Pinner
	defer pin.Unpin()
	invoke := func(name string, args ...uintptr) error {
		r, _, _ := winmm.NewProc(name).Call(args...)
		if r != 0 {
			return fmt.Errorf("%s: код %d", name, r)
		}
		return nil
	}
	if err := invoke("waveOutSetVolume", handle, 0xffffffff); err != nil {
		return err
	}
	if err := invoke("waveOutPause", handle); err != nil {
		return err
	}
	for i := range buffers {
		b := &buffers[i]
		b.header = waveHeader{Data: &b.data[0], Length: uint32(len(b.data))}
		pin.Pin(&b.data[0])
		pin.Pin(&b.header)
		if err := invoke("waveOutPrepareHeader", handle, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(b.header)); err != nil {
			return err
		}
		defer winmm.NewProc("waveOutUnprepareHeader").Call(handle, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(b.header))
	}
	defer winmm.NewProc("waveOutReset").Call(handle)
	offset := 0
	queue := func(i int) error {
		b := &buffers[i]
		n := min(len(b.data), len(pcm)-offset)
		gain := int32(100)
		if channel != nil {
			gain = channel.volume.Load()
		}
		applyMixerGain(b.data[:n], pcm[offset:offset+n], gain)
		b.header.Length = uint32(n)
		if err := invoke("waveOutWrite", handle, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(b.header)); err != nil {
			return err
		}
		offset += n
		b.queued = true
		return nil
	}
	for i := range buffers {
		if offset < len(pcm) {
			if err := queue(i); err != nil {
				return err
			}
		}
	}
	timer := time.NewTimer(time.Duration(max(0, start-clockNow())))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}
	if err := invoke("waveOutRestart", handle); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Duration(len(pcm)/2)*time.Second/sampleRate + 2*time.Second)
	defer deadline.Stop()
	previous := 0
	started := time.Now()
	for {
		pending := false
		for i := range buffers {
			b := &buffers[i]
			if b.queued && atomic.LoadUint32(&b.header.Flags)&1 != 0 { // WHDR_DONE
				b.queued = false
			}
			if !b.queued && offset < len(pcm) {
				if err := queue(i); err != nil {
					return err
				}
			}
			pending = pending || b.queued
		}
		if !pending {
			if channel != nil {
				channel.meter(pcmPeak(pcm[previous*2:]))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return fmt.Errorf("устройство вывода не завершило воспроизведение")
		case <-ticker.C:
			if channel != nil {
				// MMTIME: request sample position, with a clock fallback for old drivers.
				position := struct{ Kind, Value, Extra uint32 }{Kind: 2}
				r, _, _ := winmm.NewProc("waveOutGetPosition").Call(handle, uintptr(unsafe.Pointer(&position)), unsafe.Sizeof(position))
				frame := int(time.Since(started).Seconds() * sampleRate)
				if r == 0 && position.Kind == 2 {
					frame = int(position.Value)
				}
				if r == 0 && position.Kind == 4 {
					frame = int(position.Value) / 2
				}
				frame = min(len(pcm)/2, max(previous, frame))
				channel.meter(pcmPeak(pcm[previous*2 : frame*2]))
				previous = frame
			}
		}
	}
}
