# Streamy — screenshare app (phase 1)

A tiny always-on background app that will let you press a global hotkey, pick a
screen or window and stream it live to anyone with a browser link — your own
replacement for Discord Go Live, free to run.

This folder is **the desktop app**. The web side (viewer page + signaling
Worker) lives in the sibling `stream-site/` folder and the two only share the
wire format documented in [`PROTOCOL.md`](PROTOCOL.md).

**Status: phase 1 of 7.** What exists right now is the skeleton: tray icon,
global hotkey, config loading, logging and clean shutdown. There is no capture
and no video yet — see [Roadmap](#roadmap).

---

## What phase 1 does

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

Pressing the hotkey (default `Ctrl+Shift+F9`) currently toggles a *simulated*
session and pops a notification saying the capture pipeline arrives in phase 2.
That notification is the point: seeing it proves the hotkey, the app wiring and
the tray all work.

## Requirements

- **Go 1.24 or newer** (`go version`). Developed against Go 1.27.
- No C compiler, no Node, no Python. The Windows build is pure Go.
- ffmpeg is only needed from phase 2 onwards (already installed here:
  `ffmpeg -version` works).

## Build and run

```powershell
cd screenshare-app

go run ./cmd/streamy            # run in the foreground; Ctrl+C to quit
go run ./cmd/streamy -headless  # no tray icon, logs everything (best for debugging)

go build -o streamy.exe ./cmd/streamy
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

## Settings

Everything is JSON with sensible defaults; the file is created on first run. All
of these keys exist already so phase 2 only has to start reading them.

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
- Old config files keep working: a key that is missing falls back to its default
  (see `internal/config/config.go` for the trick).

## Testing

```powershell
go test ./...          # unit tests
go test -v ./internal/hotkey/   # just the hotkey parser
```

The clipboard is a shared, briefly-locked OS resource, so `internal/app` takes a
`copyToClipboard` function that tests replace — that way running the tests never
overwrites your real clipboard.

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

Windows-specific things that will need attention later: a firewall prompt
appears the first time the app opens a listening socket, and TURN may be
required behind symmetric NAT (phase 5).

## Roadmap

| Phase | Contents | State |
| --- | --- | --- |
| 1 | Tray, hotkey, config, logging, clean shutdown | **done** |
| 2 | ffmpeg capture → stdout, encoder auto-detection | next |
| 3 | pion WebRTC to a test viewer page | |
| 4 | Cloudflare Worker + Durable Object signaling | |
| 5 | Multi-viewer, keyframe handling, viewer limits | |
| 6 | Wails + Svelte picker and settings, launched from the tray | |
| 7 | Reconnect logic, README polish, build script | |

`internal/app/streamer.go` already declares the `Streamer` interface that phase 2
implements, and `internal/app/app.go` already calls it — so the tray, hotkey and
shutdown code will not need to change.

## Platform notes

Windows is the only target so far. Every platform-specific piece is isolated
behind an interface or a build tag so that macOS and Linux can be added without
touching the rest:

- `internal/clipboard/clipboard_windows.go` + `_other.go`
- `internal/singleinstance/singleinstance_windows.go` + `_other.go`
- `internal/notify/notify_windows.go` + `_other.go`
- `internal/tray/icon.go` picks `.ico` or `.png` by `runtime.GOOS`
- `internal/hotkey` wraps `golang.design/x/hotkey`, which is itself cross-platform

Known gaps on non-Windows builds today: the clipboard and balloon notifications
are Windows-only, and `fyne.io/systray` needs GTK headers (cgo) on Linux.

## Dependencies

Kept deliberately small. Each one, and why:

| Module | Why |
| --- | --- |
| `fyne.io/systray` | The tray icon and menu. On Windows it is pure Go (Win32 syscalls, no cgo) and it owns the message loop for us. |
| `golang.design/x/hotkey` | Global shortcuts via `RegisterHotKey` on Windows. No keyboard hook, no polling. |
| `golang.org/x/sys` | The Win32 bindings we need directly: clipboard, single-instance mutex, tray balloons. |

No Electron, no bundled Chromium, no UI toolkit loaded while idle.
