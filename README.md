# Streamy — screenshare app (phase 2)

A tiny always-on background app that will let you press a global hotkey, pick a
screen or window and stream it live to anyone with a browser link — your own
replacement for Discord Go Live, free to run.

This folder is **the desktop app**. The web side (viewer page + signaling
Worker) lives in the sibling `stream-site/` folder and the two only share the
wire format documented in [`PROTOCOL.md`](PROTOCOL.md).

**Status: phase 2 of 7.** The capture pipeline is real: pressing the hotkey now
records your screen to a playable MP4 with ffmpeg, using Desktop Duplication
where the driver allows it. There is still no networking — the video goes to a
file, not to a browser — see [Roadmap](#roadmap).

---

## What works

### The skeleton (phase 1)

| Requirement | Where |
| --- | --- |
| System tray icon with a menu (Start/Stop sharing, Open settings, Copy viewer link, Quit) | `internal/tray/tray.go` |
| Global hotkey listener, configurable, no UI on press | `internal/hotkey/` |
| Tray notifications | `internal/notify/` |
| Settings as JSON in the user config directory, with defaults | `internal/config/config.go` |
| Structured logging (`log/slog`) to stderr and a file | `internal/logging/logging.go` |
| Clean shutdown: hotkey released, pipeline stopped, log flushed | `internal/app/app.go` (`Run`/`shutdown`) |
| No UI framework loaded while idle | nothing imports Wails yet |
| Only one instance at a time | `internal/singleinstance/` |
| Tray icons generated, not binary blobs | `tools/genicon/` → `internal/tray/assets/` |

### The capture pipeline (phase 2)

| Requirement | Where |
| --- | --- |
| Screen capture via Desktop Duplication (`ddagrab`), with a GDI fallback (`gdigrab`) | `internal/capture/ffmpeg.go` |
| Deciding *what* to capture, as a pure testable policy | `internal/capture/target.go` (`Decide`) |
| Window, monitor and DXGI output enumeration | `internal/capture/win_*.go` |
| ffmpeg discovery, capability probing, encoder choice | `internal/encode/` |
| The ffmpeg subprocess: arguments, graceful stop, output tail on failure | `internal/encode/runner.go` |
| The app-facing pipeline: resolve → start → record → stop | `internal/pipeline/recorder.go` |
| A debugging CLI for all of the above | `cmd/streamy-capture/` |
| Rotation (portrait monitors) handled with a `transpose` filter | `internal/capture/ffmpeg.go` |

Pressing the hotkey starts a recording and the tray says **Recording**. Pressing
it again stops, and asks ffmpeg politely (a `q` on its stdin) so the file is
finalised rather than truncated. The tray's "Copy viewer link" copies a
`file://` link to the recording that is being written.

## Requirements

- **Go 1.24 or newer** (`go version`). Developed against Go 1.27.
- No C compiler, no Node, no Python. The Windows build is pure Go, and phase 2
  added no dependencies at all — the Win32 and DXGI calls go through
  `golang.org/x/sys` and `syscall`.
- **ffmpeg**, needed from phase 2 onwards. The app finds it on `PATH`, or reads
  `encoding.ffmpegPath`. It is used only as a subprocess; nothing links against
  libav.

```powershell
winget install Gyan.FFmpeg     # or any other ffmpeg build with h264 encoders
ffmpeg -version
```

## Build and run

```powershell
cd screenshare-app

go run ./cmd/streamy            # run in the foreground; Ctrl+C to quit
go run ./cmd/streamy -headless  # no tray icon, logs everything (best for debugging)

go build -o streamy.exe ./cmd/streamy
go build -o streamy-capture.exe ./cmd/streamy-capture   # the debugging CLI (phase 2)
go test ./...
go vet ./...
```

For a build with **no console window** (what you want for daily use):

```powershell
go build -ldflags "-H=windowsgui" -o streamy.exe ./cmd/streamy
```

Flags:

| Flag | Meaning |
| --- | --- |
| `-config <path>` | Use a different config file |
| `-headless` | Run without a tray icon |
| `-log-level <level>` | Override `logLevel` from the config (`debug`, `info`, `warn`, `error`) |
| `-version` | Print the version and exit |

## Where things live

```
%AppData%\streamy\config.json    settings (hand editable)
%AppData%\streamy\streamy.log    append-only log
```

`streamy.log` is the first place to look when something behaves oddly — every
decision the app makes is logged there.

## Recording

Press the hotkey and the app writes an H.264 MP4 to:

```
%UserProfile%\Videos\Streamy\streamy-2026-01-02_15-04-05.mp4
```

Recordings go in **Videos**, not next to the config file, because they are yours
and not the app's. The name is a local timestamp so a folder of them sorts in
order. `-recorder` in the debug CLI chooses this directory too.

Stopping is a `q` on ffmpeg's stdin rather than a kill, because that is what
makes ffmpeg write the MP4 index and leave a file that plays. Hitting Stop a
second time is safe; so is quitting the app mid-recording.

The tray follows the pipeline's own word for the session: the status line reads
**Recording** and the menu offers **Start recording** / **Stop recording**,
rather than claiming to share video that is being written to a file. Phase 3
only has to change the session label to `Streaming` for the same code to say
*streaming*.

## How the app picks what to capture

Pressing the hotkey runs one policy, in order:

1. **A fullscreen window** — a game, a video player, a presentation → capture the
   **monitor that window is on**, not the window itself. Capture the whole screen
   and everything the GPU draws ends up in the video; capture a window by title
   and exclusive-fullscreen games record a black frame.
2. **Otherwise** → `capture.defaultTarget`: `primary`, `mouse`, `focused`, or
   `monitor` (with `capture.monitorIndex`).

The choice and the reason are logged every single time:

```
capture target chosen target="monitor 2 (1920x1080+0+0, dxgi output 0)"
  reason="capture.defaultTarget is \"primary\" (and no fullscreen window was found)"
  grabber=ddagrab encoder=h264_nvenc
```

Two parts of that are worth knowing about.

**"Fullscreen" means fullscreen.** A maximised window stops at the taskbar, and
recording a window that merely fills most of a screen is not what anyone means by
fullscreen capture, so it is not treated as one. The tray, the desktop (Progman),
Explorer's shell windows, and the app's own windows are never capturable.

**Which screen is screen 0?** Windows, ffmpeg and the graphics driver each number
the screens differently, and only one of those numbers is the one ffmpeg's
`ddagrab` filter wants: the DXGI output index. Getting this wrong records the
wrong monitor, so it is not guessed — the monitor and the output are matched
through the `HMONITOR` handle that both APIs report, and that handle identifies
the screen exactly. `capture/win_dxgi.go` explains why the obvious alternative
(the `id` field in the Windows display configuration API) does not work: it is a
driver-assigned number — 33024, 33025, 33029 on the machine this was built on —
and has nothing to do with an output index. Run `streamy-capture -list-monitors`
to see the mapping.

## The debugging CLI: `streamy-capture`

`cmd/streamy-capture` drives the capture and encode packages directly, with a
flag for every knob the app decides for you. Nothing about it is needed to run
the app; it exists because "the video is wrong" is not a useful bug report.

```powershell
go run ./cmd/streamy-capture -list-monitors            # monitors, each with its DXGI index
go run ./cmd/streamy-capture -list-windows             # what a picker would offer
go run ./cmd/streamy-capture -list-paths               # raw display paths (diagnostics)
go run ./cmd/streamy-capture -encoders                 # probe every H.264 encoder
go run ./cmd/streamy-capture -print-command            # print the ffmpeg command and stop
go run ./cmd/streamy-capture -duration 10s -o out.mp4
go run ./cmd/streamy-capture -duration 10s -encoder x264 -fps 15
go run ./cmd/streamy-capture -monitor 1 -duration 5s   # a particular screen
go run ./cmd/streamy-capture -window "Firefox" -duration 5s
go run ./cmd/streamy-capture -recorder -duration 5s    # the exact path the tray app uses
```

`-print-command` is the quickest answer to "what is ffmpeg actually being asked
to do", and what it prints can be pasted into a terminal unchanged.

| Flag | Meaning |
| --- | --- |
| `-duration <d>` | Stop after `d` (default `5s`; `0` means until Ctrl+C) |
| `-o <path>` | Where to write (default: a temporary file) |
| `-stdout` | Write the encoded stream to stdout instead of a file |
| `-format <f>` | `mp4` (default), `mkv`, `mp4-frag`, `h264` |
| `-encoder <e>` | `auto` (default), `nvenc`, `qsv`, `amf`, `x264` (ffmpeg's `libx264`, software) |
| `-grabber <g>` | `auto` (default), `ddagrab`, `gdigrab` |
| `-monitor <n>` | Capture monitor `n` instead of auto-selecting |
| `-window <text>` | Capture the window whose title contains `text` |
| `-target` | Print what would be captured and why, then exit |
| `-recorder` | Go through `pipeline.Recorder`, the path the tray app uses |
| `-fps`, `-bitrate`, `-scale` | Override the encoding settings |
| `-mouse` | Draw the pointer into the capture (default true) |
| `-titles` | Print window titles in `-list-windows` (off by default) |
| `-v` | Show ffmpeg's own diagnostics |

With `-stdout` the encoded video owns stdout and every progress line moves to
stderr, so the two can be piped separately:

```powershell
cmd /c "streamy-capture.exe -stdout -duration 5s -format h264 > stream.h264 2> log.txt"
```

`-list-windows` prints window titles only when `-titles` is given. Titles are
whatever you happen to have open, and this command's output tends to end up
pasted into a bug report; the default output is process ids, window classes and
rectangles, which is enough to diagnose a capture problem.

## Settings

Everything is JSON with sensible defaults; the file is created on first run. The
`capture` and `encoding` blocks are read by phase 2; `stream` and `signaling`
are read from phase 3 onwards.

```json
{
  "schema": 1,
  "logLevel": "info",
  "hotkey": {
    "enabled": true,
    "toggle": "Ctrl+Shift+F9",
    "picker": "Ctrl+Shift+F10"
  },
  "capture": {
    "defaultTarget": "primary",
    "monitorIndex": 0,
    "alwaysShowPicker": false,
    "autoSwitchTarget": false,
    "captureCursor": true
  },
  "encoding": {
    "encoder": "auto",
    "ffmpegPath": "",
    "resolution": "source",
    "fps": 30,
    "bitrateKbps": 6000,
    "keyframeSeconds": 1,
    "encoderThreads": 0,
    "extraEncoderArgs": null
  },
  "stream": {
    "maxViewers": 10,
    "stunServers": ["stun:stun.l.google.com:19302"],
    "turnServers": null
  },
  "signaling": { "url": "" }
}
```

Notes:

- **Hotkeys** need at least one modifier plus one key, e.g. `Ctrl+Shift+F9`,
  `Win+Alt+S`, `Ctrl+Space`. Spelling and order are free: `control + shift + f9`
  works and is normalised to `Ctrl+Shift+F9`. Run keys `F1`–`F20`, letters,
  digits, arrows, `Space`, `Enter`, `Esc`, `Tab`, `Delete` and the media keys
  are all understood.
- **A bad value stops the app at startup with a message** instead of failing
  halfway through a stream. That is `Config.Validate()`.
- **A UTF-8 byte order mark is tolerated.** PowerShell's `Set-Content
  -Encoding utf8` and several editors add one, and JSON itself does not allow
  one, so without this a perfectly good config file would be silently replaced
  by the defaults. The app never writes one itself.
- Old config files keep working: a key that is missing falls back to its default
  (see `internal/config/config.go` for the trick).

## Testing

```powershell
go test ./...          # unit tests
go test -v ./internal/hotkey/            # just the hotkey parser
go test -v ./internal/capture/           # the target policy and the DXGI join
go test -v ./internal/encode/            # ffmpeg arguments
go test -v ./internal/pipeline/          # the app-facing pipeline
```

The clipboard is a shared, briefly-locked OS resource, so `internal/app` takes a
`copyToClipboard` function that tests replace — that way running the tests never
overwrites your real clipboard.

The capture tests are written to run **without a screen**. Anything that would
need a real monitor, a real GPU or a real ffmpeg process is kept out of the
testable function: `capture.Decide` is a pure policy over a struct,
`capture.joinMonitors` takes the monitor list and the graphics outputs as
arguments, and `encode.Args` only builds a slice of strings. The one thing that
cannot be faked is the native struct layout, which is why there is a test that
asserts byte offsets — a wrong offset there does not fail a test, it writes into
the wrong memory.

The race detector (`go test -race`) needs cgo, and therefore a C compiler, which
is not installed here (`CGO_ENABLED=0`). To enable it:

```powershell
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e   # MinGW-w64, provides gcc
$env:Path += ";C:\Program Files\WinLibs\...\bin"          # wherever it landed
$env:CGO_ENABLED=1; go test -race ./...
```

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| "Hotkey unavailable" notification | Another app already owns that combination. `RegisterHotKey` is first-come-first-served, so change `hotkey.toggle` in the config and restart. The tray menu still works. |
| No tray icon at all | Windows 11 hides new icons by default: click the `^` chevron in the taskbar and drag Streamy onto the bar. Check `streamy.log` for a startup error. |
| Second launch exits immediately | By design — one instance only, so the hotkey and the icon are not registered twice. Look for the existing icon. |
| Pressing the hotkey does nothing | It should pop a notification. If not, run with `-log-level debug` and watch `streamy.log`. Note the shortcut fires on **key release**, not key press. |
| "Could not copy link" | Another app held the clipboard lock; the app still starts sharing. Press "Copy viewer link" again from the tray menu. |
| Antivirus flags the exe | Unsigned Go binaries are frequently flagged when built with `-H=windowsgui`. Build without that flag, or add an exclusion. |
| Log file grows forever | Not handled yet; log rotation is on the phase 7 list. |
| The recording is a black screen | `gdigrab` cannot see exclusive-fullscreen games. Grep the log for "Desktop Duplication (ddagrab) is unavailable": if it is there, Desktop Duplication is switched off — a remote-desktop or screen-locking tool can do that, and it comes back when that tool is closed. |
| The wrong screen was recorded | Read the `capture target chosen` line in the log; it names the monitor and says why. `streamy-capture -list-monitors` shows the monitor → DXGI index mapping. To pin it, set `capture.defaultTarget` to `monitor` and `capture.monitorIndex`. |
| The video is sideways | A rotated (portrait) display. The app rotates the capture from Desktop Duplication itself and leaves GDI alone, because GDI already receives a rotated desktop. `streamy-capture -list-monitors` prints the rotation it detected. |
| "ffmpeg not found" | Install it (`winget install Gyan.FFmpeg`) or put the full path in `encoding.ffmpegPath`. Leaving that empty is fine as long as ffmpeg is on `PATH`. |
| `h264_nvenc` (or `qsv`/`amf`) fails | Encoder probing tries candidates in order and reports what worked. Run `streamy-capture -encoders` to see which encoders this machine accepts, then pin one with `encoding.encoder` (`x264` always works and costs CPU). |
| Recording stops by itself after ~5 seconds | You are running `streamy-capture`, whose default `-duration` is `5s`. The tray app records until you press the hotkey again. |
| The file is huge / the machine stutters | Lower `encoding.bitrateKbps`, `encoding.fps`, or set `encoding.resolution` (e.g. `1280x720`). Software encoding at 1080p60 is expensive; a hardware encoder is the real fix. |
| The MP4 will not open, or is 0 bytes | Something killed ffmpeg instead of asking it to stop. The app sends `q` on stdin and waits, and only kills as a last resort, so in the worst case the last second is lost. |
| My settings are being ignored | The log says `could not read the config, using the defaults`, which means the file did not parse (and the reason is on that line). A UTF-8 byte order mark is already tolerated; a trailing comma is not. |
| `-stdout` produces a "corrupt" file | The text progress lines went into the byte stream. They should not: with `-stdout` every message moves to stderr. Redirect the two separately (see the `cmd /c` line above) — PowerShell 5.1's pipeline decodes bytes as text and will corrupt a binary stream. |

Windows-specific things that will need attention later: a firewall prompt
appears the first time the app opens a listening socket, and TURN may be
required behind symmetric NAT (phase 5).

## Roadmap

| Phase | Contents | State |
| --- | --- | --- |
| 1 | Tray, hotkey, config, logging, clean shutdown | **done** |
| 2 | ffmpeg capture → file, encoder auto-detection, target policy | **done** |
| 3 | pion WebRTC to a test viewer page | next |
| 4 | Cloudflare Worker + Durable Object signaling | |
| 5 | Multi-viewer, keyframe handling, viewer limits | |
| 6 | Wails + Svelte picker and settings, launched from the tray | |
| 7 | Reconnect logic, README polish, build script | |

Phase 2 stops where it has to: the encoded stream ends up in a file, and the
recorder can also hand that stream to an `io.Writer` instead (that is what
`streamy-capture -stdout` exercises). Phase 3 replaces the writer with a WebRTC
track, which is why `internal/pipeline` owns the `Streamer` interface that
`internal/app` depends on — the tray, the hotkey and the shutdown code did not
have to change to gain capture, and will not have to change to gain streaming.

## Platform notes

Windows is the only target so far. Every platform-specific piece is isolated
behind an interface or a build tag so that macOS and Linux can be added without
touching the rest:

- `internal/clipboard/clipboard_windows.go` + `_other.go`
- `internal/singleinstance/singleinstance_windows.go` + `_other.go`
- `internal/notify/notify_windows.go` + `_other.go`
- `internal/tray/icon.go` picks `.ico` or `.png` by `runtime.GOOS`
- `internal/hotkey` wraps `golang.design/x/hotkey`, which is itself cross-platform
- `internal/capture/win_api.go`, `win_display.go`, `win_window.go`, `win_dxgi.go`
  are the Windows half of capture, with `resolver_windows.go` / `resolver_other.go`
  choosing between them at build time. The policy (`target.go`) and the ffmpeg
  arguments (`ffmpeg.go`) underneath are portable and know nothing about Win32.

Known gaps on non-Windows builds today: the clipboard and balloon notifications
are Windows-only, capture is Windows-only (`-list-monitors` reports that plainly
rather than failing oddly), and `fyne.io/systray` needs GTK headers (cgo) on
Linux.

## Dependencies

Kept deliberately small. Each one, and why:

| Module | Why |
| --- | --- |
| `fyne.io/systray` | The tray icon and menu. On Windows it is pure Go (Win32 syscalls, no cgo) and it owns the message loop for us. |
| `golang.design/x/hotkey` | Global shortcuts via `RegisterHotKey` on Windows. No keyboard hook, no polling. |
| `golang.org/x/sys` | The Win32 bindings we need directly: clipboard, single-instance mutex, tray balloons, monitor and window enumeration, display configuration. |

Phase 2 added no dependencies. Desktop Duplication, DXGI enumeration and the
display configuration API are all reached through `syscall.SyscallN` and the
handful of bindings in `internal/capture/win_api.go` and `win_dxgi.go`, which is
why the app still starts without any runtime it does not control besides ffmpeg.

No Electron, no bundled Chromium, no UI toolkit loaded while idle.
