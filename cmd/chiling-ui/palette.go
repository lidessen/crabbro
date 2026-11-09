package main

import "github.com/egoist/mygo/ui"

// teal is the second voice beside the coral accent: it tints what the
// coral would otherwise share with the errors, as POST does.
func (p *palette) teal() ui.Color {
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
	p.fillHover, p.fillPressed = p.fill.Mix(white, 0.1), p.fill.Mix(black, 0.15)
	return p
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
	p.fillHover, p.fillPressed = p.fill.Mix(white, 0.1), p.fill.Mix(black, 0.15)
	return p
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
