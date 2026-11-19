package main

import (
	"embed"
	"log"

	"github.com/egoist/mygo/ui"
)

// The window's type is Geist and Geist Mono (SIL Open Font License, see
// fonts/OFL.txt), carried in the binary so it reads the same on every
// system: one family drawn in two widths, so prose and the code, paths and
// ids beside it agree in weight and height.
//
//go:embed fonts/*.ttf
var fontFiles embed.FS

const (
	fontSans = "Geist"
	fontMono = "Geist Mono"
)

// codeFont is the font of the code editors: the mono, at the body's size.
func codeFont() ui.Font { return ui.Font{Family: fontMono, Size: 12.5} }

func init() {
	for family, files := range map[string][]string{
		fontSans: {"Geist-Regular.ttf", "Geist-Medium.ttf", "Geist-SemiBold.ttf"},
		fontMono: {"GeistMono-Regular.ttf", "GeistMono-Medium.ttf", "GeistMono-SemiBold.ttf"},
	} {
		for _, name := range files {
			data, err := fontFiles.ReadFile("fonts/" + name)
			if err == nil {
				err = ui.RegisterFont(data, family)
			}
			if err != nil {
				// Text falls back to the system's font; say why.
				log.Printf("font %s: %v", name, err)
			}
		}
	}
}
