package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"streamy/internal/capture"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resolver := capture.NewResolver(nil)
	windows, err := resolver.Windows(ctx)
	if err != nil {
		panic(err)
	}
	for _, w := range windows {
		if !strings.Contains(strings.ToLower(w.Title), strings.ToLower(os.Args[1])) {
			continue
		}
		desktop, err := resolver.Snapshot(ctx)
		if err != nil {
			panic(err)
		}
		target := capture.Target{Kind: capture.KindWindow, Window: w, Reason: "probe"}.WithDesktop(desktop)
		data, err := capture.Preview(ctx, target, 480, 270)
		if err != nil {
			fmt.Println("preview error:", err)
			continue
		}
		if err := os.WriteFile(os.Args[2], data, 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("wrote %d bytes for %q %dx%d\n", len(data), w.Title, w.Rect.Width, w.Rect.Height)
		return
	}
	fmt.Println("window not found")
}
