//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

func ignoreVoiceReadError(err error) bool {
	// UDP ICMP errors after a peer exits and oversized untrusted datagrams are not fatal.
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.Errno(10040)) // WSAEMSGSIZE
}

// One shared-mode output stream stays open for the entire conversation.
func renderVoice(ctx context.Context, ready chan<- error, fill func([]byte)) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	announced := false
	defer func() {
		if !announced {
			ready <- result
		}
	}()
	if err := initAudioCOM(); err != nil {
		return err
	}
	defer ole.CoUninitialize()
	e, err := audioEnumerator()
	if err != nil {
		return err
	}
	defer e.Release()
	var device *wca.IMMDevice
	if err := e.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &device); err != nil {
		return fmt.Errorf("устройство вывода: %w", err)
	}
	defer device.Release()
	var client *wca.IAudioClient
	hr, _, _ := syscall.SyscallN(device.VTable().Activate, uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(wca.IID_IAudioClient)), uintptr(wca.CLSCTX_ALL), 0, uintptr(unsafe.Pointer(&client)))
	if int32(hr) < 0 {
		return fmt.Errorf("голос WASAPI Activate: 0x%08x", uint32(hr))
	}
	defer client.Release()
	format := wca.WAVEFORMATEX{WFormatTag: 1, NChannels: 1, NSamplesPerSec: sampleRate, NAvgBytesPerSec: sampleRate * 2, NBlockAlign: 2, WBitsPerSample: 16}
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err := client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, 400000, 0, &format, nil); err != nil {
		return fmt.Errorf("голос WASAPI Initialize: %w", err)
	}
	var capacity uint32
	if err := client.GetBufferSize(&capacity); err != nil {
		return err
	}
	var render *wca.IAudioRenderClient
	if err := client.GetService(wca.IID_IAudioRenderClient, &render); err != nil {
		return err
	}
	defer render.Release()
	write := func(frames uint32) error {
		if frames == 0 {
			return nil
		}
		var data *byte
		if err := render.GetBuffer(frames, &data); err != nil {
			return err
		}
		fill(unsafe.Slice(data, int(frames)*2))
		return render.ReleaseBuffer(frames, 0)
	}
	if err := write(capacity); err != nil {
		return err
	}
	if err := client.Start(); err != nil {
		return err
	}
	defer client.Stop()
	ready <- nil
	announced = true
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		var padding uint32
		if err := client.GetCurrentPadding(&padding); err != nil {
			return err
		}
		if padding > capacity {
			return fmt.Errorf("неверный размер выходного буфера")
		}
		if err := write(capacity - padding); err != nil {
			return err
		}
	}
}
