package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"github.com/lidessen/chiling/pkg/api"
)

// The rules page: the rule list and, below it, the editor of the one
// chosen — an overview to read it by and the YAML code to edit it in.

// editorFade is how the script editor follows a theme change: its face and
// hairline fade to the new palette rather than swap under the eye.
var editorFade = ui.ElementTransition{Colors: true, Duration: 180 * time.Millisecond}

// ruleConfirming is the rule a native delete dialog is out for, "" for
// none. The frame function sets it when it asks and reads it to quiet the
// Delete button; the dialog's goroutine answers through a.post, which
// clears it on the view thread before the next frame, so one rule asks at
// a time and no second dialog opens while one is up.
var ruleConfirming string

// The overview's working state, on the view thread. ruleVM is the rule's
// document parsed for reading; ruleVMSeen is the script it was parsed from,
// so it reparses only when the draft changed.
var (
	ruleVM        *vmDoc
	ruleVMSeen    string
	ruleVMErr     string
	ruleCodeState *editor.State
	ruleCodeSeen  string
)

// rulesPage is the rule list and, below it, the editor of the one chosen.
func (a *app) rulesPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	a.ruleState.Key = func(row int) any { return string(st.rules[row].ID) }
	a.ruleState.Selected = &a.ruleSel

	// The editor shows the rule the table chose, its script loaded from
	// the list when the choice changes.
	if id := a.chosenRuleID(st.rules); id != a.ruleShownID {
		a.ruleShownID = id
		a.scriptDraft = ""
		ruleVMSeen = ""
		ruleVM = nil
		ruleCodeState = nil
		a.loadRuleScript(id)
	}
	dirty := a.ruleShownID != "" && a.scriptDraft != "" && a.scriptDraft != a.scriptLoadedText(st.rules)

	base := *c.Theme()
	p.quiet(c)
	ui.SplitVertical(c, &detailSplitH,
		func() {
			if len(st.rules) == 0 {
				a.rulesEmpty(c, p)
				return
			}
			cols := []ui.TableColumn{
				{Title: "Name", ID: "name"},
				{Title: "Priority", ID: "priority", Width: 76, Align: ui.End, Sortable: true},
				{Title: "Enabled", ID: "enabled", Width: 84},
			}
			ui.Table(c, &a.ruleState, cols, len(st.rules), func(row, col int) {
				a.ruleCell(c, p, st.rules[row], col)
			}).Grow(1)
		},
		func() { a.ruleEditor(c, p, st, dirty) },
	).Grow(1)
	c.SetTheme(&base)
}

// rulesEmpty is what the list shows while there are no rules: a quiet
// sliders mark and the command that scaffolds the first one from a
// capture, where the empty table would sit.
func (a *app) rulesEmpty(c *ui.Context, p *palette) {
	emptyState(c, p, iconSliders, "No rules yet",
		"Rules rewrite, mock or block traffic. Scaffold the first one from a captured request.",
		"chiling rules from req_1 --action mock")
}

// chosenRuleID is the rule the table's choice names, "" for none.
func (a *app) chosenRuleID(rules []api.Rule) string {
	if a.ruleSel < 0 || a.ruleSel >= len(rules) {
		return ""
	}
	return string(rules[a.ruleSel].ID)
}

// scriptLoadedText is the rule's script as the store has it, which the
// editor compares its draft against to say whether it is dirty.
func (a *app) scriptLoadedText(rules []api.Rule) string {
	for _, r := range rules {
		if string(r.ID) == a.ruleShownID {
			return r.Script
		}
	}
	return ""
}

// loadRuleScript fills the editor's draft with the rule's script.
func (a *app) loadRuleScript(id string) {
	if id == "" {
		return
	}
	go func() {
		r, err := a.c.GetRule(a.ctx, id)
		a.post(func() {
			if a.ruleShownID == id && err == nil {
				a.scriptDraft = r.Script
				a.invalidate()
			}
		})
	}()
}

