package main

import (
	_ "embed"

	"github.com/egoist/mygo/ui"
)

// logoPNG is the crab, the app's mark: the same artwork as its icon.
//
//go:embed logo.png
var logoPNG []byte

// logo is the decoded mark, nil if the bytes do not decode.
var logo ui.ImageSource

func init() { logo, _ = ui.DecodeImage(logoPNG) }

// brandMark draws the crab at size DIPs square, rounded as an app icon.
func brandMark(c *ui.Context, p *palette, size float32) {
	if logo == nil {
		ui.Row(c).Size(size, size).Center().Radius(size * 0.3).Background(p.accent.Alpha(0.16)).Children(func() {
			ui.Icon(c, iconShell).FontSize(size * 0.6).TextColor(p.accent)
		})
		return
	}
	ui.Image(c, logo).Size(size, size).Radius(size * 0.28)
}
