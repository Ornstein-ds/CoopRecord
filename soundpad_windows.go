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
	header := waveHeader{Data: &pcm[0], Length: uint32(len(pcm))}
	var pin runtime.Pinner
	pin.Pin(&pcm[0])
	pin.Pin(&header)
	defer pin.Unpin()
	invoke := func(name string, args ...uintptr) error {
		r, _, _ := winmm.NewProc(name).Call(args...)
		if r != 0 {
			return fmt.Errorf("%s: код %d", name, r)
		}
		return nil
	}
	volume := int32(-1)
	updateVolume := func() error {
		if channel == nil || channel.volume.Load() == volume {
			return nil
		}
		volume = channel.volume.Load()
		n := uintptr(volume) * 65535 / 100
		return invoke("waveOutSetVolume", handle, n|n<<16)
	}
	if err := updateVolume(); err != nil {
		return err
	}
	if err := invoke("waveOutPause", handle); err != nil {
		return err
	}
	if err := invoke("waveOutPrepareHeader", handle, uintptr(unsafe.Pointer(&header)), unsafe.Sizeof(header)); err != nil {
		return err
	}
	defer winmm.NewProc("waveOutUnprepareHeader").Call(handle, uintptr(unsafe.Pointer(&header)), unsafe.Sizeof(header))
	defer winmm.NewProc("waveOutReset").Call(handle)
	if err := invoke("waveOutWrite", handle, uintptr(unsafe.Pointer(&header)), unsafe.Sizeof(header)); err != nil {
		return err
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
	for atomic.LoadUint32(&header.Flags)&1 == 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return fmt.Errorf("устройство вывода не завершило воспроизведение")
		case <-ticker.C:
			if err := updateVolume(); err != nil {
				return err
			}
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
	return nil
}