// ruleCell draws one cell of the rules table.
func (a *app) ruleCell(c *ui.Context, p *palette, r api.Rule, col int) {
	switch col {
	case 0:
		ui.Row(c).Grow(1).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, ruleLabel(r)).FontSize(12.5).FontWeight(550).TextColor(map[bool]ui.Color{true: p.text, false: p.muted}[r.Enabled]).SingleLine().Shrink(1).MinWidth(20)
			if r.FailureMode == "fail-closed" {
				ui.Badge(c, "fail-closed")
			}
		})
	case 1:
		ui.Text(c, fmt.Sprint(r.Priority)).FontSize(11.5).Font(mono()).TextColor(p.faint).TextAlign(ui.End).SingleLine()
	case 2:
		if r.Enabled {
			a.pill(c, "on", p.added, p.added.Alpha(0.13), false)
		} else {
			a.pill(c, "off", p.faint, p.faint.Alpha(0.12), false)
		}
	}
}

// ruleLabel is what the list names a rule: its name, else its id.
func ruleLabel(r api.Rule) string {
	if r.Name != "" {
		return r.Name
	}
	return string(r.ID)
}

// ruleEditor is the rule the list chose: its toolbar — the name, the
// meta, the overview and code views, and what to do — over the view itself.
// The overview reads the rule; the YAML in the code view is what is edited.
func (a *app) ruleEditor(c *ui.Context, p *palette, st store, dirty bool) {
	if a.ruleShownID == "" {
		if len(st.rules) == 0 {
			ui.Column(c).Grow(1).BorderWidth(1, 0, 0, 0).BorderColor(p.border)
			return
		}
		ui.Column(c).Grow(1).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
			emptyState(c, p, iconSliders, "Select a rule", "Choose a rule to read it, or edit its YAML.", "")
		})
		return
	}
	rule := a.ruleShownID
	r := ruleOf(st.rules, rule)

	a.syncRuleVM()
	// A draft the overview cannot read opens in the code view.
	if a.ruleEditMode == 0 && a.scriptDraft != "" && ruleVM == nil {
		a.ruleEditMode = 1
	}

	ui.Column(c).Grow(1).MinHeight(0).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		ui.Row(c).Height(38).Shrink(0).Padding(0, 14).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				if r != nil && r.Name != "" {
					ui.Text(c, r.Name).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
					ui.Text(c, "·").FontSize(12).Font(mono()).TextColor(p.faint).SingleLine()
					ui.Text(c, rule).FontSize(11.5).Font(mono()).FontWeight(550).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(40)
				} else {
					ui.Text(c, rule).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(40)
				}
			})
			if dirty {
				pillEl(c.Key("rule-edited"), "edited", p.warning(), p.warning().Alpha(0.14))
			}
			ui.Spacer(c)
			if meta := ruleMeta(r); meta != "" {
				ui.Text(c, meta).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().Shrink(1).MinWidth(0)
			}
			a.viewSwitch(c, p)
			a.ghostButton(c, "rule-toggle", toggleLabel(st.rules, rule), iconSwitch).
				Clicked(func() { a.actToggleRule(rule, !enabledOf(st.rules, rule)) })
			a.ghostButton(c, "rule-delete", "Delete", iconTrash).Disabled(ruleConfirming == rule).
				Clicked(func() { a.confirmDeleteRule(rule) })
			a.ghostButton(c, "rule-revert", "Revert", iconUndo).Disabled(!dirty).
				Clicked(func() { a.scriptDraft = a.scriptLoadedText(st.rules) })
			a.fillButton(c, "rule-save", "Save", iconSave).Disabled(!dirty).
				Clicked(func() { a.actSaveRule(rule, a.scriptDraft) })
		})
		if a.ruleEditMode == 0 {
			a.ruleOverview(c, p, st, r)
		} else {
			a.ruleCode(c, p)
		}
	})
}

// viewSwitch is the overview and code pair of tabs.
func (a *app) viewSwitch(c *ui.Context, p *palette) {
	tab := func(key, label string, on bool, disabled bool, fn func()) {
		b := ui.ButtonBase(c.Key(key)).Height(24).Padding(0, 10).Radius(6).
			AlignItems(ui.Center).Label(label).Tooltip(label).Disabled(disabled)
		col := p.muted
		switch {
		case on:
			b.Background(p.active)
			col = p.text
		case b.Hovered():
			b.Background(p.hover)
			col = p.text
		}
		b.Transition(hoverFade)
		b.Children(func() { ui.Text(c, label).FontSize(12).FontWeight(550).TextColor(col).SingleLine() })
		if b.Clicked() {
			fn()
		}
	}
	tab("view-overview", "Overview", a.ruleEditMode == 0, ruleVM == nil, func() { a.ruleEditMode = 0 })
	tab("view-code", "Code", a.ruleEditMode == 1, false, func() { a.ruleEditMode = 1 })
}

