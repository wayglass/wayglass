package app

import (
	"context"
	"errors"
	"io/fs"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// NewOptions is the window. A plain function, not a method, so nothing in
// it is bound to JavaScript by accident.
func NewOptions(a *App, assets fs.FS) *options.App {
	return &options.App{
		Title:            "Wayglass",
		Width:            1320,
		Height:           860,
		MinWidth:         960,
		MinHeight:        640,
		BackgroundColour: &options.RGBA{R: 20, G: 21, B: 24, A: 255},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        a.startup,
		OnShutdown:       a.shutdown,
		Bind:             []any{a},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			WebviewIsTransparent: false,
			About: &mac.AboutInfo{
				Title:   "Wayglass",
				Message: "Monitor agents and decide the changes routed to you.",
			},
		},
	}
}

// wailsNative calls the Wails runtime with the context from OnStartup.
type wailsNative struct {
	ctx  context.Context
	once sync.Once
	ok   bool
}

func (n *wailsNative) Emit(event string, data ...any) { runtime.EventsEmit(n.ctx, event, data...) }

func (n *wailsNative) SetTitle(title string) { runtime.WindowSetTitle(n.ctx, title) }

func (n *wailsNative) OpenURL(url string) { runtime.BrowserOpenURL(n.ctx, url) }

func (n *wailsNative) CopyText(text string) error { return runtime.ClipboardSetText(n.ctx, text) }

// Notify sends a native notification. It asks for permission once; a denied
// permission or an unbundled dev binary just means no notifications.
func (n *wailsNative) Notify(title, body string) error {
	n.once.Do(func() {
		if runtime.InitializeNotifications(n.ctx) != nil || !runtime.IsNotificationAvailable(n.ctx) {
			return
		}
		granted, err := runtime.RequestNotificationAuthorization(n.ctx)
		n.ok = err == nil && granted
	})
	if !n.ok {
		return errors.New("notifications are not available or not allowed")
	}
	return runtime.SendNotification(n.ctx, runtime.NotificationOptions{ID: "wayglass-" + title + body, Title: title, Body: body})
}
