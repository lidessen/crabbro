package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"gopkg.in/yaml.v3"

	"github.com/lidessen/chiling/pkg/api"
)

// The rules page: the rule list and, below it, the editor of the one
// chosen — a visual form and the YAML code, switchable.

// editorFade is how the script editor follows a theme change: its face and
// hairline fade to the new palette rather than swap under the eye.
var editorFade = ui.ElementTransition{Colors: true, Duration: 180 * time.Millisecond}

// ruleConfirming is the rule a native delete dialog is out for, "" for
// none. The frame function sets it when it asks and reads it to quiet the
// Delete button; the dialog's goroutine answers through a.post, which
// clears it on the view thread before the next frame, so one rule asks at
// a time and no second dialog opens while one is up.
var ruleConfirming string

// The visual editor's working state, all of it on the view thread. ruleVM
// is the document the form edits; ruleVMSeen is the script text it was
// parsed from, so the form reparses only when the draft changed behind it;
// ruleVisualBaseline is the form's own marshaling, so formatting alone
// never reads as an edit.
var (
	ruleVM             *vmDoc
	ruleVMSeen         string
	ruleVMErr          string
	ruleVMAdv          string
	ruleVisualBaseline string
	rulePriorityStr    string
	// rulePriorityApplied is the priority the model currently holds, so
	// the store's value and the text field take turns without fighting.
	rulePriorityApplied int
	// ruleParamText mirrors the string parameters an input is editing,
	// keyed by stage and parameter; a map value has no stable address, so
	// the mirror holds its own and writes back into the model each frame.
	ruleParamText    map[string]*string
	ruleCodeState    *editor.State
	ruleCodeSeen     string
	ruleParamEditors map[string]*paramEditor
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
		ruleParamText = nil
		ruleParamEditors = nil
		ruleCodeState = nil
		ruleMethodSel = ""
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

// methodOptions are the match methods the native select offers, "any"
// standing for the empty match.
var methodOptions = []string{"any", "GET", "POST", "PUT", "PATCH", "DELETE", "CONNECT"}

// ruleMethodSel mirrors the match method of the rule in the editor, as
// the select shows it.
var ruleMethodSel string

// methodIsReal tells a select value from the "any" sentinel.
func methodIsReal(sel string) bool {
	for _, opt := range methodOptions {
		if opt == sel && sel != "any" {
			return true
		}
	}
	return false
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
// meta, the visual and code views, and what to do — over the view itself.
func (a *app) ruleEditor(c *ui.Context, p *palette, st store, dirty bool) {
	if a.ruleShownID == "" {
		if len(st.rules) == 0 {
			// There is nothing to choose: the list above already says so.
			ui.Column(c).Grow(1).BorderWidth(1, 0, 0, 0).BorderColor(p.border)
			return
		}
		ui.Column(c).Grow(1).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
			emptyState(c, p, iconSliders, "Select a rule", "Choose a rule to read and edit its script.", "")
		})
		return
	}
	rule := a.ruleShownID
	r := ruleOf(st.rules, rule)

	// The visual model follows the draft: it reparses when the text
	// changed behind it (a load, a save, a revert, code typing) and says
	// whether this rule is one the form can express.
	a.syncRuleVM()
	visualOK := ruleVM != nil && ruleVMErr == "" && ruleVMAdv == ""
	invalidParams := false
	for _, e := range ruleParamEditors {
		invalidParams = invalidParams || e.err != ""
	}
	// Fall back to code only when a real draft is in and the form cannot
	// hold it — not while the load is still in flight.
	if a.ruleEditMode == 0 && a.scriptDraft != "" && !visualOK {
		a.ruleEditMode = 1 // an advanced rule: the form cannot hold it
	}
	// The store's priority is the one that orders rules; a document the
	// API made carries none. The form shows and edits that one, and the
	// baseline follows, so the merge alone is never an edit.
	if ruleVM != nil && ruleVMErr == "" {
		if r := ruleOf(st.rules, rule); r != nil && r.Priority != rulePriorityApplied {
			rulePriorityApplied = r.Priority
			ruleVM.Priority = r.Priority
			rulePriorityStr = strconv.Itoa(r.Priority)
			ruleVisualBaseline = mustMarshalRule(ruleVM)
		}
	}

	ui.Column(c).Grow(1).MinHeight(0).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		ui.Row(c).Height(38).Shrink(0).Padding(0, 14).Gap(8).AlignItems(ui.Center).Children(func() {
			// The rule, as the list names it, its id beside the name as a
			// quiet afterthought, as a tab's marks read.
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				if r != nil && r.Name != "" {
					ui.Text(c, r.Name).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
					ui.Text(c, "·").FontSize(12).Font(mono()).TextColor(p.faint).SingleLine()
					ui.Text(c, rule).FontSize(11.5).Font(mono()).FontWeight(550).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(40)
				} else {
					ui.Text(c, rule).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(40)
				}
			})
			if dirty || invalidParams {
				// The mark of unsaved work: it drops in as editing starts
				// and lifts away as the draft is saved or reverted.
				pillEl(c.Key("rule-edited"), "edited", p.warning(), p.warning().Alpha(0.14))
			}
			ui.Spacer(c)
			if meta := ruleMeta(r); meta != "" {
				ui.Text(c, meta).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().Shrink(1).MinWidth(0)
			}
			a.viewSwitch(c, p, visualOK, invalidParams)
			a.ghostButton(c, "rule-toggle", toggleLabel(st.rules, rule), iconSwitch).
				Clicked(func() { a.actToggleRule(rule, !enabledOf(st.rules, rule)) })
			a.ghostButton(c, "rule-delete", "Delete", iconTrash).Disabled(ruleConfirming == rule).
				Clicked(func() { a.confirmDeleteRule(rule) })
			a.ghostButton(c, "rule-revert", "Revert", iconUndo).Disabled(!dirty && !invalidParams).
				Clicked(func() { a.scriptDraft = a.scriptLoadedText(st.rules); ruleParamEditors = nil })
			// The priority the rule has now, unless the form holds a
			// newer one: a draft the form cannot parse must not save the
			// priority as 0.
			priority := 0
			if r != nil {
				priority = r.Priority
			}
			if ruleVM != nil && ruleVMErr == "" {
				priority = ruleVM.Priority
			}
			a.fillButton(c, "rule-save", "Save", iconSave).Disabled(!dirty || invalidParams).
				Clicked(func() { a.actSaveRule(rule, a.scriptDraft, priority) })
		})
		switch {
		case a.ruleEditMode == 0 && visualOK:
			a.ruleVisual(c, p, st, rule)
		default:
			a.ruleCode(c, p)
		}
	})
}

