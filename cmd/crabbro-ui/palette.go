package main

import (
	"os"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

// codeThemes keeps the code theme of each ground, by the appearance and
// the face it was made for, so an editor is handed the same theme frame
// after frame.
var codeThemes = map[[2]bool]*editor.Theme{}

// codeTheme colors code in the window's own voices rather than a borrowed
// scheme: keys in the teal, strings in the amber, numbers in the blue of
// what changed, and the accent kept for the few words that act. sheet is
// code read on a face of its own, as a body is, rather than edited on the
// page: it has no caret to mark a line or pair brackets round.
func (p *palette) codeTheme(c *ui.Context, sheet bool) *editor.Theme {
	key := [2]bool{p.dark, sheet}
	if t := codeThemes[key]; t != nil {
		return t
	}
	t := editor.DefaultTheme(c.Theme())
	if sheet {
		t.Background, t.MatchingBracket = p.surface, ui.Transparent
	}
	t.LineNumber = p.faint.Alpha(0.6)
	t.ActiveLineNumber = p.muted
	t.ActiveLine = p.text.Alpha(0.035)
	t.IndentGuide = p.text.Alpha(0.07)
	t.ActiveIndentGuide = p.text.Alpha(0.18)
	t.Cursor = p.accent
	tok := func(col ui.Color, kinds ...editor.TokenKind) {
		for _, k := range kinds {
			t.Tokens[k].Color = col
		}
	}
	tok(p.teal(), editor.TokenProperty, editor.TokenAttribute, editor.TokenType)
	tok(p.warning(), editor.TokenString, editor.TokenRegexp, editor.TokenLink, editor.TokenCode)
	tok(p.changed, editor.TokenNumber, editor.TokenConstant, editor.TokenEscape, editor.TokenFunction, editor.TokenHeading)
	tok(p.accent, editor.TokenKeyword, editor.TokenTag, editor.TokenMeta)
	tok(p.faint, editor.TokenOperator, editor.TokenPunctuation, editor.TokenComment)
	t.Tokens[editor.TokenComment].Italic = true
	codeThemes[key] = t
	return t
}

// teal is the second voice beside the coral accent: it tints what the
// coral would otherwise share with the errors, as POST does.
func (p *palette) teal() ui.Color {
	if p.second != nil {
		return *p.second
	}
	if p.dark {
		return ui.Hex("#5ccfbf")
	}
	return ui.Hex("#17766a")
}

// The shell, workspace ground, and raised surfaces use warm neutrals.
// Coral marks navigation and primary actions; status colors carry meaning.
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
// "emerald". CRABBRO_THEME chooses it at launch.
var variant = os.Getenv("CRABBRO_THEME")

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
		editor:         ui.Hex("#1b1a1c"),
		chrome:         ui.Hex("#151416"),
		border:         ui.Hex("#363336"),
		hover:          ui.RGBA(255, 255, 255, 0.06),
		active:         ui.RGBA(255, 129, 107, 0.13),
		text:           ui.Hex("#f1edeb"),
		muted:          ui.Hex("#c4bcb9"),
		faint:          ui.Hex("#a09998"),
		accent:         ui.Hex("#ff816b"),
		fill:           ui.Hex("#bb3e30"),
		fillText:       ui.Hex("#fff6e8"),
		dangerFill:     ui.Hex("#a51f3b"),
		surface:        ui.Hex("#222123"),
		surfaceHover:   ui.Hex("#2b292c"),
		surfacePressed: ui.Hex("#353236"),
		scrollbar:      ui.RGBA(255, 255, 255, 0.16),
		inverse:        ui.Hex("#2f2a2b"),
		inverseText:    ui.Hex("#f0e7e3"),
		focus:          ui.RGBA(255, 129, 107, 0.7),
		added:          ui.Hex("#9ccb84"),
		changed:        ui.Hex("#8fb4ff"),
		removed:        ui.Hex("#ff6b8f"),
	}
	return p.finish()
}

func lightPalette() *palette {
	p := &palette{
		editor:         ui.Hex("#f6f5f3"),
		chrome:         ui.Hex("#f0eeeb"),
		border:         ui.Hex("#e2ded9"),
		hover:          ui.RGBA(41, 29, 26, 0.055),
		active:         ui.RGBA(179, 57, 38, 0.09),
		text:           ui.Hex("#261f20"),
		muted:          ui.Hex("#5f5552"),
		faint:          ui.Hex("#726761"),
		accent:         ui.Hex("#ac3829"),
		fill:           ui.Hex("#b13629"),
		fillText:       ui.Hex("#fffaf5"),
		dangerFill:     ui.Hex("#c8264f"),
		surface:        ui.Hex("#fffefd"),
		surfaceHover:   ui.Hex("#f5f1ed"),
		surfacePressed: ui.Hex("#ebe5e0"),
		scrollbar:      ui.RGBA(0, 0, 0, 0.18),
		inverse:        ui.Hex("#2a1c1e"),
		inverseText:    ui.Hex("#faf4e8"),
		focus:          ui.RGBA(172, 56, 41, 0.65),
		added:          ui.Hex("#2f7a1c"),
		changed:        ui.Hex("#2a5bd7"),
		removed:        ui.Hex("#c8164a"),
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
	t.Radius = 8
	t.Spacing = 3.5
	t.FontSize = 13
	t.Font = fontSans
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
	t.Background, t.Surface, t.TextMuted = p.surface, p.editor, p.muted
	t.Spacing = 3.9
	c.SetTheme(&t)
}

// wash is how strongly a color tints the ground behind text of the same
// color, as a status pill's does.
func (p *palette) wash() float32 {
	if p.dark {
		return 0.16
	}
	return 0.1
}

// warning is the color of what waits: amber on either appearance.
func (p *palette) warning() ui.Color {
	if p.dark {
		return ui.Hex("#e8b86d")
	}
	return ui.Hex("#9a5f14")
}

var (
	white = ui.RGBA(255, 255, 255, 1)
	black = ui.RGBA(0, 0, 0, 1)
)
