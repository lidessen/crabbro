package main

import (
	"github.com/egoist/mygo"
)

// The application menu bar: the pages with their accelerators, the
// recording, and the standard roles. The menu is built once and never
// mutated afterwards — items change state only as the user chooses them,
// so the frame never touches AppKit's menus from inside a build.
func (a *app) menu() *mygo.Menu {
	page := func(key, label, acc string) *mygo.MenuItem {
		return &mygo.MenuItem{
			ID: "page-" + key, Label: label, Accelerator: acc,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				a.setPage(key)
			},
		}
	}
	record := &mygo.MenuItem{
		ID: "record", Label: "Toggle Recording", Accelerator: "CmdOrCtrl+R",
		Click: func(*mygo.MenuItem, *mygo.Window) { a.menuRecord() },
	}
	return mygo.NewMenu([]*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Label: "File", Submenu: []*mygo.MenuItem{
			record,
			mygo.Separator(),
			{Role: mygo.RoleClose},
		}},
		{Role: mygo.RoleEditMenu},
		{Label: "View", Submenu: []*mygo.MenuItem{
			page(pageRequests, "Requests", "CmdOrCtrl+1"),
			page(pageRules, "Rules", "CmdOrCtrl+2"),
			page(pageTrajectory, "Trajectory", "CmdOrCtrl+3"),
			page(pageDaemon, "Daemon", "CmdOrCtrl+4"),
			mygo.Separator(),
			{Role: mygo.RoleToggleFullScreen},
		}},
		{Role: mygo.RoleWindowMenu},
		{Role: mygo.RoleHelp},
	})
}

// setPage shows a page and redraws, from the menu or any caller on the
// main thread.
func (a *app) setPage(key string) {
	a.page = key
	a.invalidate()
}

// menuRecord starts or stops the recording, as the state stands when the
// item is chosen. Menu clicks arrive on the main thread, so the actions
// are safe here.
func (a *app) menuRecord() {
	if a.recordingNow() {
		a.actStopRecording()
		return
	}
	a.actStartRecording()
}