// viewSwitch is the visual and code pair of tabs. The visual one dims when
// the rule is more than the form can express.
func (a *app) viewSwitch(c *ui.Context, p *palette, visualOK, invalidParams bool) {
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
	tab("view-visual", "Visual", a.ruleEditMode == 0, !visualOK, func() { a.ruleEditMode = 0 })
	tab("view-code", "Code", a.ruleEditMode == 1, invalidParams, func() { a.ruleEditMode = 1 })
}

// syncRuleVM keeps the visual model in step with the draft: it reparses
// when the text changed behind the form, and refreshes the mirrors the
// inputs bind to.
func (a *app) syncRuleVM() {
	if ruleVMSeen == a.scriptDraft {
		return
	}
	doc, err := parseRuleDoc(a.scriptDraft)
	ruleParamText = nil
	ruleParamEditors = nil
	if err != nil {
		ruleVM, ruleVMErr, ruleVMAdv = nil, err.Error(), ""
		ruleVMSeen = a.scriptDraft
		return
	}
	ruleVM, ruleVMErr = doc, ""
	ruleVMAdv = doc.docAdvanced()
	ruleVMSeen = a.scriptDraft
	if ruleVMAdv == "" {
		ruleVisualBaseline = mustMarshalRule(doc)
		rulePriorityStr = strconv.Itoa(doc.Priority)
		rulePriorityApplied = doc.Priority
	}
}

// syncVisual writes the form's document back into the draft, when the form
// has moved off its own baseline. Formatting alone never counts.
func (a *app) syncVisual() {
	if ruleVM == nil {
		return
	}
	fresh := mustMarshalRule(ruleVM)
	if fresh == ruleVisualBaseline {
		return
	}
	ruleVisualBaseline = fresh
	a.scriptDraft = fresh
	ruleVMSeen = fresh
}

// mustMarshalRule marshals the form's document; a model the form built
// cannot fail to serialize.
func mustMarshalRule(doc *vmDoc) string {
	out, err := marshalRuleDoc(doc)
	if err != nil {
		return ""
	}
	return out
}

