package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"github.com/lidessen/crabbro/pkg/api"
)

// The rules page: the rules as a column, each under its mark,
// and beside it the one chosen — an overview to read it by and the YAML
// code to edit it in.

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

// ruleListW is the width of the column of rules, as the page's splitter
// keeps it.
var ruleListW float32 = 276

// rulesPage is the column of rules and, beside it, the one chosen.
func (a *app) rulesPage(c *ui.Context, p *palette, st store) {
	ui.Column(c).Grow(1).MinHeight(0).Padding(18, workspaceInset, 16).Gap(12).Children(func() {
		workspaceTitle(c, p, "Rules").Shrink(0)
		a.rulesWorkspace(c, p, st)
	})
}

func (a *app) rulesWorkspace(c *ui.Context, p *palette, st store) {
	if len(st.rules) == 0 {
		a.ruleShownID = ""
		panel(c, p).Grow(1).Children(func() { a.rulesEmpty(c, p) })
		return
	}
	if ruleOf(st.rules, a.ruleShownID) == nil {
		a.chooseRule(st.rules[0])
	}
	dirty := a.ruleShownID != "" && a.scriptDraft != "" && a.scriptDraft != a.scriptLoadedText(st.rules)

	ui.Split(c, &ruleListW,
		func() {
			ui.Column(c).Fill().Padding(0, 6, 0, 0).Children(func() {
				panel(c.Key("rules:list"), p).Grow(1).MinHeight(0).Children(func() { a.ruleList(c, p, st) })
			})
		},
		func() {
			ui.Column(c).Fill().Padding(0, 0, 0, 6).Children(func() {
				panel(c.Key("rules:editor"), p).Grow(1).MinHeight(0).Children(func() { a.ruleEditor(c, p, st, dirty) })
			})
		},
	).Grow(1).MinHeight(0)
}

// ruleList is the rules as a column: each under its mark, filled
// while it is enabled and in outline while it is disabled, with what
// it matches and what it does in a line below its name.
func (a *app) ruleList(c *ui.Context, p *palette, st store) {
	inForce := 0
	for _, r := range st.rules {
		if r.Enabled {
			inForce++
		}
	}
	ui.Column(c).Fill().MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Shrink(0).Padding(14, 16, 10).Gap(5).Children(func() {
			ui.Text(c, "Rule library").FontSize(14).FontWeight(600).TextColor(p.text).SingleLine()
			sub := fmt.Sprintf("%d enabled", inForce)
			if off := len(st.rules) - inForce; off > 0 {
				sub += fmt.Sprintf("  ·  %d disabled", off)
			}
			ui.Text(c, sub).FontSize(12).TextColor(p.muted).SingleLine()
		})
		hairline(c, p)
		ui.Scroll(c).Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().Padding(8, 8, 16).Gap(1).Children(func() {
				for _, r := range st.rules {
					a.ruleRow(c, p, r)
				}
			})
		})
	})
}

// ruleRow is one rule of the column.
func (a *app) ruleRow(c *ui.Context, p *palette, r api.Rule) {
	on := string(r.ID) == a.ruleShownID
	b := ui.ButtonBase(c.Key("rule:"+string(r.ID))).FillWidth().Height(58).Padding(0, 10, 0, 12).Gap(9).
		Radius(8).AlignItems(ui.Center).Label(ruleLabel(r)).Selected(on)
	switch {
	case on:
		// The chosen rule keeps a fine line of the accent round its tint,
		// which holds the row's edge on a ground nearly its own color.
		b.Background(p.active).InsetShadow(0, 0, 0, 1, p.accent.Alpha(0.14))
	case b.Pressed():
		b.Background(p.text.Alpha(0.06))
	case b.Hovered():
		b.Background(p.hover)
	}
	b.Transition(hoverFade)
	b.Children(func() {
		ruleMark(c, p, 18, r.Enabled)
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				col := p.text
				if !r.Enabled {
					col = p.muted
				}
				ui.Text(c, ruleLabel(r)).FontSize(13).FontWeight(600).TextColor(col).SingleLine().Shrink(1).MinWidth(20)
				if r.FailureMode == "fail-closed" {
					tag(c, p, "fail-closed")
				}
				// The priority rides the name's line, so the summary below
				// keeps the row's whole width for what the rule does.
				ui.Spacer(c)
				ui.Text(c, fmt.Sprint(r.Priority)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().Shrink(0).
					Tooltip("Priority")
			})
			summary := ruleSummary(r)
			ui.RichText(c, dataSpans(summary, p.text.Mix(p.faint, 0.8), p.faint.Alpha(0.55))...).
				Font(mono()).FontSize(11).SingleLine().Tooltip(summary)
		})
	})
	if b.Clicked() && !on {
		a.chooseRule(r)
	}
}

