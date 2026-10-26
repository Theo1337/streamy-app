//go:build windows

package rtc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	processLoopbackExcludeTree = 1
	activationTypeProcess      = 1
	propVariantBlob            = 0x41
	audioClientShareModeShared = 0
	audioClientFlagLoopback    = 0x00020000
	audioClientFlagAutoConvert = 0x80000000
	audioClientFlagSrcQuality  = 0x08000000
	audioCaptureFlagSilent     = 0x00000002
)

var (
	moveNativeMemory = windows.NewLazySystemDLL("kernel32.dll").NewProc("RtlMoveMemory")
	audioClientIID   = windows.GUID{
		Data1: 0x1cb9ad4c, Data2: 0xdbfa, Data3: 0x4c32,
		Data4: [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2},
	}
	activateHandlerIID = windows.GUID{
		Data1: 0x41d949ab, Data2: 0x9862, Data3: 0x444a,
		Data4: [8]byte{0x80, 0xf6, 0xc2, 0x61, 0x33, 0x4d, 0xa5, 0xeb},
	}
	agileObjectIID     = windows.GUID{Data1: 0x94ea2b94, Data2: 0xe9cc, Data3: 0x49e0, Data4: [8]byte{0xc0, 0xff, 0xee, 0x64, 0xca, 0x8f, 0x5b, 0x90}}
	iunknownIID        = windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	activationResults  sync.Map
	activationID       atomic.Uint64
	activationVtbl     uintptr
	activationVtblMu   sync.Once
	activationCallback uintptr
)

type activationResult struct {
	done        chan struct{}
	audioClient uintptr
	err         error
}

type processEntry struct {
	pid     uint32
	parent  uint32
	exeName string
}

func discordRootProcessID() (uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("list processes to find the Discord desktop app: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, fmt.Errorf("read process list to find Discord: %w", err)
	}
	entries := make([]processEntry, 0, 128)
	for {
		entries = append(entries, processEntry{
			pid:     entry.ProcessID,
			parent:  entry.ParentProcessID,
			exeName: windows.UTF16ToString(entry.ExeFile[:]),
		})
		err := windows.Process32Next(snapshot, &entry)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return 0, fmt.Errorf("continue reading process list to find Discord: %w", err)
		}
	}

	byPID := make(map[uint32]processEntry, len(entries))
	for _, item := range entries {
		byPID[item.pid] = item
	}
	roots := make(map[uint32]struct{})
	cyclic := false
	for _, item := range entries {
		if !isDiscordProcess(item.exeName) {
			continue
		}
		root, looped := discordTreeRoot(item, byPID)
		roots[root.pid] = struct{}{}
		cyclic = cyclic || looped
	}
	if cyclic {
		// Windows records a process's parent PID once, when it is created, and
		// never rewrites it. A recycled PID can therefore make two Discord
		// processes name each other as parent, so the walks above disagree
		// about the root. The real root is the Discord process that owns the
		// rest of the tree.
		roots = map[uint32]struct{}{widestDiscordRoot(entries, byPID).pid: {}}
	}
	if len(roots) == 0 {
		return 0, nil
	}
	if len(roots) > 1 {
		return 0, errors.New("multiple Discord desktop process trees are running; close extra Discord instances so Streamy can exclude them reliably")
	}
	for pid := range roots {
		return pid, nil
	}
	return 0, nil
}

// discordTreeRoot walks parent links to the top of the Discord tree. The second
// result reports that the walk found a cycle instead of a real root; without
// that guard a stale parent PID would loop forever and hang the stream start.
func discordTreeRoot(process processEntry, byPID map[uint32]processEntry) (processEntry, bool) {
	root := process
	seen := map[uint32]struct{}{root.pid: {}}
	for {
		parent, ok := byPID[root.parent]
		if !ok || !isDiscordProcess(parent.exeName) {
			return root, false
		}
		if _, repeat := seen[parent.pid]; repeat {
			return root, true
		}
		seen[parent.pid] = struct{}{}
		root = parent
	}
}

