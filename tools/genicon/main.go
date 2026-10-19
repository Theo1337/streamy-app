// Command genicon renders the tray icons used by Streamy.
//
// It draws the icons with plain image/draw maths instead of shipping binary
// blobs we cannot inspect, and writes two files per state:
//
//	icon_idle.ico / icon_idle.png   — shown when nothing is being shared
//	icon_live.ico / icon_live.png   — shown while a stream is running
//
// .ico is what Windows wants (fyne.io/systray turns the bytes into a temp file
// and loads it with LoadImage), .png is what macOS and Linux want later on.
//
// Run it from the module root, then commit the result:
//
//	go run ./tools/genicon
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// sizes are the resolutions packed into the .ico. Windows picks the closest one
// for the notification area, the taskbar and the Alt+Tab switcher.
var sizes = []int{16, 32, 48}

// supersampling factor: we draw at 4x and box-filter down, which is a cheap way
// to get anti-aliased edges without a 2D graphics library.
const supersample = 4

type rgb struct{ r, g, b uint8 }

var (
	bgIdle = rgb{0x4F, 0x46, 0xE5} // indigo: idle
	bgLive = rgb{0xDC, 0x26, 0x26} // red: live
	white  = rgb{0xFF, 0xFF, 0xFF}
)

func main() {
	out := flag.String("out", filepath.Join("internal", "tray", "assets"), "output directory")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "genicon:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, state := range []struct {
		name string
		bg   rgb
		live bool
	}{
		{"idle", bgIdle, false},
		{"live", bgLive, true},
	} {
		ico, err := renderICO(state.bg, state.live, sizes)
		if err != nil {
			return err
		}
		icoPath := filepath.Join(dir, "icon_"+state.name+".ico")
		if err := os.WriteFile(icoPath, ico, 0o644); err != nil {
			return err
		}

		pngPath := filepath.Join(dir, "icon_"+state.name+".png")
		if err := writePNG(pngPath, render(32, state.bg, state.live)); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes) and %s\n", icoPath, len(ico), pngPath)
	}
	return nil
}

// render draws one square icon at the given size.
func render(size int, bg rgb, live bool) *image.RGBA {
	w := size * supersample

	// Each supersampled pixel keeps straight (non premultiplied) colour plus
	// coverage in alpha, so the box filter below can average correctly.
	type pixel struct {
		r, g, b uint8
		a       uint8
	}
	src := make([]pixel, w*w)

	set := func(x, y int, c rgb) {
		if x < 0 || y < 0 || x >= w || y >= w {
			return
		}
		src[y*w+x] = pixel{c.r, c.g, c.b, 0xFF}
	}

	radius := 0.22 * float64(w)
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			cx, cy := float64(x)+0.5, float64(y)+0.5
			if !inRoundedRect(cx, cy, 0, 0, float64(w), float64(w), radius) {
				continue // transparent corner
			}
			set(x, y, bg)
			if inGlyph(cx, cy, float64(w), live) {
				set(x, y, white)
			}
		}
	}

	// Box filter w -> size.
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	n := float64(supersample * supersample)
	for oy := 0; oy < size; oy++ {
		for ox := 0; ox < size; ox++ {
			var ar, ag, ab, aSum float64
			for y := oy * supersample; y < (oy+1)*supersample; y++ {
				for x := ox * supersample; x < (ox+1)*supersample; x++ {
					p := src[y*w+x]
					aw := float64(p.a) / 255
					ar += float64(p.r) * aw
					ag += float64(p.g) * aw
					ab += float64(p.b) * aw
					aSum += aw
				}
			}
			alpha := aSum / n
			var c color.RGBA
			if aSum > 0 {
				c = color.RGBA{
					R: clamp8(ar / aSum),
					G: clamp8(ag / aSum),
					B: clamp8(ab / aSum),
					A: clamp8(alpha * 255),
				}
			}
			dst.SetRGBA(ox, oy, c)
		}
	}
	return dst
}