// syncRuleVM keeps the parsed document in step with the draft: it reparses
// when the text changed.
func (a *app) syncRuleVM() {
	if ruleVMSeen == a.scriptDraft {
		return
	}
	ruleVMSeen = a.scriptDraft
	doc, err := parseRuleDoc(a.scriptDraft)
	if err != nil {
		ruleVM, ruleVMErr = nil, err.Error()
		return
	}
	ruleVM, ruleVMErr = doc, ""
}

// ruleOverview is the rule read as cards: what it matches, its settings,
// and the stages of each phase. It edits nothing; the code view does.
func (a *app) ruleOverview(c *ui.Context, p *palette, st store, r *api.Rule) {
	doc := ruleVM
	if doc == nil {
		if a.scriptDraft == "" {
			return // still loading
		}
		ui.Column(c).Grow(1).Padding(14).Children(func() {
			card(c, p, func() {
				ui.Text(c, "This rule can't be read as an overview.").FontSize(12.5).FontWeight(600).TextColor(p.text)
				ui.Text(c, ruleVMErr).FontSize(11.5).Font(mono()).TextColor(p.removed).FillWidth()
				ui.Text(c, "Its YAML is in the Code view.").FontSize(12).TextColor(p.muted)
			})
		})
		return
	}
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).FillWidth().Padding(14, 16, 24).Gap(14).Children(func() {
			card(c, p, func() {
				cardTitle(c, p, "Match")
				switch {
				case doc.On.Hook != "":
					a.fact(c, p, "Hook", doc.On.Hook)
				case doc.On.Request == nil:
					ui.Text(c, "No request match — this rule never runs.").FontSize(12).TextColor(p.muted)
				default:
					m := doc.On.Request
					any := func(s string) string {
						if s == "" {
							return "any"
						}
						return s
					}
					a.fact(c, p, "Method", any(m.Method))
					a.fact(c, p, "Host", any(m.Host))
					if m.PathPrefix != "" {
						a.fact(c, p, "Path prefix", m.PathPrefix)
					} else {
						a.fact(c, p, "Path", any(m.Path))
					}
				}
			})
			card(c, p, func() {
				cardTitle(c, p, "Settings")
				if doc.Description != "" {
					a.fact(c, p, "Description", doc.Description)
				}
				if r != nil {
					a.fact(c, p, "Priority", fmt.Sprint(r.Priority))
					a.factYesNo(c, p, "Enabled", r.Enabled)
				}
				mode := doc.FailureMode
				if r != nil && r.FailureMode != "" {
					mode = r.FailureMode
				}
				if mode != "" {
					a.fact(c, p, "Failure mode", mode)
				}
				if doc.Timeout != "" {
					a.fact(c, p, "Timeout", doc.Timeout)
				}
				for _, k := range sortedKeys(doc.Vars) {
					a.fact(c, p, "var "+k, paramString(doc.Vars[k]))
				}
			})
			for _, kind := range []string{"request", "response", "frame"} {
				ph := doc.phaseOf(kind)
				if ph == nil || len(ph.Pipeline) == 0 {
					continue
				}
				ui.Column(c.Key("pipe:" + kind)).FillWidth().Gap(6).Children(func() {
					cardTitle(c, p, kind+" pipeline")
					for i := range ph.Pipeline {
						a.stageCard(c, p, &ph.Pipeline[i], fmt.Sprintf("%s:%d", kind, i))
					}
				})
			}
		})
	})
}