// ruleVisual is the form: what the rule matches, its description and
// priority, and its pipelines of stages.
func (a *app) ruleVisual(c *ui.Context, p *palette, st store, rule string) {
	doc := ruleVM
	if doc == nil {
		return
	}
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).FillWidth().Padding(14, 16, 24).Gap(14).Children(func() {
			// Every field writes the model, then the model writes the
			// draft: one place syncs, the controls only touch the model.
			a.ruleMatchCard(c, p, doc)
			a.ruleSettingsCard(c, p, doc, st, rule)
			for _, kind := range []string{"request", "response", "frame"} {
				a.rulePipelineCard(c, p, doc, kind)
			}
			a.syncVisual()
		})
	})
}

// ruleMatchCard is what the rule matches on.
func (a *app) ruleMatchCard(c *ui.Context, p *palette, doc *vmDoc) {
	card(c, p, func() {
		cardTitle(c, p, "Match")
		if doc.On.Request == nil {
			ui.Text(c, "No request match — this rule never runs.").FontSize(12).TextColor(p.muted)
			ui.Row(c).Gap(8).Children(func() {
				a.fillButton(c, "match-add", "Add request match", iconPlus).Clicked(func() {
					doc.On.Request = &vmMatch{}
				})
			})
			return
		}
		m := doc.On.Request
		a.matchRow(c, p, "Method", func() {
			// The native select edits a mirror whose "any" stands for the
			// empty match; it lands on the model after the field builds.
			if ruleMethodSel == "" {
				ruleMethodSel = "any"
			}
			if m.Method != "" && m.Method != ruleMethodSel {
				ruleMethodSel = m.Method
			}
			if m.Method == "" && ruleMethodSel != "any" && !methodIsReal(ruleMethodSel) {
				ruleMethodSel = "any"
			}
			ui.Select(c, &ruleMethodSel, methodOptions)
			if sel := ruleMethodSel; (sel == "any") != (m.Method == "") || (sel != "any" && sel != m.Method) {
				m.Method = map[bool]string{true: "", false: sel}[sel != "any"]
			}
		})
		a.matchInput(c, p, "Host", &m.Host, "api.example.com")
		a.matchInput(c, p, "Path", &m.Path, "/v1/login")
		a.matchInput(c, p, "Path prefix", &m.PathPrefix, "/v1/")
	})
}

// matchRow is one labeled row of the match card.
func (a *app) matchRow(c *ui.Context, p *palette, label string, field func()) {
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine().Width(80).Shrink(0)
		field()
	})
}

// matchInput is one text field of the match card.
func (a *app) matchInput(c *ui.Context, p *palette, label string, value *string, placeholder string) {
	a.matchRow(c, p, label, func() {
		ui.TextInput(c, value).Grow(1).Font(mono()).FontSize(12).Placeholder(placeholder)
	})
}

// ruleSettingsCard is the rule's description and priority. Enabled and the
// failure mode stay read-only here — the toolbar toggles enabled, and both
// have their own endpoints.
func (a *app) ruleSettingsCard(c *ui.Context, p *palette, doc *vmDoc, st store, rule string) {
	card(c, p, func() {
		cardTitle(c, p, "Settings")
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Description").FontSize(12).TextColor(p.muted).SingleLine().Width(80).Shrink(0)
			ui.TextInput(c, &doc.Description).Grow(1).FontSize(12).Placeholder("What this rule is for")
		})
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Priority").FontSize(12).TextColor(p.muted).SingleLine().Width(80).Shrink(0)
			ui.TextInput(c, &rulePriorityStr).Width(90).Font(mono()).FontSize(12)
			ui.Spacer(c)
			ui.Text(c, "Enabled").FontSize(12).TextColor(p.muted).SingleLine()
			if enabledOf(st.rules, rule) {
				a.pill(c, "on", p.added, p.added.Alpha(0.13), false)
			} else {
				a.pill(c, "off", p.faint, p.faint.Alpha(0.12), false)
			}
		})
		// The priority text lands on the model as a number; junk keeps the
		// last good value until it parses.
		if n, err := strconv.Atoi(strings.TrimSpace(rulePriorityStr)); err == nil && n != rulePriorityApplied {
			rulePriorityApplied = n
			doc.Priority = n
		}
	})
}

