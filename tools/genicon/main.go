// Command genicon renders Streamy's icons from the SVG sources in the repo.
//
// Rendering an SVG properly needs a real renderer, so this tool drives headless
// Chrome or Edge. Everything else — the .ico container, the PNG copies, the
// per-size downscales — is plain Go, and the generated files are committed, so a
// normal `go build` needs nothing installed.
//
// Sources:
//
//	ui/assets/app-icon.svg              — the coloured app icon
//	internal/tray/assets/icon_idle.svg  — white glyph, nothing being shared
//	internal/tray/assets/icon_live.svg  — white glyph with the red live badge
//
// Run it from the module root, then commit the result:
//
//	go run ./tools/genicon
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// traySizes are the resolutions packed into the tray .ico. Windows picks the
// closest one for the notification area, the taskbar and the Alt+Tab switcher.
var traySizes = []int{16, 32, 48}

// appSizes cover the Wails window icon, which Windows also uses for the taskbar
// and the Alt+Tab switcher, including on high-DPI displays.
var appSizes = []int{16, 32, 48, 64, 128, 256}

// output is one generated file.
type output struct {
	source string // SVG path, relative to the module root
	path   string // destination, relative to the module root
	ico    []int  // non-nil: write a multi-resolution .ico
	size   int    // non-zero: write a single square PNG
}

var outputs = []output{
	// The coloured app icon: window/taskbar icon, settings brand mark, website
	// favicon and the installer's icon.png (installer/build.ps1 copies the
	// frontend one into the staging folder).
	{source: "ui/assets/app-icon.svg", path: "ui/assets/icon_idle.ico", ico: appSizes},
	{source: "ui/assets/app-icon.svg", path: "ui/frontend/public/icon.png", size: 256},
	{source: "ui/assets/app-icon.svg", path: "../stream-site/viewer/public/favicon.png", size: 64},
	{source: "ui/assets/app-icon.svg", path: "../installer/out/stage/icon.png", size: 256},

	// Tray icons: white so they read on the dark Windows notification area.
	{source: "internal/tray/assets/icon_idle.svg", path: "internal/tray/assets/icon_idle.ico", ico: traySizes},
	{source: "internal/tray/assets/icon_idle.svg", path: "internal/tray/assets/icon_idle.png", size: 32},
	{source: "internal/tray/assets/icon_live.svg", path: "internal/tray/assets/icon_live.ico", ico: traySizes},
	{source: "internal/tray/assets/icon_live.svg", path: "internal/tray/assets/icon_live.png", size: 32},
}

func main() {
	root := flag.String("root", ".", "module root (the folder containing ui/ and internal/)")
	flag.Parse()

	browser, err := findBrowser()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genicon:", err)
		os.Exit(1)
	}

	if err := run(*root, browser); err != nil {
		fmt.Fprintln(os.Stderr, "genicon:", err)
		os.Exit(1)
	}
}