// widestDiscordRoot returns the Discord process with the most Discord
// descendants. When parent links contain a cycle, that is the process the whole
// tree hangs off, because the other cycle members are its children.
func widestDiscordRoot(entries []processEntry, byPID map[uint32]processEntry) processEntry {
	children := make(map[uint32][]uint32, len(entries))
	for _, item := range entries {
		children[item.parent] = append(children[item.parent], item.pid)
	}

	best := processEntry{}
	bestCount := -1
	for _, candidate := range entries {
		if !isDiscordProcess(candidate.exeName) {
			continue
		}
		count := 0
		seen := map[uint32]struct{}{candidate.pid: {}}
		queue := []uint32{candidate.pid}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			for _, child := range children[pid] {
				if _, visited := seen[child]; visited {
					continue
				}
				seen[child] = struct{}{}
				if entry, ok := byPID[child]; ok && isDiscordProcess(entry.exeName) {
					count++
				}
				queue = append(queue, child)
			}
		}
		if count > bestCount {
			best, bestCount = candidate, count
		}
	}
	return best
}

func isDiscordProcess(name string) bool {
	switch strings.ToLower(name) {
	case "discord.exe", "discordcanary.exe", "discordptb.exe", "discorddevelopment.exe":
		return true
	default:
		return false
	}
}

func captureProcessLoopback(ctx context.Context, excludePID uint32, startEncoder func(pcmFormat) error, output io.Writer, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		sendAudioReady(ready, fmt.Errorf("initialize COM for WASAPI: %w", err))
		return err
	}
	defer windows.CoUninitialize()

	client, err := activateProcessLoopback(ctx, excludePID)
	if err != nil {
		sendAudioReady(ready, err)
		return err
	}
	defer releaseCOM(client)

	// Process-loopback clients are virtual mixer objects. GetMixFormat and
	// IsFormatSupported return E_NOTIMPL (0x80004001), so the capture format
	// has to be supplied to Initialize. AUTOCONVERTPCM resamples the mix.
	formatBytes := pcmWaveFormat(48000, 2)
	format, err := rawPCMFormat(1, 16, 48000, 2, 4, [16]byte{})
	if err != nil {
		sendAudioReady(ready, err)
		return err
	}

	flags := uintptr(audioClientFlagLoopback | audioClientFlagAutoConvert | audioClientFlagSrcQuality)
	if _, err := callCOM(client, 3,
		audioClientShareModeShared,
		flags,
		0,
		0,
		uintptr(unsafe.Pointer(&formatBytes[0])),
		0,
	); err != nil {
		err = fmt.Errorf("initialize WASAPI process-loopback stream: %w", err)
		sendAudioReady(ready, err)
		return err
	}
	runtime.KeepAlive(formatBytes)

	var captureClient uintptr
	if _, err := callCOM(client, 14,
		uintptr(unsafe.Pointer(&captureClientIID)),
		uintptr(unsafe.Pointer(&captureClient)),
	); err != nil {
		err = fmt.Errorf("get WASAPI capture service: %w", err)
		sendAudioReady(ready, err)
		return err
	}
	defer releaseCOM(captureClient)

	if err := startEncoder(format); err != nil {
		sendAudioReady(ready, err)
		return err
	}
	if _, err := callCOM(client, 10); err != nil {
		err = fmt.Errorf("start WASAPI process-loopback stream: %w", err)
		sendAudioReady(ready, err)
		return err
	}
	defer callCOM(client, 11)
	sendAudioReady(ready, nil)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var frames uint32
		if _, err := callCOM(captureClient, 5, uintptr(unsafe.Pointer(&frames))); err != nil {
			return fmt.Errorf("check WASAPI capture packet: %w", err)
		}
		if frames == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		var data uintptr
		var packetFrames, captureFlags uint32
		var devicePosition, qpcPosition uint64
		if _, err := callCOM(captureClient, 3,
			uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&packetFrames)),
			uintptr(unsafe.Pointer(&captureFlags)),
			uintptr(unsafe.Pointer(&devicePosition)),
			uintptr(unsafe.Pointer(&qpcPosition)),
		); err != nil {
			return fmt.Errorf("read WASAPI capture packet: %w", err)
		}
		byteCount := int(packetFrames) * int(format.blockAlign)
		pcm := make([]byte, byteCount)
		if captureFlags&audioCaptureFlagSilent == 0 && byteCount > 0 {
			if data == 0 {
				_, _ = callCOM(captureClient, 4, uintptr(packetFrames))
				return errors.New("WASAPI returned a nil buffer for a non-silent packet")
			}
			readNativeInto(data, pcm)
		}
		if _, err := callCOM(captureClient, 4, uintptr(packetFrames)); err != nil {
			return fmt.Errorf("release WASAPI capture packet: %w", err)
		}
		if byteCount == 0 {
			continue
		}
		if _, err := output.Write(pcm); err != nil {
			return fmt.Errorf("send WASAPI samples to the Opus encoder: %w", err)
		}
	}
}

