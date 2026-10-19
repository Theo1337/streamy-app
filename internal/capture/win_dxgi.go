//go:build windows

package capture

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file talks to DXGI directly, through COM, for one reason: it is the only
// reliable way to learn which output index ffmpeg's ddagrab filter should be
// pointed at.
//
// The obvious source, the Windows display configuration API, does not work.
// DISPLAYCONFIG_PATH_TARGET_INFO has a field called `id` that looks like it
// should be the output index; on the machine this was developed on it reports
// 33024, 33025 and 33029 for three outputs. Those are driver-assigned
// identifiers. ffmpeg's output_idx is the argument to
// IDXGIAdapter::EnumOutputs, a zero-based counter. There is no relationship
// between the two, so the display path can describe an output but cannot name
// it in the terms ffmpeg uses. Run `streamy-capture -list-paths` to see them.
//
// DXGI_OUTPUT_DESC is what is actually needed, and it carries three useful
// things at once: the desktop rectangle, the rotation, and the HMONITOR, which
// joins exactly with the monitors EnumDisplayMonitors reports.
//
// COM from Go is not as bad as it looks. A COM interface pointer points at a
// pointer to an array of function pointers, and every method takes the object
// pointer as its first argument. That is the entire ABI; the only fiddly part
// is counting the vtable slots correctly, which is what the constants below are
// for.

// COM vtable slots. Each interface inherits the methods of the ones it is built
// on, so these numbers count from IUnknown upwards. They are only meaningful
// relative to each other, which is why the whole sequence is listed even though
// this file calls only some of them:
//
//	IUnknown     QueryInterface(0) AddRef(1) Release(2)
//	IDXGIObject  SetPrivateData(3) SetPrivateDataInterface(4) GetPrivateData(5) GetParent(6)
const (
	comSlotRelease = 2

	comSlotFactoryEnumAdapters  = 7
	comSlotFactoryEnumAdapters1 = 12
	comSlotAdapterEnumOutputs   = 7
	comSlotAdapterGetDesc       = 8
	comSlotAdapterGetDesc1      = 10
	comSlotOutputGetDesc        = 7
)

// DXGI error and flag values.
const (
	dxgiErrorNotFound        = 0x887A0002
	dxgiAdapterFlagSoftware  = 0x00000002
	dxgiModeRotationIdentity = 1
	dxgiModeRotation90       = 2
	dxgiModeRotation180      = 3
	dxgiModeRotation270      = 4
)

var (
	dxgiDLL                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
)

// iidIDXGIFactory1 is IID_IDXGIFactory1, {770AAE78-F26F-4DBA-A829-253C83D1B387}.
var iidIDXGIFactory1 = windows.GUID{
	Data1: 0x770AAE78,
	Data2: 0xF26F,
	Data3: 0x4DBA,
	Data4: [8]byte{0xA8, 0x29, 0x25, 0x3C, 0x83, 0xD1, 0xB3, 0x87},
}

// dxgiAdapterDesc1 is DXGI_ADAPTER_DESC1 (312 bytes).
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuid           luID
	Flags                 uint32
}

// dxgiOutputDesc is DXGI_OUTPUT_DESC (96 bytes).
type dxgiOutputDesc struct {
	// DeviceName is like `\\.\DISPLAY1`.
	DeviceName         [32]uint16
	DesktopCoordinates windows.Rect
	AttachedToDesktop  int32
	Rotation           uint32
	// Monitor is an HMONITOR, the same handle EnumDisplayMonitors reports.
	Monitor uintptr
}

// dxgiAdapter is one graphics adapter.
type dxgiAdapter struct {
	// LUID identifies the adapter. The D3D11 device ffmpeg creates is bound to
	// one adapter, and only that adapter's outputs can be captured.
	LUID uint64
	// Name is the driver's description, for logging.
	Name string
	// Software is true for the software rasteriser, which cannot do Desktop
	// Duplication and which ffmpeg skips.
	Software bool
}

// dxgiOutput is one output of one adapter.
type dxgiOutput struct {
	AdapterLUID  uint64
	AdapterIndex int
	// Index is the value for ddagrab's output_idx.
	Index int
	// DeviceName is like `\\.\DISPLAY1`.
	DeviceName string
	// Desktop is the output's rectangle in virtual desktop coordinates, in the
	// orientation the user sees.
	Desktop Rect
	// Attached is false for outputs that exist but are not in use.
	Attached bool
	// Rotation in degrees: 0, 90, 180 or 270.
	Rotation int
	// Monitor is the HMONITOR this output drives.
	Monitor uintptr
}

// comObject is a raw COM interface pointer.
//
// It is deliberately not typed. COM has no room for Go's type system at this
// level: all we have is a vtable and the convention that every method takes the
// object pointer as a hidden first argument.
type comObject unsafe.Pointer

// comCall invokes a COM method.
//
// The object pointer points at the vtable pointer; the requested method is the
// vtable entry at the given slot; and the object pointer is passed again as the
// first argument, which is the hidden `this`. Nothing about the arguments is
// checked, so a wrong slot number is a crash rather than an error, which is why
// the slot constants above are named after the methods they belong to.
func comCall(obj comObject, slot int, args ...uintptr) uintptr {
	vtbl := *(**uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Add(unsafe.Pointer(vtbl), uintptr(slot)*unsafe.Sizeof(uintptr(0))))

	call := make([]uintptr, 0, len(args)+1)
	call = append(call, uintptr(obj))
	call = append(call, args...)

	r, _, _ := syscall.SyscallN(fn, call...)
	return r
}