// inGlyph reports whether the point (in supersampled pixels, s is the icon
// width) belongs to the white monitor glyph. The "live" variant adds a filled
// dot in the middle of the screen.
func inGlyph(cx, cy, s float64, live bool) bool {
	// Normalised coordinates make the fractions below readable.
	x, y := cx/s, cy/s

	// Screen: an outlined rounded rectangle.
	const (
		x0, y0, x1, y1 = 0.20, 0.24, 0.80, 0.64
		border         = 0.075
	)
	if inRoundedRectFrac(x, y, x0, y0, x1, y1, 0.06) &&
		!inRoundedRectFrac(x, y, x0+border, y0+border, x1-border, y1-border, 0.03) {
		return true
	}

	// Stand.
	if inRectFrac(x, y, 0.44, y1, 0.56, 0.70) {
		return true
	}
	if inRectFrac(x, y, 0.33, 0.70, 0.67, 0.76) {
		return true
	}

	if live {
		// Broadcast dot, snug inside the screen.
		dx, dy := x-0.50, y-0.44
		if dx*dx+dy*dy <= 0.13*0.13 {
			return true
		}
	}
	return false
}

func inRectFrac(x, y, x0, y0, x1, y1 float64) bool {
	return x >= x0 && x <= x1 && y >= y0 && y <= y1
}

func inRoundedRectFrac(x, y, x0, y0, x1, y1, r float64) bool {
	return inRoundedRect(x, y, x0, y0, x1, y1, r)
}

// inRoundedRect is the classic "point in rounded rectangle" test: inside the
// inner rectangle, or within r of one of the four corner centres.
func inRoundedRect(px, py, x0, y0, x1, y1, r float64) bool {
	if px < x0 || px > x1 || py < y0 || py > y1 {
		return false
	}
	if r <= 0 {
		return true
	}
	ix0, iy0, ix1, iy1 := x0+r, y0+r, x1-r, y1-r
	// Straight edges.
	if px >= ix0 && px <= ix1 {
		return true
	}
	if py >= iy0 && py <= iy1 {
		return true
	}
	// Corners: nearest centre.
	cx := ix0
	if px > ix1 {
		cx = ix1
	}
	cy := iy0
	if py > iy1 {
		cy = iy1
	}
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}

func writePNG(path string, img *image.RGBA) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// renderICO packs one BMP-format image per size into an .ico container.
//
// The ICO file is: ICONDIR, then one 16-byte ICONDIRENTRY per image, then the
// raw payloads. Each payload is a BITMAPINFOHEADER followed by bottom-up BGRA
// rows and a 1bpp AND mask (all zeroes here — the alpha channel does the real
// transparency work).
func renderICO(bg rgb, live bool, sizes []int) ([]byte, error) {
	images := make([]*image.RGBA, len(sizes))
	for i, s := range sizes {
		images[i] = render(s, bg, live)
	}

	payloads := make([][]byte, len(images))
	for i, img := range images {
		payloads[i] = bmpPayload(img)
	}

	var buf bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			panic(err) // bytes.Buffer never fails
		}
	}

	// ICONDIR
	write(uint16(0)) // reserved
	write(uint16(1)) // type: icon
	write(uint16(len(images)))

	// ICONDIRENTRY
	offset := 6 + 16*len(images)
	for i, img := range images {
		w := img.Bounds().Dx()
		h := img.Bounds().Dy()
		buf.WriteByte(byte(w % 256)) // 0 means 256
		buf.WriteByte(byte(h % 256))
		buf.WriteByte(0)  // palette size
		buf.WriteByte(0)  // reserved
		write(uint16(1))  // colour planes
		write(uint16(32)) // bits per pixel
		write(uint32(len(payloads[i])))
		write(uint32(offset))
		offset += len(payloads[i])
	}

	for _, p := range payloads {
		buf.Write(p)
	}
	return buf.Bytes(), nil
}

func bmpPayload(img *image.RGBA) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	var buf bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}

	// BITMAPINFOHEADER. biHeight is doubled because the bitmap stores the XOR
	// image and the AND mask one after the other.
	write(uint32(40))        // biSize
	write(int32(w))          // biWidth
	write(int32(h * 2))      // biHeight
	write(uint16(1))         // biPlanes
	write(uint16(32))        // biBitCount
	write(uint32(0))         // biCompression: BI_RGB
	write(uint32(w * h * 4)) // biSizeImage
	write(int32(0))          // biXPelsPerMeter
	write(int32(0))          // biYPelsPerMeter
	write(uint32(0))         // biClrUsed
	write(uint32(0))         // biClrImportant

	// XOR image: BGRA, bottom row first.
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}

	// AND mask: 1 bit per pixel, each row padded to 4 bytes.
	rowBytes := ((w + 31) / 32) * 4
	buf.Write(make([]byte, rowBytes*h))

	return buf.Bytes()
}
