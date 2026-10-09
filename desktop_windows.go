//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

var desktopHandlerIID = ole.NewGUID("41D949AB-9862-444A-80F6-C261334DA5EB")
var agileIID = ole.NewGUID("94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90")
var desktopActivations sync.Map // Keep callbacks alive until Windows releases its final COM reference.

type desktopActivation struct {
	vtable *[4]uintptr
	refs   atomic.Int32
	pin    runtime.Pinner
	done   chan desktopActivationResult
}

type desktopActivationResult struct {
	client *wca.IAudioClient
	err    error
}

var desktopVTable = [4]uintptr{
	syscall.NewCallback(func(this, iid, out uintptr) uintptr {
		h := *(**desktopActivation)(unsafe.Pointer(&this))
		guid := *(**ole.GUID)(unsafe.Pointer(&iid))
		result := *(**uintptr)(unsafe.Pointer(&out))
		*result = 0
		if *guid != *ole.IID_IUnknown && *guid != *desktopHandlerIID && *guid != *agileIID {
			return 0x80004002 // E_NOINTERFACE
		}
		*result = this
		h.refs.Add(1)
		return 0
	}),
	syscall.NewCallback(func(this uintptr) uintptr {
		h := *(**desktopActivation)(unsafe.Pointer(&this))
		return uintptr(h.refs.Add(1))
	}),
	syscall.NewCallback(func(this uintptr) uintptr {
		h := *(**desktopActivation)(unsafe.Pointer(&this))
		return uintptr(h.release())
	}),
	syscall.NewCallback(func(this, operation uintptr) uintptr {
		h := *(**desktopActivation)(unsafe.Pointer(&this))
		op := *(***[4]uintptr)(unsafe.Pointer(&operation))
		var result int32
		var unknown *ole.IUnknown
		hr, _, _ := syscall.SyscallN((*op)[3], operation, uintptr(unsafe.Pointer(&result)), uintptr(unsafe.Pointer(&unknown)))
		r := desktopActivationResult{}
		if int32(hr) < 0 {
			r.err = fmt.Errorf("активация захвата: 0x%08x", uint32(hr))
		} else if result < 0 {
			r.err = fmt.Errorf("захват звука компьютера недоступен: 0x%08x (нужна Windows 11)", uint32(result))
		} else if unknown == nil {
			r.err = fmt.Errorf("Windows не вернула устройство захвата")
		} else {
			r.err = unknown.PutQueryInterface(wca.IID_IAudioClient, &r.client)
		}
		if unknown != nil {
			unknown.Release()
		}
		h.done <- r
		return 0
	}),
}

func (h *desktopActivation) release() int32 {
	n := h.refs.Add(-1)
	if n == 0 {
		desktopActivations.Delete(h)
		h.pin.Unpin()
	}
	return n
}

// Process loopback excludes our entire process tree, preventing voice/soundpad feedback.
func activateDesktopClient(ctx context.Context) (*wca.IAudioClient, error) {
	activate := syscall.NewLazyDLL("mmdevapi.dll").NewProc("ActivateAudioInterfaceAsync")
	if err := activate.Find(); err != nil {
		return nil, fmt.Errorf("для захвата звука компьютера нужна Windows 11: %w", err)
	}
	params := struct{ Kind, PID, Mode uint32 }{1, uint32(os.Getpid()), 1}
	variant := struct {
		Type     uint16
		Reserved [3]uint16
		Size     uint32
		Data     *byte
	}{Type: 65, Size: uint32(unsafe.Sizeof(params)), Data: (*byte)(unsafe.Pointer(&params))} // VT_BLOB
	h := &desktopActivation{vtable: &desktopVTable, done: make(chan desktopActivationResult, 1)}
	h.refs.Store(1)
	h.pin.Pin(h)
	h.pin.Pin(&params)
	h.pin.Pin(&variant)
	desktopActivations.Store(h, true)
	var operation *ole.IUnknown
	hr, _, _ := activate.Call(uintptr(unsafe.Pointer(wide("VAD\\Process_Loopback"))), uintptr(unsafe.Pointer(wca.IID_IAudioClient)), uintptr(unsafe.Pointer(&variant)), uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&operation)))
	if int32(hr) < 0 {
		h.release()
		return nil, fmt.Errorf("захват звука компьютера недоступен: 0x%08x (нужна Windows 11)", uint32(hr))
	}
	select {
	case result := <-h.done:
		operation.Release()
		h.release()
		return result.client, result.err
	case <-ctx.Done():
		// Activation cannot be cancelled; release a late result on an MTA thread.
		go func() {
			result := <-h.done
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
			defer ole.CoUninitialize()
			if result.client != nil {
				result.client.Release()
			}
			operation.Release()
			h.release()
		}()
		return nil, ctx.Err()
	}
}
