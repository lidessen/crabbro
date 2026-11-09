package main

import (
	"os"

	"github.com/egoist/mygo/ui"
)

// teal is the second voice beside the coral accent: it tints what the
// coral would otherwise share with the errors, as POST does.
func (p *palette) teal() ui.Color {
	if p.second != nil {
		return *p.second
	}
	if p.dark {
		return ui.Hex("#5ccfbf")
	}
	return ui.Hex("#1c8a7c")
}

// palette is the look of the window, in the roles Mayoi's workbench uses:
// the chrome of the shell (title bar, sidebar, status bar), the editor the
// pages sit on, the faces of controls, and the accent and its tints. Chiling's own
// voice is the coral of its crab, over quiet warm-neutral greys.
type palette struct {
	dark bool

	editor ui.Color // the pages' ground
	chrome ui.Color // the title bar, the sidebar and the status bar
	border ui.Color
	hover  ui.Color
	active ui.Color // a chosen row: the accent as a tint, not itself
	text   ui.Color
	muted  ui.Color
	faint  ui.Color
	accent ui.Color
	// fill is the accent as a button's background, under fillText; hover
	// and pressed are its states.
	fill, fillHover, fillPressed, fillText ui.Color
	dangerFill                             ui.Color
	surface, surfaceHover, surfacePressed  ui.Color
	scrollbar                              ui.Color
	inverse, inverseText                   ui.Color
	// focus is the keyboard's focus ring: the accent, dimmed to a wash.
	focus ui.Color
	// added, changed and removed are what went well, what waits, and
	// what went wrong, as text that reads on the chrome and the editor.
	added, changed, removed ui.Color
	// second overrides teal, for a variant whose accent sits near it.
	second *ui.Color
}

// variant names the palette in use: "coral" (the default), "indigo" or
// "emerald". CHILING_THEME chooses it at launch.
var variant = os.Getenv("CHILING_THEME")

// finish lays the chosen variant over p's coral base and derives the
// button states from its fill.
func (p *palette) finish() *palette {
	h := ui.Hex
	set := func(dark, light map[string]string) {
		m := light
		if p.dark {
			m = dark
		}
		fields := map[string]*ui.Color{"editor": &p.editor, "chrome": &p.chrome, "border": &p.border, "text": &p.text,
			"muted": &p.muted, "faint": &p.faint, "accent": &p.accent, "fill": &p.fill, "surface": &p.surface,
			"surfaceHover": &p.surfaceHover, "surfacePressed": &p.surfacePressed, "added": &p.added, "changed": &p.changed}
		for k, v := range m {
			if k == "second" {
				c := h(v)
				p.second = &c
				continue
			}
			*fields[k] = h(v)
		}
		if a, ok := m["accent"]; ok {
			ac := h(a)
			p.active = ac.Alpha(map[bool]float32{true: 0.15, false: 0.11}[p.dark])
			p.focus = ac.Alpha(0.5)
			p.dangerFill = p.removed
		}
		p.hover = map[bool]ui.Color{true: ui.RGBA(255, 255, 255, 0.045), false: ui.RGBA(0, 0, 0, 0.04)}[p.dark]
	}
	switch variant {
	case "indigo":
		set(map[string]string{"editor": "#12141c", "chrome": "#0d0f15", "border": "#252a38", "text": "#e3e6f0", "muted": "#9aa1b5",
			"faint": "#6a7188", "accent": "#7f95ff", "fill": "#4f6af0", "surface": "#181b25", "surfaceHover": "#1f2330",
			"surfacePressed": "#272c3b", "changed": "#6cc4ff", "second": "#5ccfbf"},
			map[string]string{"editor": "#fbfbfe", "chrome": "#f3f4f9", "border": "#e2e5ef", "text": "#20232f", "muted": "#646a80",
				"faint": "#8a90a6", "accent": "#3f55d6", "fill": "#4a5fe0", "surface": "#ffffff", "surfaceHover": "#f1f2f8",
				"surfacePressed": "#e8eaf3", "changed": "#0a7fc2", "second": "#1c8a7c"})
	case "emerald":
		set(map[string]string{"editor": "#121614", "chrome": "#0d100f", "border": "#232b27", "text": "#e2e9e5", "muted": "#96a39c",
			"faint": "#67746d", "accent": "#34d399", "fill": "#0b8a5f", "surface": "#181d1b", "surfaceHover": "#1f2523",
			"surfacePressed": "#272e2b", "added": "#c3e36a", "second": "#b79cff"},
			map[string]string{"editor": "#fbfdfc", "chrome": "#f1f6f3", "border": "#dfe8e3", "text": "#1f2a25", "muted": "#5f6e66",
				"faint": "#86948c", "accent": "#0f8a5f", "fill": "#0f8a5f", "surface": "#ffffff", "surfaceHover": "#eff5f1",
				"surfacePressed": "#e6eee9", "added": "#5a8a10", "second": "#7c4dff"})
	}
	p.fillHover, p.fillPressed = p.fill.Mix(white, 0.1), p.fill.Mix(black, 0.15)
	return p
}