// comRelease drops a reference, which is the only memory management COM asks of
// us: objects are reference counted, and the caller releases what it obtains.
func comRelease(obj comObject) {
	if obj != nil {
		comCall(obj, comSlotRelease)
	}
}

// hresultFailed reports whether an HRESULT is a failure.
//
// The comparison goes through uint32 because an HRESULT is a 32-bit value
// returned in a 64-bit register, and the upper half of that register is not
// promised to be anything in particular.
func hresultFailed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

func hresultError(hr uintptr, what string) error {
	code := uint32(hr)
	return fmt.Errorf("%s failed: HRESULT 0x%08X (%v)", what, code, windows.Errno(code))
}

func dxgiRotationDegrees(rotation uint32) int {
	switch rotation {
	case dxgiModeRotation90:
		return 90
	case dxgiModeRotation180:
		return 180
	case dxgiModeRotation270:
		return 270
	default:
		// IDENTITY, and UNSPECIFIED, which means no rotation is applied.
		return 0
	}
}

// enumerateDXGI lists the adapters and every output DXGI knows about.
//
// The enumeration order matters and is preserved: AdapterIndex is the position
// among adapters and Index is the position among that adapter's outputs, which
// is exactly what ffmpeg's output_idx refers to.
func enumerateDXGI() (adapters []dxgiAdapter, outputs []dxgiOutput, err error) {
	// COM apartment state belongs to an OS thread, and Go goroutines are moved
	// between threads at will. Pinning this one for the duration removes the
	// whole class of problem rather than hoping the scheduler never intervenes
	// halfway through the enumeration.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var factory comObject
	if hr, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
		uintptr(unsafe.Pointer(&factory)),
	); hresultFailed(hr) {
		return nil, nil, hresultError(hr, "CreateDXGIFactory1")
	}
	defer comRelease(factory)

	for adapterIndex := 0; ; adapterIndex++ {
		var adapter comObject
		if hr := comCall(factory, comSlotFactoryEnumAdapters1,
			uintptr(adapterIndex), uintptr(unsafe.Pointer(&adapter))); uint32(hr) == dxgiErrorNotFound {
			// Past the last adapter: this is how the enumeration ends.
			break
		} else if hresultFailed(hr) {
			if adapterIndex == 0 {
				return nil, nil, hresultError(hr, "IDXGIFactory1::EnumAdapters1")
			}
			break
		}

		var desc dxgiAdapterDesc1
		if hr := comCall(adapter, comSlotAdapterGetDesc1, uintptr(unsafe.Pointer(&desc))); hresultFailed(hr) {
			comRelease(adapter)
			return adapters, outputs, hresultError(hr, "IDXGIAdapter1::GetDesc1")
		}

		adapters = append(adapters, dxgiAdapter{
			LUID:     luidValue(desc.AdapterLuid),
			Name:     windows.UTF16ToString(desc.Description[:]),
			Software: desc.Flags&dxgiAdapterFlagSoftware != 0,
		})

		for outputIndex := 0; ; outputIndex++ {
			var output comObject
			if hr := comCall(adapter, comSlotAdapterEnumOutputs,
				uintptr(outputIndex), uintptr(unsafe.Pointer(&output))); uint32(hr) == dxgiErrorNotFound {
				break
			} else if hresultFailed(hr) {
				break
			}

			var outDesc dxgiOutputDesc
			if hr := comCall(output, comSlotOutputGetDesc, uintptr(unsafe.Pointer(&outDesc))); !hresultFailed(hr) {
				outputs = append(outputs, dxgiOutput{
					AdapterLUID:  luidValue(desc.AdapterLuid),
					AdapterIndex: adapterIndex,
					Index:        outputIndex,
					DeviceName:   windows.UTF16ToString(outDesc.DeviceName[:]),
					Desktop: Rect{
						X:      int(outDesc.DesktopCoordinates.Left),
						Y:      int(outDesc.DesktopCoordinates.Top),
						Width:  int(outDesc.DesktopCoordinates.Right - outDesc.DesktopCoordinates.Left),
						Height: int(outDesc.DesktopCoordinates.Bottom - outDesc.DesktopCoordinates.Top),
					},
					Attached: outDesc.AttachedToDesktop != 0,
					Rotation: dxgiRotationDegrees(outDesc.Rotation),
					Monitor:  outDesc.Monitor,
				})
			}
			comRelease(output)
		}

		comRelease(adapter)
	}

	if len(adapters) == 0 {
		return nil, nil, fmt.Errorf("DXGI reported no graphics adapters")
	}
	return adapters, outputs, nil
}

// captureAdapter picks the adapter ffmpeg's D3D11 device will land on.
//
// ffmpeg creates its d3d11va device from the first adapter that is not the
// software rasteriser, and only that adapter's outputs can be captured by
// ddagrab. Mirroring the choice here is what makes the output indices mean the
// same thing to both programs.
func captureAdapter(adapters []dxgiAdapter) (dxgiAdapter, []dxgiAdapter, bool) {
	var skipped []dxgiAdapter
	for _, a := range adapters {
		if a.Software {
			skipped = append(skipped, a)
			continue
		}
		return a, skipped, true
	}
	return dxgiAdapter{}, skipped, false
}