func sendAudioReady(ready chan<- error, err error) {
	select {
	case ready <- err:
	default:
	}
}

func activateProcessLoopback(ctx context.Context, excludePID uint32) (uintptr, error) {
	if err := initializeActivationCallbacks(); err != nil {
		return 0, err
	}
	result := &activationResult{done: make(chan struct{})}
	id := activationID.Add(1)
	activationResults.Store(id, result)
	defer activationResults.Delete(id)

	handler, err := windows.LocalAlloc(0x0040, uint32(2*unsafe.Sizeof(uintptr(0))))
	if err != nil {
		return 0, fmt.Errorf("allocate WASAPI activation callback: %w", err)
	}
	defer windows.LocalFree(windows.Handle(handler))
	writeNative(handler, nativeUintBytes(activationVtbl))
	writeNative(handler+unsafe.Sizeof(uintptr(0)), nativeUintBytes(uintptr(id)))

	pointerSize := unsafe.Sizeof(uintptr(0))
	blobPointerOffset := uintptr(8) + (uintptr(4)+pointerSize-1)&^(pointerSize-1)
	variantSize := uintptr(8) + (uintptr(4)+pointerSize-1)&^(pointerSize-1) + pointerSize
	params, err := windows.LocalAlloc(0x0040, uint32(variantSize))
	if err != nil {
		return 0, fmt.Errorf("allocate WASAPI process-loopback parameters: %w", err)
	}
	defer windows.LocalFree(windows.Handle(params))
	activationParams, err := windows.LocalAlloc(0x0040, 12)
	if err != nil {
		return 0, fmt.Errorf("allocate WASAPI activation structure: %w", err)
	}
	defer windows.LocalFree(windows.Handle(activationParams))
	activationBytes := make([]byte, 12)
	binary.LittleEndian.PutUint32(activationBytes, activationTypeProcess)
	binary.LittleEndian.PutUint32(activationBytes[4:], excludePID)
	binary.LittleEndian.PutUint32(activationBytes[8:], processLoopbackExcludeTree)
	writeNative(activationParams, activationBytes)
	variantBytes := make([]byte, variantSize)
	binary.LittleEndian.PutUint16(variantBytes, propVariantBlob)
	binary.LittleEndian.PutUint32(variantBytes[8:], 12)
	copy(variantBytes[blobPointerOffset:], nativeUintBytes(activationParams))
	writeNative(params, variantBytes)

	devicePath, err := windows.UTF16PtrFromString(`VAD\Process_Loopback`)
	if err != nil {
		return 0, err
	}
	proc := windows.NewLazySystemDLL("Mmdevapi.dll").NewProc("ActivateAudioInterfaceAsync")
	var asyncOperation uintptr
	hr, _, _ := proc.Call(
		uintptr(unsafe.Pointer(devicePath)),
		uintptr(unsafe.Pointer(&audioClientIID)),
		params,
		handler,
		uintptr(unsafe.Pointer(&asyncOperation)),
	)
	if int32(hr) < 0 {
		releaseCOM(asyncOperation)
		return 0, fmt.Errorf("ActivateAudioInterfaceAsync failed: HRESULT 0x%08x; process-loopback capture requires Windows 10 build 20348 or newer", uint32(hr))
	}
	if asyncOperation == 0 {
		return 0, errors.New("ActivateAudioInterfaceAsync returned a nil operation")
	}
	defer releaseCOM(asyncOperation)

	select {
	case <-result.done:
	case <-ctx.Done():
		<-result.done
		return 0, ctx.Err()
	}
	if result.err != nil {
		return 0, result.err
	}
	if result.audioClient == 0 {
		return 0, errors.New("WASAPI process-loopback activation returned a nil audio client")
	}
	return result.audioClient, nil
}

