package main

import "github.com/egoist/mygo/ui"

// Icons from Lucide (ISC license), drawn in the color of the text.
func lucide(body string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + body + `</svg>`))
}

var (
	iconShell   = lucide(`<path d="M4 17l6-6-6-6"/><path d="M12 19h8"/>`)
	iconList    = lucide(`<line x1="8" x2="21" y1="6" y2="6"/><line x1="8" x2="21" y1="12" y2="12"/><line x1="8" x2="21" y1="18" y2="18"/><line x1="3" x2="3.01" y1="6" y2="6"/><line x1="3" x2="3.01" y1="12" y2="12"/><line x1="3" x2="3.01" y1="18" y2="18"/>`)
	iconSliders = lucide(`<line x1="21" x2="14" y1="4" y2="4"/><line x1="10" x2="3" y1="4" y2="4"/><line x1="21" x2="12" y1="12" y2="12"/><line x1="8" x2="3" y1="12" y2="12"/><line x1="21" x2="16" y1="20" y2="20"/><line x1="12" x2="3" y1="20" y2="20"/><line x1="14" x2="14" y1="2" y2="6"/><line x1="8" x2="8" y1="10" y2="14"/><line x1="16" x2="16" y1="18" y2="22"/>`)
	iconClock   = lucide(`<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>`)
	iconServer  = lucide(`<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>`)
	iconRecord  = lucide(`<circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="4"/>`)
	iconStop    = lucide(`<rect width="16" height="16" x="4" y="4" rx="2"/>`)
	iconReplay  = lucide(`<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/>`)
	iconSwitch  = lucide(`<rect width="20" height="12" x="2" y="6" rx="6"/><circle cx="16" cy="12" r="2"/>`)
	iconTrash   = lucide(`<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>`)
	iconUndo    = lucide(`<path d="M3 7v6h6"/><path d="M21 17a9 9 0 0 0-9-9 9 9 0 0 0-6 2.3L3 13"/>`)
	iconSave    = lucide(`<path d="M15.2 3a2 2 0 0 1 1.4.6l3.8 3.8a2 2 0 0 1 .6 1.4V19a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z"/><path d="M17 21v-7a1 1 0 0 0-1-1H8a1 1 0 0 0-1 1v7"/><path d="M7 3v4a1 1 0 0 0 1 1h7"/>`)
	iconPlay    = lucide(`<polygon points="6 3 20 12 6 21 6 3"/>`)
	iconUp      = lucide(`<path d="m5 12 7-7 7 7"/><path d="M12 19V5"/>`)
	iconDown    = lucide(`<path d="M12 5v14"/><path d="m19 12-7 7-7-7"/>`)
	iconX       = lucide(`<path d="M18 6 6 18"/><path d="m6 6 12 12"/>`)
	iconPlus    = lucide(`<path d="M5 12h14"/><path d="M12 5v14"/>`)
)