// ruleSummaries keeps each rule's one-line summary by the script it was
// read from, so the column does not parse YAML every frame.
var ruleSummaries = map[string]struct{ script, text string }{}

// ruleSummary is a rule in one line: what it matches, and what it does.
func ruleSummary(r api.Rule) string {
	if s, ok := ruleSummaries[string(r.ID)]; ok && s.script == r.Script {
		return s.text
	}
	text := string(r.ID)
	if doc, err := parseRuleDoc(r.Script); err == nil {
		text = doc.summary()
	}
	ruleSummaries[string(r.ID)] = struct{ script, text string }{r.Script, text}
	return text
}

// rulesEmpty is what the list shows while there are no rules: a quiet
// sliders mark and the command that scaffolds the first one from a
// capture, where the empty table would sit.
func (a *app) rulesEmpty(c *ui.Context, p *palette) {
	emptyState(c, p, iconSliders, "No rules yet",
		"Rules rewrite, mock or block traffic. Scaffold the first one from a captured request.",
		"crabbro rules from req_1 --action mock")
}

// The rule list already includes the script. Keep the draft tied to its ID
// as snapshots arrive so changes to list order cannot replace the draft.
func (a *app) chooseRule(r api.Rule) {
	a.ruleShownID, a.scriptDraft = string(r.ID), r.Script
	ruleVMSeen, ruleVM, ruleCodeState = "", nil, nil
	a.invalidate()
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

// ruleLabel is what the list names a rule: its name, else its id.
func ruleLabel(r api.Rule) string {
	if r.Name != "" {
		return r.Name
	}
	return string(r.ID)
}

// ruleEditor is the rule the list chose: its name set as a heading with
// what to do with it at its right, then the overview and the code as two
// words to choose between, over the view itself. The overview reads the
// rule; the YAML in the code view is what is edited.
func (a *app) ruleEditor(c *ui.Context, p *palette, st store, dirty bool) {
	if a.ruleShownID == "" {
		emptyState(c, p, iconSliders, "Select a rule", "Choose a rule to read it, or edit its YAML.", "")
		return
	}
	rule := a.ruleShownID
	r := ruleOf(st.rules, rule)

	a.syncRuleVM()
	// A draft the overview cannot read opens in the code view.
	if a.ruleEditMode == 0 && a.scriptDraft != "" && ruleVM == nil {
		a.ruleEditMode = 1
	}

	ui.Column(c).Fill().MinHeight(0).Children(func() {
		ui.Row(c).FillWidth().Shrink(0).Padding(14, 16, 8).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Gap(4).Grow(1).MinWidth(0).Children(func() {
				name := rule
				if r != nil && r.Name != "" {
					name = r.Name
				}
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, name).FontSize(16).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
					if dirty {
						stateDot(c, p, p.warning(), "Edited")
					}
				})
				// The id and the small facts after it, each its own word
				// with one point between: set as one mono string the
				// points take a whole cell each and the line reads gapped.
				ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
					for i, part := range append([]string{rule}, ruleMeta(r)...) {
						if i > 0 {
							ui.Text(c, "·").FontSize(11).TextColor(p.faint.Alpha(0.7)).SingleLine().Shrink(0)
						}
						t := ui.Text(c, part).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
						if i == 0 {
							t.Shrink(1).MinWidth(20).Selectable()
						} else {
							t.Shrink(0)
						}
					}
				})
			})
		})
		ui.Row(c).FillWidth().Shrink(0).Padding(0, 12).Gap(4).Wrap().AlignItems(ui.Center).Children(func() {
			a.words(c, "rule-view", &a.ruleEditMode, "Overview", "YAML")
			ui.Spacer(c)
			ui.Row(c).Gap(4).Margin(0, 0, 4).AlignItems(ui.Center).Children(func() {
				a.ghostButton(c, "rule-toggle", toggleLabel(st.rules, rule), iconSwitch).
					Clicked(func() { a.actToggleRule(rule, !enabledOf(st.rules, rule)) })
				a.ghostButton(c, "rule-delete", "Delete", iconTrash).Disabled(ruleConfirming == rule).
					Clicked(func() { a.confirmDeleteRule(rule) })
				if dirty {
					a.ghostButton(c, "rule-revert", "Revert", iconUndo).
						Clicked(func() { a.scriptDraft = a.scriptLoadedText(st.rules) })
				}
				a.fillButton(c, "rule-save", "Save", iconSave, !dirty).
					Clicked(func() { a.actSaveRule(rule, a.scriptDraft) })
			})
		})
		hairline(c, p)
		if a.ruleEditMode == 0 {
			a.ruleOverview(c, p, r)
		} else {
			a.ruleCode(c, p)
		}
	})
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