func initializeActivationCallbacks() error {
	var initErr error
	activationVtblMu.Do(func() {
		queryInterface := syscall.NewCallback(func(this, iid, out uintptr) uintptr {
			if iid == 0 || out == 0 {
				return 0x80004003
			}
			if !supportsActivationCallbackIID(readNative(iid, 16)) {
				writeNative(out, nativeUintBytes(0))
				return 0x80004002
			}
			writeNative(out, nativeUintBytes(this))
			return 0
		})
		addRef := syscall.NewCallback(func(uintptr) uintptr { return 1 })
		release := syscall.NewCallback(func(uintptr) uintptr { return 1 })
		activationCallback = syscall.NewCallback(func(this, operation uintptr) uintptr {
			id := nativeUint(readNative(this+unsafe.Sizeof(uintptr(0)), int(unsafe.Sizeof(uintptr(0)))))
			value, ok := activationResults.Load(uint64(id))
			if !ok {
				return 0x80004005
			}
			result := value.(*activationResult)
			var activateResult uintptr
			var audioClient uintptr
			_, err := callCOM(operation, 3,
				uintptr(unsafe.Pointer(&activateResult)),
				uintptr(unsafe.Pointer(&audioClient)),
			)
			if err == nil && int32(activateResult) < 0 {
				err = fmt.Errorf("WASAPI process-loopback activation failed: HRESULT 0x%08x", uint32(activateResult))
			}
			result.audioClient, result.err = audioClient, err
			close(result.done)
			return 0
		})

		vtable, err := windows.LocalAlloc(0x0040, uint32(4*unsafe.Sizeof(uintptr(0))))
		if err != nil {
			initErr = fmt.Errorf("allocate WASAPI callback vtable: %w", err)
			return
		}
		methods := []uintptr{queryInterface, addRef, release, activationCallback}
		methodBytes := make([]byte, len(methods)*int(unsafe.Sizeof(uintptr(0))))
		for i, method := range methods {
			offset := i * int(unsafe.Sizeof(uintptr(0)))
			copy(methodBytes[offset:offset+int(unsafe.Sizeof(uintptr(0)))], nativeUintBytes(method))
		}
		writeNative(vtable, methodBytes)
		activationVtbl = vtable
	})
	return initErr
}

func supportsActivationCallbackIID(requested []byte) bool {
	return bytes.Equal(requested, guidBytes(iunknownIID)) ||
		bytes.Equal(requested, guidBytes(activateHandlerIID)) ||
		bytes.Equal(requested, guidBytes(agileObjectIID))
}

var captureClientIID = windows.GUID{
	Data1: 0xc8adbd64, Data2: 0xe71e, Data3: 0x48a0,
	Data4: [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17},
}

func callCOM(object uintptr, method uintptr, args ...uintptr) (uintptr, error) {
	if object == 0 {
		return 0, errors.New("call on a nil COM interface")
	}
	vtable := nativeUint(readNative(object, int(unsafe.Sizeof(uintptr(0)))))
	procedure := nativeUint(readNative(vtable+method*unsafe.Sizeof(uintptr(0)), int(unsafe.Sizeof(uintptr(0)))))
	allArgs := make([]uintptr, 1, len(args)+1)
	allArgs[0] = object
	allArgs = append(allArgs, args...)
	result, _, _ := syscall.SyscallN(procedure, allArgs...)
	if int32(result) < 0 {
		return result, fmt.Errorf("COM method %d failed: HRESULT 0x%08x", method, uint32(result))
	}
	return result, nil
}

func releaseCOM(object uintptr) {
	if object == 0 {
		return
	}
	vtable := nativeUint(readNative(object, int(unsafe.Sizeof(uintptr(0)))))
	procedure := nativeUint(readNative(vtable+2*unsafe.Sizeof(uintptr(0)), int(unsafe.Sizeof(uintptr(0)))))
	_, _, _ = syscall.SyscallN(procedure, object)
}

func readNative(address uintptr, length int) []byte {
	buffer := make([]byte, length)
	readNativeInto(address, buffer)
	return buffer
}

func readNativeInto(address uintptr, buffer []byte) {
	if len(buffer) == 0 {
		return
	}
	_, _, _ = moveNativeMemory.Call(
		uintptr(unsafe.Pointer(&buffer[0])),
		address,
		uintptr(len(buffer)),
	)
}

func writeNative(address uintptr, buffer []byte) {
	if len(buffer) == 0 {
		return
	}
	_, _, _ = moveNativeMemory.Call(
		address,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
}

func nativeUintBytes(value uintptr) []byte {
	buffer := make([]byte, unsafe.Sizeof(uintptr(0)))
	for i := range buffer {
		buffer[i] = byte(value >> (8 * i))
	}
	return buffer
}

func nativeUint(buffer []byte) uintptr {
	var value uintptr
	for i := range buffer {
		value |= uintptr(buffer[i]) << (8 * i)
	}
	return value
}

func guidBytes(guid windows.GUID) []byte {
	buffer := make([]byte, 16)
	binary.LittleEndian.PutUint32(buffer, guid.Data1)
	binary.LittleEndian.PutUint16(buffer[4:], guid.Data2)
	binary.LittleEndian.PutUint16(buffer[6:], guid.Data3)
	copy(buffer[8:], guid.Data4[:])
	return buffer
}