func darkPalette() *palette {
	p := &palette{
		dark:           true,
		editor:         ui.Hex("#17171a"),
		chrome:         ui.Hex("#111113"),
		border:         ui.Hex("#2a2a30"),
		hover:          ui.RGBA(255, 255, 255, 0.045),
		active:         ui.RGBA(255, 138, 91, 0.14),
		text:           ui.Hex("#e8e5e1"),
		muted:          ui.Hex("#9a9aa6"),
		faint:          ui.Hex("#6e6e7a"),
		accent:         ui.Hex("#ff8a5b"),
		fill:           ui.Hex("#d9532a"),
		fillText:       ui.Hex("#ffffff"),
		dangerFill:     ui.Hex("#c42b4d"),
		surface:        ui.Hex("#1f1f24"),
		surfaceHover:   ui.Hex("#28282e"),
		surfacePressed: ui.Hex("#303038"),
		scrollbar:      ui.RGBA(255, 255, 255, 0.16),
		inverse:        ui.Hex("#2b2b34"),
		inverseText:    ui.Hex("#e6e6ee"),
		focus:          ui.RGBA(255, 138, 91, 0.5),
		added:          ui.Hex("#a9d48f"),
		changed:        ui.Hex("#8fb4ff"),
		removed:        ui.Hex("#f5627f"),
	}
	return p.finish()
}

func lightPalette() *palette {
	p := &palette{
		editor:         ui.Hex("#fdfcfb"),
		chrome:         ui.Hex("#f6f4f2"),
		border:         ui.Hex("#e8e5e1"),
		hover:          ui.RGBA(0, 0, 0, 0.04),
		active:         ui.RGBA(224, 89, 42, 0.11),
		text:           ui.Hex("#26262c"),
		muted:          ui.Hex("#6b6a72"),
		faint:          ui.Hex("#8f8e97"),
		accent:         ui.Hex("#c9431b"),
		fill:           ui.Hex("#d9532a"),
		fillText:       ui.Hex("#ffffff"),
		dangerFill:     ui.Hex("#c8264f"),
		surface:        ui.Hex("#ffffff"),
		surfaceHover:   ui.Hex("#f3f1ee"),
		surfacePressed: ui.Hex("#ebe8e4"),
		scrollbar:      ui.RGBA(0, 0, 0, 0.18),
		inverse:        ui.Hex("#26262c"),
		inverseText:    ui.Hex("#f6f4f2"),
		focus:          ui.RGBA(224, 89, 42, 0.45),
		added:          ui.Hex("#3d8a28"),
		changed:        ui.Hex("#2a5bd7"),
		removed:        ui.Hex("#c8264f"),
	}
	return p.finish()
}

// applyPal sets the window's theme from pal, as the widgets draw with it,
// and returns the palette the frame paints with.
func applyPal(c *ui.Context) *palette {
	t := *c.Theme()
	p := lightPalette()
	if t.Dark {
		p = darkPalette()
	}
	t.Dark = p.dark
	t.Background = p.editor
	t.Accent, t.AccentText = p.fill, p.fillText
	t.AccentHover, t.AccentPressed = p.fillHover, p.fillPressed
	t.Danger = p.removed
	t.Warning = p.warning()
	t.Success = p.added
	t.Focus = p.focus
	t.Border = p.border
	t.Text, t.TextMuted = p.text, p.muted
	t.Selection = p.active
	t.Radius = 6
	t.Spacing = 3.5
	t.FontSize = 13
	t.ScrollbarWidth = 8
	t.Surface, t.SurfaceHover, t.SurfacePressed = p.surface, p.surfaceHover, p.surfacePressed
	t.Scrollbar = p.scrollbar
	t.Inverse, t.InverseText = p.inverse, p.inverseText
	c.SetTheme(&t)
	return p
}

// quiet sets the theme for a table's build: rows show chosen in a tint of
// the accent, not in the accent itself, which would swallow the cells.
// The caller restores the theme it kept when the table is built.
func (p *palette) quiet(c *ui.Context) {
	t := *c.Theme()
	t.Accent, t.AccentText, t.SurfaceHover = p.active, p.text, p.hover
	c.SetTheme(&t)
}

// warning is the color of what waits: amber on either appearance.
func (p *palette) warning() ui.Color {
	if p.dark {
		return ui.Hex("#e8b86d")
	}
	return ui.Hex("#c4862a")
}

var (
	white = ui.RGBA(255, 255, 255, 1)
	black = ui.RGBA(0, 0, 0, 1)
)
