//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var queryCounter = kernel.NewProc("QueryPerformanceCounter")
var counterFrequency = func() int64 {
	var f int64
	kernel.NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&f)))
	return f
}()

// Same clock and units as WASAPI's QPC timestamps, independent of wall time.
func clockNow() int64 {
	var n int64
	queryCounter.Call(uintptr(unsafe.Pointer(&n)))
	return n/counterFrequency*1e9 + n%counterFrequency*1e9/counterFrequency
}

type inputDevice struct{ ID, Name string }

func initAudioCOM() error {
	err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	if e, ok := err.(*ole.OleError); ok && e.Code() == 1 {
		return nil
	}
	return err
}

func audioEnumerator() (*wca.IMMDeviceEnumerator, error) {
	var e *wca.IMMDeviceEnumerator
	err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &e)
	return e, err
}

func inputDevices() ([]inputDevice, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initAudioCOM(); err != nil {
		return nil, err
	}
	defer ole.CoUninitialize()
	e, err := audioEnumerator()
	if err != nil {
		return nil, err
	}
	defer e.Release()
	var collection *wca.IMMDeviceCollection
	if err = e.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, err
	}
	defer collection.Release()
	var count uint32
	if err = collection.GetCount(&count); err != nil {
		return nil, err
	}
	devices := []inputDevice{}
	for i := uint32(0); i < count; i++ {
		var d *wca.IMMDevice
		if err = collection.Item(i, &d); err != nil {
			return nil, err
		}
		var id string
		err = d.GetId(&id)
		name := id
		var props *wca.IPropertyStore
		if err == nil && d.OpenPropertyStore(wca.STGM_READ, &props) == nil {
			var v wca.PROPVARIANT
			if props.GetValue(&wca.PKEY_Device_FriendlyName, &v) == nil {
				// go-wca's String consumes and frees the LPWSTR itself.
				name = v.String()
			}
			props.Release()
		}
		d.Release()
		if err != nil {
			return nil, err
		}
		devices = append(devices, inputDevice{id, name})
	}
	return devices, nil
}

// All COM calls stay on one OS thread. The callback must not block the audio driver.
func captureAudio(ctx context.Context, deviceID string, ready chan<- error, emit func(packet) error, level *atomic.Int32) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	announced := false
	defer func() {
		if !announced {
			ready <- result
		}
		level.Store(0)
	}()
	if result = initAudioCOM(); result != nil {
		return
	}
	defer ole.CoUninitialize()
	e, err := audioEnumerator()
	if err != nil {
		return err
	}
	defer e.Release()
	var device *wca.IMMDevice
	id, err := syscall.UTF16PtrFromString(deviceID)
	if err != nil {
		return err
	}
	// go-wca v0.3.0 leaves GetDevice unimplemented and passes Activate's context incorrectly.
	hr, _, _ := syscall.SyscallN(e.VTable().GetDevice, uintptr(unsafe.Pointer(e)), uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(&device)))
	if int32(hr) < 0 {
		return fmt.Errorf("микрофон недоступен: 0x%08x", uint32(hr))
	}
	defer device.Release()
	var client *wca.IAudioClient
	hr, _, _ = syscall.SyscallN(device.VTable().Activate, uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(wca.IID_IAudioClient)), uintptr(wca.CLSCTX_ALL), 0, uintptr(unsafe.Pointer(&client)))
	if int32(hr) < 0 {
		return fmt.Errorf("WASAPI Activate: 0x%08x", uint32(hr))
	}
	defer client.Release()
	format := wca.WAVEFORMATEX{WFormatTag: 1, NChannels: 1, NSamplesPerSec: sampleRate, NAvgBytesPerSec: sampleRate * 2, NBlockAlign: 2, WBitsPerSample: 16}
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err = client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, 1000000, 0, &format, nil); err != nil {
		return fmt.Errorf("микрофон: %w (проверьте разрешение Windows на доступ к микрофону)", err)
	}
	var capture *wca.IAudioCaptureClient
	if err = client.GetService(wca.IID_IAudioCaptureClient, &capture); err != nil {
		return err
	}
	defer capture.Release()
	if err = client.Start(); err != nil {
		return err
	}
	defer client.Stop()
	ready <- nil
	announced = true
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var lastData = time.Now()
	var seen bool
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		for {
			var frames uint32
			if err = capture.GetNextPacketSize(&frames); err != nil {
				return err
			}
			if frames == 0 {
				break
			}
			var data *byte
			var status uint32
			var pos, qpc uint64
			if err = capture.GetBuffer(&data, &frames, &status, &pos, &qpc); err != nil {
				return err
			}
			if frames == 0 {
				break
			}
			pcm := make([]byte, int(frames)*2)
			if status&2 == 0 && data != nil {
				copy(pcm, unsafe.Slice(data, len(pcm)))
			}
			if err = capture.ReleaseBuffer(frames); err != nil {
				return err
			}
			if status&4 != 0 {
				return fmt.Errorf("микрофон вернул недостоверную временную метку; запись остановлена")
			}
			if seen && status&1 != 0 {
				return fmt.Errorf("пропуск аудио в драйвере микрофона; запись остановлена, принятый звук сохранён")
			}
			seen = true
			lastData = time.Now()
			peak := int32(0)
			for i := 0; i < len(pcm); i += 2 {
				n := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
				if n < 0 {
					n = -n
				}
				if n > peak {
					peak = n
				}
			}
			level.Store(peak * 100 / 32768)
			if err = emit(packet{Time: int64(qpc) * 100, PCM: pcm}); err != nil {
				return err
			}
		}
		if time.Since(lastData) > 3*time.Second {
			return fmt.Errorf("микрофон перестал передавать звук")
		}
	}
}
