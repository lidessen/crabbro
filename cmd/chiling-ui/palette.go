package main

import "github.com/egoist/mygo/ui"

// palette is the look of the window, in the roles Mayoi's workbench uses:
// the chrome of the shell (title bar, sidebar, status bar), the editor the
// pages sit on, the faces of controls, and the accent and its tints. The
// colors are Mayoi's, so the two apps read as kin.
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
		editor:         ui.Hex("#18181d"),
		chrome:         ui.Hex("#131317"),
		border:         ui.Hex("#26262e"),
		hover:          ui.RGBA(255, 255, 255, 0.045),
		active:         ui.RGBA(139, 124, 246, 0.16),
		text:           ui.Hex("#d6d6de"),
		muted:          ui.Hex("#8c8c99"),
		faint:          ui.Hex("#666672"),
		accent:         ui.Hex("#8b7cf6"),
		fill:           ui.Hex("#7260e3"),
		fillText:       ui.Hex("#ffffff"),
		dangerFill:     ui.Hex("#bb3253"),
		surface:        ui.Hex("#202027"),
		surfaceHover:   ui.Hex("#272730"),
		surfacePressed: ui.Hex("#2e2e38"),
		scrollbar:      ui.RGBA(255, 255, 255, 0.16),
		inverse:        ui.Hex("#2b2b34"),
		inverseText:    ui.Hex("#e6e6ee"),
		focus:          ui.RGBA(139, 124, 246, 0.55),
		added:          ui.Hex("#a9d48f"),
		changed:        ui.Hex("#8fb4ff"),
		removed:        ui.Hex("#f0718b"),
	}
	p.fillHover, p.fillPressed = p.fill.Mix(white, 0.1), p.fill.Mix(black, 0.15)
	return p
}

func lightPalette() *palette {
	p := &palette{
		editor:         ui.Hex("#fcfcfd"),
		chrome:         ui.Hex("#f4f4f7"),
		border:         ui.Hex("#e4e4ea"),
		hover:          ui.RGBA(0, 0, 0, 0.04),
		active:         ui.RGBA(109, 91, 208, 0.12),
		text:           ui.Hex("#2a2a33"),
		muted:          ui.Hex("#6c6c79"),
		faint:          ui.Hex("#8e8e9a"),
		accent:         ui.Hex("#6d5bd0"),
		fill:           ui.Hex("#6d5bd0"),
		fillText:       ui.Hex("#ffffff"),
		dangerFill:     ui.Hex("#d0365a"),
		surface:        ui.Hex("#ffffff"),
		surfaceHover:   ui.Hex("#f2f2f6"),
		surfacePressed: ui.Hex("#eaeaf0"),
		scrollbar:      ui.RGBA(0, 0, 0, 0.18),
		inverse:        ui.Hex("#2a2a33"),
		inverseText:    ui.Hex("#f4f4f7"),
		focus:          ui.RGBA(109, 91, 208, 0.5),
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
