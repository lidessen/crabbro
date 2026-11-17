package main

import (
	"strings"
	"unicode"

	"github.com/egoist/mygo/ui"
)

// Setting the window's data as text rather than as strings. An address, a
// path, a header's value or a line of JSON is not one run of mono in one
// color: it has parts, and the eye reads it by them. So the marks that
// only join the parts — the slashes, colons and quotes — step back, the
// part that names the thing steps forward, and a figure's unit is set
// smaller than the figure.

// monoAdvance is the width of one character of the mono face at the size
// data is set in, 12: what a column of names is measured by.
const monoAdvance = 7.2

// joiners are the marks that hold a value's parts together and carry no
// meaning of their own.
const joiners = `/:;=,&?@{}[]()<>|"'→`

// dataSpans is s with its joining marks in dim and the rest in ink.
func dataSpans(s string, ink, dim ui.Color) []ui.Span {
	var out []ui.Span
	start, wasJoin := 0, false
	flush := func(end int) {
		if end > start {
			col := ink
			if wasJoin {
				col = dim
			}
			out = append(out, ui.Span{Text: s[start:end], Color: col})
		}
		start = end
	}
	for i, r := range s {
		if j := strings.ContainsRune(joiners, r); j != wasJoin {
			flush(i)
			wasJoin = j
		}
	}
	flush(len(s))
	return out
}

// dataText is a value set in mono with its joining marks held back: what
// a fact, a header or a parameter reads in.
func dataText(c *ui.Context, p *palette, s string) ui.Element {
	return ui.RichText(c, dataSpans(s, p.text, p.faint.Alpha(0.75))...).Font(mono()).FontSize(12)
}

// hostSpans is a host as it is read: the name in ink, the port after it a
// step back, as the lesser part of where a request went.
func hostSpans(host string, ink, dim ui.Color) []ui.Span {
	name, port := host, ""
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.HasSuffix(host, "]") {
		name, port = host[:i], host[i+1:]
	}
	out := []ui.Span{{Text: name, Color: ink}}
	if port != "" {
		out = append(out, ui.Span{Text: ":", Color: dim.Alpha(0.7)}, ui.Span{Text: port, Color: dim})
	}
	return out
}

// hostText is a host set in mono by hostSpans.
func hostText(c *ui.Context, host string, ink, dim ui.Color) ui.Element {
	return ui.RichText(c, hostSpans(host, ink, dim)...).Font(mono()).FontSize(12)
}

// pathSpans is a request's path as it is read: the folders it lies under
// a step back, their slashes fainter still, and the last segment — the
// thing asked for — in ink and a weight up. A query after it is set as
// its pairs: the names quiet, the values in ink.
func pathSpans(p *palette, path string) []ui.Span {
	query := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i+1:]
	}
	slash, lead := p.faint.Alpha(0.7), p.text.Mix(p.muted, 0.7)
	var out []ui.Span
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if i > 0 {
			out = append(out, ui.Span{Text: "/", Color: slash})
		}
		if seg == "" {
			continue
		}
		if i == len(segs)-1 || (i == len(segs)-2 && segs[len(segs)-1] == "") {
			out = append(out, ui.Span{Text: seg, Color: p.text, Weight: 500})
		} else {
			out = append(out, ui.Span{Text: seg, Color: lead})
		}
	}
	if query == "" {
		return out
	}
	out = append(out, ui.Span{Text: "?", Color: slash})
	for i, pair := range strings.Split(query, "&") {
		if i > 0 {
			out = append(out, ui.Span{Text: "&", Color: slash})
		}
		k, v, has := strings.Cut(pair, "=")
		out = append(out, ui.Span{Text: k, Color: p.muted})
		if has {
			out = append(out, ui.Span{Text: "=", Color: slash}, ui.Span{Text: v, Color: p.text})
		}
	}
	return out
}

// pathText is a path set in mono by pathSpans, at size.
func pathText(c *ui.Context, p *palette, path string, size float32) ui.Element {
	return ui.RichText(c, pathSpans(p, path)...).Font(mono()).FontSize(size)
}

// filePathSpans is a file's path: the folders quiet, the file in ink.
func filePathSpans(p *palette, path string) []ui.Span {
	i := strings.LastIndexByte(path, '/')
	if i < 0 || i == len(path)-1 {
		return dataSpans(path, p.text, p.faint.Alpha(0.75))
	}
	out := dataSpans(path[:i+1], p.muted, p.faint.Alpha(0.7))
	return append(out, ui.Span{Text: path[i+1:], Color: p.text, Weight: 500})
}

// figureSpans is a measured value as a figure and its unit: "311ms" is
// 311 in ink and ms set smaller and fainter after it, as a table of
// measurements is set in print. A value with no figure reads whole.
func figureSpans(label string, size float32, ink, dim ui.Color) []ui.Span {
	i := len(label)
	for i > 0 && unicode.IsLetter(rune(label[i-1])) {
		i--
	}
	if i == 0 || i == len(label) {
		return []ui.Span{{Text: label, Color: ink}}
	}
	return []ui.Span{
		{Text: label[:i], Color: ink},
		{Text: label[i:], Color: dim, Size: size - 1.5},
	}
}

// figureText is a measured value set in mono by figureSpans.
func figureText(c *ui.Context, label string, size float32, ink, dim ui.Color) ui.Element {
	return ui.RichText(c, figureSpans(label, size, ink, dim)...).Font(mono()).FontSize(size)
}
