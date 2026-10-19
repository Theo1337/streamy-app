# ui/ (phase 6)

The on-demand settings and picker window: a Wails application (Go backend plus a
webview) with a Svelte frontend.

It is a **separate process** that the tray launches when you open settings, and it
exits when you close it. That keeps the always-on tray process free of any UI
framework, which is a hard requirement: it must idle at a few megabytes of RAM.

## Planned contents

| Path | Purpose |
| --- | --- |
| `main.go` | Wails entry point: window setup, bindings |
| `app.go` | The bridge: reads and writes the config, hands capture targets to the tray |
| `frontend/` | Svelte app: the screen/window picker with live thumbnails, and the settings form |
| `frontend/src/lib/ipc.js` | The client side of the tray ↔ UI channel (named pipe or localhost WebSocket) |
| `wails.json` | Wails project configuration |

## Planned build

```powershell
cd ui
wails build          # produces streamy-ui.exe next to the tray's streamy.exe
```

`internal/app/app.go` already looks for `streamy-ui.exe` beside the tray binary
and launches it when available, falling back to a notification that explains the
window is not built yet. So the wiring is in place; only the UI process itself is
missing.

Nothing is implemented yet. This placeholder exists so the folder layout matches
the project plan from the start.
