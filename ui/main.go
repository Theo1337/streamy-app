package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed frontend/dist
var frontend embed.FS

func main() {
	address := flag.String("ipc-address", "", "authenticated tray IPC address")
	token := flag.String("ipc-token", "", "one-time tray IPC token")
	view := flag.String("view", "settings", "window to show: settings or share")
	flag.Parse()
	if *address == "" || *token == "" {
		log.Fatal("Streamy UI must be launched by the tray process")
	}
	if *view != "share" {
		*view = "settings"
	}
	assets, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		log.Fatal(fmt.Errorf("load embedded frontend: %w", err))
	}
	app := NewSettingsApp(*address, *token, *view)
	title := "Streamy settings"
	width := 980
	height := 760
	if *view == "share" {
		title = "Streamy — Share your screen"
		width = 1100
		height = 780
	}
	err = wails.Run(&options.App{
		Title:            title,
		Width:            width,
		Height:           height,
		MinWidth:         720,
		MinHeight:        560,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 17, G: 24, B: 39, A: 1},
		WindowStartState: options.Normal,
		DisableResize:    false,
		Frameless:        false,
		DragAndDrop:      nil,
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
	})
	if err != nil {
		log.Fatal(fmt.Errorf("run settings window: %w", err))
	}
}