// rulePipelineCard is one phase's stages and the add-stage menu.
func (a *app) rulePipelineCard(c *ui.Context, p *palette, doc *vmDoc, kind string) {
	ph := doc.phaseOf(kind)
	empty := ph == nil || len(ph.Pipeline) == 0
	title := strings.ToUpper(kind) + " PIPELINE"
	ui.Column(c.Key("pipe:" + kind)).FillWidth().Gap(6).Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, title).FontSize(10.5).FontWeight(650).LetterSpacing(0.6).TextColor(p.faint).SingleLine()
			ui.Spacer(c)
			if !empty {
				a.addStageButton(c, p, doc, kind)
			}
		})
		if empty {
			card(c, p, func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "No stages.").FontSize(12).TextColor(p.faint)
					ui.Spacer(c)
					a.addStageButton(c, p, doc, kind)
				})
			})
			return
		}
		for i := range ph.Pipeline {
			a.stageCard(c, p, doc, kind, i)
		}
	})
}

// addStageButton offers the builtin stages; choosing one appends a stage
// the form can edit.
func (a *app) addStageButton(c *ui.Context, p *palette, doc *vmDoc, kind string) {
	b := ui.ButtonBase(c.Key("add:"+kind)).Height(24).Padding(0, 8).Gap(5).Radius(6).
		AlignItems(ui.Center).Label("Add a stage to " + kind).Tooltip("Add a stage")
	if b.Hovered() {
		b.Background(p.hover)
	}
	b.Transition(hoverFade)
	b.Children(func() {
		ui.Icon(c, iconPlus).FontSize(12).TextColor(p.muted)
		ui.Text(c, "Add stage").FontSize(11.5).TextColor(p.muted).SingleLine()
	})
	b.Menu(func(m *ui.Menu) {
		for _, uses := range builtinStages {
			name := strings.TrimPrefix(uses, "chiling.")
			if m.Item(name).Chosen() {
				doc.ensurePhase(kind).Pipeline = append(doc.ensurePhase(kind).Pipeline,
					vmStage{ID: name, Uses: uses, With: map[string]any{}})
			}
		}
	})
}

// stageCard is one stage of the visual editor: its uses, its id and if,
// its with rows — or, for a stage the form cannot express, the read-only
// facts and a pointer at the Code view.
func (a *app) stageCard(c *ui.Context, p *palette, doc *vmDoc, kind string, i int) {
	ph := doc.phaseOf(kind)
	s := &ph.Pipeline[i]
	key := fmt.Sprintf("stage:%s:%d", kind, i)
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
			if adv := s.stageAdvanced(); adv != "" {
				ui.Badge(c, adv)
			}
			ui.Spacer(c)
			a.miniIcon(c, key+":up", iconUp, i > 0).Clicked(func() {
				ph.Pipeline[i-1], ph.Pipeline[i] = ph.Pipeline[i], ph.Pipeline[i-1]
				ruleParamEditors, ruleParamText = nil, nil
			})
			a.miniIcon(c, key+":down", iconDown, i < len(ph.Pipeline)-1).Clicked(func() {
				ph.Pipeline[i+1], ph.Pipeline[i] = ph.Pipeline[i], ph.Pipeline[i+1]
				ruleParamEditors, ruleParamText = nil, nil
			})
			a.miniIcon(c, key+":del", iconX, true).Clicked(func() {
				ph.Pipeline = append(ph.Pipeline[:i], ph.Pipeline[i+1:]...)
				ruleParamEditors, ruleParamText = nil, nil
			})
		})
		if adv := s.stageAdvanced(); adv != "" {
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				if s.Run != nil {
					ui.Text(c, strings.TrimSpace(s.Run.Script)).FontSize(11).Font(mono()).
						TextColor(p.muted).FillWidth()
				}
				ui.Text(c, "Edit in Code.").FontSize(11).TextColor(p.faint)
			})
			return
		}
		edit, shown := scalarParams(s)
		if len(edit) == 0 && len(shown) == 0 {
			ui.Text(c, "No parameters.").FontSize(11.5).TextColor(p.faint)
			return
		}
		ui.Column(c).FillWidth().Gap(6).Children(func() {
			for _, k := range edit {
				kk := k
				if value, ok := s.With[kk].(string); ok && (strings.Contains(strings.ToLower(kk), "body") || strings.Contains(value, "\n")) {
					a.paramCode(c, p, key, s, kk, false)
					continue
				}
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, kk).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
					ui.TextInput(c, paramText(key, s, kk)).Grow(1).Font(mono()).FontSize(12)
					a.miniIcon(c, key+":del:"+kk, iconX, true).Clicked(func() {
						delete(s.With, kk)
						delete(ruleParamText, key+":"+kk)
					})
				})
			}
			for _, k := range shown {
				a.paramCode(c, p, key, s, k, true)
			}
		})
	})
}