// stageCard is one stage, read: what it uses, its id and condition, and
// its parameters as they are written.
func (a *app) stageCard(c *ui.Context, p *palette, s *vmStage, key string) {
	card(c, p, func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			uses := s.stageUses()
			col, bg := methodColor(p, stageTone(uses))
			a.pill(c, uses, col, bg, false)
			if s.ID != "" && s.ID != s.Uses {
				ui.Text(c, s.ID).FontSize(11.5).Font(mono()).TextColor(p.faint).SingleLine()
			}
			if s.If != "" {
				ui.Text(c, "if "+s.If).FontSize(11).Font(mono()).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
			}
			if adv := s.stageNote(); adv != "" {
				ui.Badge(c, adv)
			}
		})
		if s.Run != nil {
			ui.Text(c, strings.TrimSpace(s.Run.Script)).FontSize(11.5).Font(mono()).TextColor(p.muted).FillWidth().Margin(4, 0, 0)
		}
		for _, k := range sortedKeys(s.With) {
			ui.Row(c.Key("with:"+key+":"+k)).FillWidth().Gap(10).AlignItems(ui.Start).Padding(2, 0).Children(func() {
				ui.Text(c, k).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
				ui.Text(c, paramString(s.With[k])).FontSize(12).Font(mono()).TextColor(p.muted).Shrink(1).MinWidth(40)
			})
		}
	})
}

// ruleCode binds the YAML draft to a persistent native editor buffer.
func (a *app) ruleCode(c *ui.Context, p *palette) {
	if ruleCodeState == nil {
		buf := editor.NewBuffer(a.scriptDraft)
		buf.SetLanguage(editor.LanguageFor("rule.yaml"))
		ruleCodeState = &editor.State{Buffer: buf, SoftWrap: true, NoScrollPastEnd: true, Label: "Rule YAML"}
		ruleCodeSeen = a.scriptDraft
	} else if a.scriptDraft != ruleCodeSeen {
		ruleCodeState.Buffer.Reset(a.scriptDraft)
		ruleCodeSeen = a.scriptDraft
	} else if text := ruleCodeState.Buffer.String(); text != ruleCodeSeen {
		a.scriptDraft, ruleCodeSeen = text, text
	}
	ui.Column(c).Grow(1).MinHeight(0).Children(func() {
		editor.View(c.Key("rule:yaml"), ruleCodeState).Grow(1).FillWidth().MinHeight(0).
			Margin(0, 14, 14).Radius(8).Border(1, p.border).Transition(editorFade)
	})
}

// ruleOf is the rule an id names in the snapshot, nil for none.
func ruleOf(rules []api.Rule, id string) *api.Rule {
	for i := range rules {
		if string(rules[i].ID) == id {
			return &rules[i]
		}
	}
	return nil
}

// ruleMeta is the small facts the editor's toolbar keeps by the buttons:
// how the rule fails, which version it is, when it last changed, as one
// quiet mono line, as the marks of a tab read.
func ruleMeta(r *api.Rule) string {
	if r == nil {
		return ""
	}
	var parts []string
	if r.FailureMode == "fail-closed" {
		parts = append(parts, "fail-closed")
	}
	if r.Version > 0 {
		parts = append(parts, fmt.Sprintf("v%d", r.Version))
	}
	if !r.UpdatedAt.IsZero() {
		parts = append(parts, updatedStamp(r.UpdatedAt))
	}
	return strings.Join(parts, " · ")
}

// updatedStamp is a change's time as a stamp: the clock when it was today,
// the day otherwise. The sync goroutine's tick redraws the page, so the
// stamp keeps up without a timer of its own.
func updatedStamp(t time.Time) string {
	t = t.Local()
	now := time.Now()
	if t.Year() == now.Year() && t.Month() == now.Month() && t.Day() == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

func enabledOf(rules []api.Rule, id string) bool {
	for _, r := range rules {
		if string(r.ID) == id {
			return r.Enabled
		}
	}
	return false
}

func toggleLabel(rules []api.Rule, id string) string {
	if enabledOf(rules, id) {
		return "Disable"
	}
	return "Enable"
}

// confirmDeleteRule asks before a delete, which nothing undoes. One
// dialog is out at a time: while it is, the flag keeps Delete quiet, and
// the answer's goroutine drops it through a.post before the next frame.
func (a *app) confirmDeleteRule(id string) {
	if ruleConfirming != "" {
		return
	}
	ruleConfirming = id
	go func() {
		ok, _ := a.askQuestion("Delete rule "+id+"?", "Rules it matched stop running at once.")
		a.post(func() {
			if ruleConfirming == id {
				ruleConfirming = ""
			}
		})
		if ok {
			a.actDeleteRule(id)
		}
	}()
}