func run(root, browser string) error {
	work, err := os.MkdirTemp("", "streamy-genicon-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	fmt.Printf("genicon: rendering with %s\n", browser)

	// One render per (source, size): several outputs share the same size, so
	// this keeps the browser launches down.
	cache := map[string][]byte{}
	render := func(source string, size int) ([]byte, error) {
		key := fmt.Sprintf("%s\x00%d", source, size)
		if data, ok := cache[key]; ok {
			return data, nil
		}
		data, err := renderPNG(browser, filepath.Join(root, source), work, size)
		if err != nil {
			return nil, fmt.Errorf("render %s at %dx%d: %w", source, size, size, err)
		}
		cache[key] = data
		return data, nil
	}

	for _, item := range outputs {
		destination := filepath.Join(root, item.path)
		if _, err := os.Stat(filepath.Dir(destination)); err != nil {
			fmt.Printf("genicon: skipping %s (no %s)\n", item.path, filepath.Dir(destination))
			continue
		}

		var data []byte
		if item.ico != nil {
			images := make([]*image.NRGBA, len(item.ico))
			for i, size := range item.ico {
				raw, err := render(item.source, size)
				if err != nil {
					return err
				}
				img, err := decodePNG(raw, size)
				if err != nil {
					return fmt.Errorf("%s at %dx%d: %w", item.source, size, size, err)
				}
				images[i] = img
			}
			data = packICO(images, item.ico)
		} else {
			raw, err := render(item.source, item.size)
			if err != nil {
				return err
			}
			if _, err := decodePNG(raw, item.size); err != nil {
				return fmt.Errorf("%s at %dx%d: %w", item.source, item.size, item.size, err)
			}
			data = raw
		}

		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("genicon: wrote %s (%d bytes)\n", item.path, len(data))
	}
	return nil
}

// renderPNG screenshots the SVG at an exact size with a transparent background.
func renderPNG(browser, svgPath, work string, size int) ([]byte, error) {
	svg, err := os.ReadFile(svgPath)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(svg, []byte("<svg")) {
		return nil, errors.New("not an SVG document")
	}

	// The width/height attributes on the root element win over CSS in some
	// renderers, so the CSS !important keeps the requested size exact.
	html := fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><style>
html,body{margin:0;padding:0;background:transparent;overflow:hidden}
svg{display:block;width:%dpx!important;height:%dpx!important}
</style></head><body>%s</body></html>`, size, size, svg)

	htmlPath := filepath.Join(work, fmt.Sprintf("render-%d.html", size))
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		return nil, err
	}
	shotPath := filepath.Join(work, fmt.Sprintf("shot-%d.png", size))
	_ = os.Remove(shotPath)

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--hide-scrollbars",
		"--force-device-scale-factor=1",
		// 00000000 = fully transparent page background.
		"--default-background-color=00000000",
		fmt.Sprintf("--window-size=%d,%d", size, size),
		"--screenshot=" + shotPath,
		fileURL(htmlPath),
	}
	cmd := exec.Command(browser, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("browser exited with %w", err)
	}

	data, err := os.ReadFile(shotPath)
	if err != nil {
		return nil, fmt.Errorf("browser did not write a screenshot: %w", err)
	}
	return data, nil
}

func fileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file:///" + strings.ReplaceAll(abs, `\`, "/")
}

func decodePNG(data []byte, size int) (*image.NRGBA, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	if bounds.Dx() != size || bounds.Dy() != size {
		return nil, fmt.Errorf("rendered %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), size, size)
	}
	// Convert once, so the ICO writer gets straight (non-premultiplied) alpha
	// and anti-aliased edges keep their colour.
	nrgba := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			nrgba.Set(x, y, color.NRGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)))
		}
	}
	return nrgba, nil
}

// packICO packs one BMP-format image per size into an .ico container.
//
// The ICO file is: ICONDIR, then one 16-byte ICONDIRENTRY per image, then the
// raw payloads. Each payload is a BITMAPINFOHEADER followed by bottom-up BGRA
// rows and a 1bpp AND mask (all zeroes here — the alpha channel does the real
// transparency work).
func packICO(images []*image.NRGBA, sizes []int) []byte {
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
	for i, size := range sizes {
		buf.WriteByte(byte(size % 256)) // 0 means 256
		buf.WriteByte(byte(size % 256))
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
	return buf.Bytes()
}

func bmpPayload(img *image.NRGBA) []byte {
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
			c := img.NRGBAAt(b.Min.X+x, b.Min.Y+y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}

	// AND mask: 1 bit per pixel, each row padded to 4 bytes.
	rowBytes := ((w + 31) / 32) * 4
	buf.Write(make([]byte, rowBytes*h))

	return buf.Bytes()
}

// findBrowser locates a Chromium build that can take headless screenshots.
// STREAMY_BROWSER overrides the search.
func findBrowser() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("STREAMY_BROWSER")); explicit != "" {
		return explicit, nil
	}

	names := []string{"chrome", "google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "msedge", "microsoft-edge"}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}

	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if base == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			)
		}
	case "darwin":
		candidates = append(candidates,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	default:
		candidates = append(candidates, "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/microsoft-edge")
	}

	sort.Strings(candidates)
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("no Chrome or Edge found; install one or set STREAMY_BROWSER to its executable")
}
