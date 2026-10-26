//go:build windows

package capture

import (
	"bytes"
	"image"
	"image/png"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowIcon returns a small PNG of the window's application icon. The picker
// uses it in the Windows tab so a list of documents is easier to scan than a
// column of identical first letters.
//
// WM_GETICON is the cheapest source and gives the exact icon the window uses.
// Many apps return 0 there, so we also try the class icon and finally the icon
// embedded in the process executable.
func WindowIcon(hwnd uintptr) []byte {
	icon, owned := windowIconHandle(hwnd)
	if icon == 0 {
		return nil
	}
	if owned {
		defer procDestroyIcon.Call(icon)
	}
	data, err := iconPNG(windows.Handle(icon))
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

var (
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
	procGetClassLongPtrW    = user32.NewProc("GetClassLongPtrW")
	procGetClassLongW       = user32.NewProc("GetClassLongW")
	procGetIconInfo         = user32.NewProc("GetIconInfo")
	procDestroyIcon         = user32.NewProc("DestroyIcon")

	procGetObjectW     = gdi32.NewProc("GetObjectW")
	procExtractIconExW = shell32.NewProc("ExtractIconExW")
)

var shell32 = windows.NewLazySystemDLL("shell32.dll")

const (
	wmGetIcon        = 0x007F
	iconSmall        = 0
	iconBig          = 1
	iconSmall2       = 2
	smtoBlock        = 0x0001
	smtoAbortIfHung  = 0x0002
	gclpHicon        = ^uintptr(13) // -14, as an unsigned pointer-sized value
	gclpHiconSmall   = ^uintptr(33) // -34
	processQueryInfo = 0x1000       // PROCESS_QUERY_LIMITED_INFORMATION
)

func windowIconHandle(hwnd uintptr) (uintptr, bool) {
	for _, kind := range []uintptr{iconBig, iconSmall2, iconSmall} {
		var result uintptr
		ok, _, _ := procSendMessageTimeoutW.Call(
			hwnd, wmGetIcon, kind, 0, smtoAbortIfHung|smtoBlock, 150,
			uintptr(unsafe.Pointer(&result)),
		)
		if ok != 0 && result != 0 {
			return result, false
		}
	}

	classIcon := func(index uintptr) uintptr {
		if err := procGetClassLongPtrW.Find(); err == nil {
			value, _, _ := procGetClassLongPtrW.Call(hwnd, index)
			return value
		}
		value, _, _ := procGetClassLongW.Call(hwnd, index)
		return value
	}
	if icon := classIcon(gclpHicon); icon != 0 {
		return icon, false
	}
	if icon := classIcon(gclpHiconSmall); icon != 0 {
		return icon, false
	}

	return processIconHandle(hwnd), true
}

func processIconHandle(hwnd uintptr) uintptr {
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil || pid == 0 {
		return 0
	}
	process, err := windows.OpenProcess(processQueryInfo, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(process)

	buffer := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return 0
	}
	path, err := windows.UTF16PtrFromString(windows.UTF16ToString(buffer[:size]))
	if err != nil {
		return 0
	}
	var large, small uintptr
	count, _, _ := procExtractIconExW.Call(
		uintptr(unsafe.Pointer(path)), 0,
		uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1,
	)
	if count == 0 {
		return 0
	}
	if large != 0 {
		if small != 0 {
			procDestroyIcon.Call(small)
		}
		return large
	}
	return small
}

type winIconInfo struct {
	IsIcon   uint32
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

type winBitmap struct {
	Type       uint32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

func iconPNG(icon windows.Handle) ([]byte, error) {
	var info winIconInfo
	ok, _, _ := procGetIconInfo.Call(uintptr(icon), uintptr(unsafe.Pointer(&info)))
	if ok == 0 || info.Color == 0 {
		if info.Mask != 0 {
			procDeleteObject.Call(uintptr(info.Mask))
		}
		return nil, nil
	}
	if info.Mask != 0 {
		defer procDeleteObject.Call(uintptr(info.Mask))
	}
	defer procDeleteObject.Call(uintptr(info.Color))

	var bitmap winBitmap
	size, _, _ := procGetObjectW.Call(
		uintptr(info.Color), unsafe.Sizeof(bitmap), uintptr(unsafe.Pointer(&bitmap)),
	)
	if size == 0 || bitmap.Width <= 0 || bitmap.Height <= 0 {
		return nil, nil
	}
	width, height := int(bitmap.Width), int(bitmap.Height)
	if width > 1024 || height > 1024 || width <= 0 || height <= 0 {
		return nil, nil
	}

	screenDC, _, _ := user32.NewProc("GetDC").Call(0)
	if screenDC == 0 {
		return nil, nil
	}
	defer user32.NewProc("ReleaseDC").Call(0, screenDC)

	pixels := make([]byte, width*height*4)
	header := previewBitmapInfo{
		Header: previewBitmapInfoHeader{
			Size:  uint32(unsafe.Sizeof(previewBitmapInfoHeader{})),
			Width: int32(width), Height: -int32(height),
			Planes: 1, BitCount: 32, Compression: biRGB,
		},
	}
	copied, _, _ := procGetDIBits.Call(
		screenDC, uintptr(info.Color), 0, uintptr(height),
		uintptr(unsafe.Pointer(&pixels[0])), uintptr(unsafe.Pointer(&header)), dibRGBColors,
	)
	if copied == 0 {
		return nil, nil
	}

	hasAlpha := false
	for i := 3; i < len(pixels); i += 4 {
		if pixels[i] != 0 {
			hasAlpha = true
			break
		}
	}

	var mask []byte
	maskStride := ((width + 31) / 32) * 4
	if !hasAlpha && info.Mask != 0 {
		mask = make([]byte, maskStride*height)
		maskHeader := previewBitmapInfo{
			Header: previewBitmapInfoHeader{
				Size:  uint32(unsafe.Sizeof(previewBitmapInfoHeader{})),
				Width: int32(width), Height: -int32(height),
				Planes: 1, BitCount: 1, Compression: biRGB,
			},
		}
		procGetDIBits.Call(
			screenDC, uintptr(info.Mask), 0, uintptr(height),
			uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&maskHeader)), dibRGBColors,
		)
	}

	imageData := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src := (y*width + x) * 4
			dst := y*imageData.Stride + x*4
			alpha := pixels[src+3]
			if !hasAlpha {
				alpha = 0xFF
				if mask != nil {
					byteIndex := y*maskStride + x/8
					if byteIndex < len(mask) && mask[byteIndex]&(0x80>>uint(x%8)) != 0 {
						alpha = 0
					}
				}
			}
			imageData.Pix[dst+0] = pixels[src+2] // R
			imageData.Pix[dst+1] = pixels[src+1] // G
			imageData.Pix[dst+2] = pixels[src+0] // B
			imageData.Pix[dst+3] = alpha
		}
	}

	imageData = shrinkIcon(imageData, 64)
	var out bytes.Buffer
	if err := png.Encode(&out, imageData); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// shrinkIcon keeps the payload small without pulling in a resampling package.
// The picker renders the icon at 18px, so nearest-neighbour is invisible there.
func shrinkIcon(src *image.NRGBA, max int) *image.NRGBA {
	width, height := src.Rect.Dx(), src.Rect.Dy()
	if width <= max && height <= max {
		return src
	}
	scale := float64(max) / float64(width)
	if height > width {
		scale = float64(max) / float64(height)
	}
	dstWidth, dstHeight := int(float64(width)*scale), int(float64(height)*scale)
	if dstWidth < 1 {
		dstWidth = 1
	}
	if dstHeight < 1 {
		dstHeight = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	for y := 0; y < dstHeight; y++ {
		for x := 0; x < dstWidth; x++ {
			sx := x * width / dstWidth
			sy := y * height / dstHeight
			srcOffset := sy*src.Stride + sx*4
			dstOffset := y*dst.Stride + x*4
			copy(dst.Pix[dstOffset:dstOffset+4], src.Pix[srcOffset:srcOffset+4])
		}
	}
	return dst
}