// ruleOverview is the rule read in plain groups: when it applies, how it
// is set, and what each phase does, stage by stage. It edits nothing; the
// code view does.
func (a *app) ruleOverview(c *ui.Context, p *palette, r *api.Rule) {
	doc := ruleVM
	if doc == nil {
		if a.scriptDraft == "" {
			ui.Text(c, "No rule source available.").FontSize(13).TextColor(p.muted).Padding(18, 24)
			return
		}
		ui.Column(c).Grow(1).Padding(18, 24).Gap(6).Children(func() {
			ui.Text(c, "This rule can't be read as an overview.").FontSize(13).FontWeight(600).TextColor(p.text)
			ui.Text(c, ruleVMErr).FontSize(11).Font(mono()).TextColor(p.removed).FillWidth()
			ui.Text(c, "Open the YAML tab to view the source.").FontSize(13).TextColor(p.muted)
		})
		return
	}
	group := func(title string, rows func()) {
		ui.Column(c.Key("group:" + title)).FillWidth().Gap(8).Children(func() {
			caption(c, p, title)
			ui.Column(c).FillWidth().Gap(7).Children(rows)
		})
	}
	ui.Scroll(c).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().MaxWidth(860).Padding(18, 24, 28).Gap(18).Children(func() {
			group("Applies when", func() {
				switch {
				case doc.On.Hook != "":
					a.fact(c, p, "Hook", doc.On.Hook)
				case doc.On.Request == nil:
					ui.Text(c, "No request match — this rule never runs.").FontSize(13).TextColor(p.muted)
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
			hairline(c, p)
			group("Settings", func() {
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
				hairline(c, p)
				group("On the "+kind, func() {
					for i := range ph.Pipeline {
						a.stageBlock(c, p, &ph.Pipeline[i], i, fmt.Sprintf("%s:%d", kind, i))
					}
				})
			}
		})
	})
}

// stageBlock is one stage, read: its place in the pipeline, what it uses,
// its id and condition, and its parameters as they are written.
func (a *app) stageBlock(c *ui.Context, p *palette, s *vmStage, n int, key string) {
	ui.Row(c.Key("stage:" + key)).FillWidth().Gap(12).AlignItems(ui.Start).Children(func() {
		ui.Text(c, fmt.Sprint(n+1)).FontSize(11).Font(mono()).TextColor(p.faint).Width(14).Padding(2, 0, 0).SingleLine().Shrink(0)
		ui.Column(c).Grow(1).MinWidth(0).Gap(5).Children(func() {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, s.stageUses()).FontSize(12).FontWeight(600).Font(mono()).TextColor(p.text).SingleLine().Shrink(0)
				if s.ID != "" && s.ID != s.Uses {
					ui.Text(c, s.ID).FontSize(12).Font(mono()).TextColor(p.faint).SingleLine()
				}
				if s.If != "" {
					ui.Text(c, "if "+s.If).FontSize(11).Font(mono()).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
				}
				if adv := s.stageNote(); adv != "" {
					tag(c, p, adv)
				}
			})
			if s.Run != nil {
				ui.Text(c, strings.TrimSpace(s.Run.Script)).FontSize(11).Font(mono()).TextColor(p.muted).FillWidth()
			}
			for _, k := range sortedKeys(s.With) {
				ui.Row(c.Key("with:" + key + ":" + k)).FillWidth().Gap(10).AlignItems(ui.Start).Children(func() {
					ui.Text(c, k).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine().Width(96).Shrink(0)
					dataText(c, p, paramString(s.With[k])).LineHeight(1.5).Shrink(1).MinWidth(40)
				})
			}
		})
	})
}

// ruleCode binds the YAML draft to a persistent native editor buffer.
func (a *app) ruleCode(c *ui.Context, p *palette) {
	if ruleCodeState == nil {
		buf := editor.NewBuffer(a.scriptDraft)
		buf.SetLanguage(editor.LanguageFor("rule.yaml"))
		ruleCodeState = &editor.State{Buffer: buf, Font: codeFont(), LineHeight: 1.6, SoftWrap: true, NoScrollPastEnd: true, Label: "Rule YAML"}
		ruleCodeSeen = a.scriptDraft
	} else if a.scriptDraft != ruleCodeSeen {
		ruleCodeState.Buffer.Reset(a.scriptDraft)
		ruleCodeSeen = a.scriptDraft
	} else if text := ruleCodeState.Buffer.String(); text != ruleCodeSeen {
		a.scriptDraft, ruleCodeSeen = text, text
	}
	ruleCodeState.Theme = p.codeTheme(c, false)
	ui.Column(c).Grow(1).MinHeight(0).Children(func() {
		editor.View(c.Key("rule:yaml"), ruleCodeState).Grow(1).FillWidth().MinHeight(0).
			Margin(8, 8, 0).Transition(editorFade)
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
// how the rule fails, which version it is, when it last changed, each a
// word of one quiet mono line.
func ruleMeta(r *api.Rule) []string {
	if r == nil {
		return nil
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
	return parts
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