// stageTone maps a stage's uses to the pill tone a request method would
// wear, so the stage list reads at a glance.
func stageTone(uses string) string {
	switch {
	case strings.Contains(uses, "mock-response"):
		return "POST"
	case strings.Contains(uses, "block"), strings.Contains(uses, "abort"):
		return "DELETE"
	case strings.Contains(uses, "set-"):
		return "PUT"
	case strings.Contains(uses, "delay"), strings.Contains(uses, "throttle"):
		return "PATCH"
	}
	return "GET"
}

// paramText offers a string parameter's text to an input and writes the
// text back into the model, as its YAML kind: a number stays a number.
func paramText(stageKey string, s *vmStage, name string) *string {
	if ruleParamText == nil {
		ruleParamText = map[string]*string{}
	}
	k := stageKey + ":" + name
	ps, ok := ruleParamText[k]
	if !ok {
		v := paramString(s.With[name])
		ps = &v
		ruleParamText[k] = ps
	}
	if *ps != paramString(s.With[name]) {
		s.With[name] = coerceParam(*ps)
	}
	return ps
}

// miniIcon is a small icon-only button of a stage card. A dead one draws
// faint and takes no clicks, as an edge of the list is.
func (a *app) miniIcon(c *ui.Context, key string, icon *ui.SVG, enabled bool) button {
	p := a.pal
	b := ui.ButtonBase(c.Key(key)).Size(22, 20).Radius(5).Justify(ui.Center).AlignItems(ui.Center)
	col := p.faint
	if enabled {
		switch {
		case b.Hovered():
			b.Background(p.hover)
			col = p.text
		}
	} else {
		b.Disabled(true)
	}
	b.Children(func() { ui.Icon(c, icon).FontSize(12).TextColor(col) })
	return button(b)
}

// paramEditor preserves unfinished structured input until it parses.
type paramEditor struct {
	state *editor.State
	seen  string
	err   string
}

func (a *app) paramCode(c *ui.Context, p *palette, stageKey string, s *vmStage, name string, structured bool) {
	if ruleParamEditors == nil {
		ruleParamEditors = map[string]*paramEditor{}
	}
	key := stageKey + ":" + name
	value := s.With[name]
	text := ""
	filename := "body.txt"
	if structured {
		data, err := yaml.Marshal(value)
		if err != nil {
			return
		}
		text, filename = string(data), "body.yaml"
	} else {
		text = value.(string)
	}
	e := ruleParamEditors[key]
	if e == nil || e.seen != text {
		buf := editor.NewBuffer(text)
		buf.SetLanguage(editor.LanguageFor(filename))
		e = &paramEditor{state: &editor.State{Buffer: buf, SoftWrap: true, NoScrollPastEnd: true, Label: name}, seen: text}
		ruleParamEditors[key] = e
	}
	current := e.state.Buffer.String()
	if current == text {
		e.err = ""
	}
	if current != text {
		if structured {
			var parsed any
			if err := yaml.Unmarshal([]byte(current), &parsed); err != nil {
				e.err = err.Error()
			} else {
				// Keep a mapping or sequence a mapping or sequence.
				valid := false
				switch value.(type) {
				case map[string]any:
					_, valid = parsed.(map[string]any)
				case []any:
					_, valid = parsed.([]any)
				}
				if !valid {
					e.err = "Keep this value a mapping or sequence."
				} else {
					s.With[name], e.err = parsed, ""
					if b, err := yaml.Marshal(parsed); err == nil {
						e.seen = string(b)
					}
				}
			}
		} else {
			s.With[name], e.seen = current, current
		}
	}
	ui.Column(c).FillWidth().Gap(5).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, name).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text)
			ui.Spacer(c)
			a.miniIcon(c, key+":del", iconX, true).Clicked(func() { delete(s.With, name); delete(ruleParamEditors, key) })
		})
		editor.View(c.Key("param:"+key), e.state).FillWidth().Height(140).MinWidth(0)
		if e.err != "" {
			ui.Text(c, e.err).FontSize(11).TextColor(p.removed).FillWidth()
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
