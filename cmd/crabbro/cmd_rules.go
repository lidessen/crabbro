package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

// ---------------------------------------------------------------------------
// Rules commands
// ---------------------------------------------------------------------------

func cmdRules(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules <command> [options]\n\nCommands: list, diagnose, create, get, enable, disable, delete, test, fixture")
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "list":
		return cmdRulesList(cfg, subArgs)
	case "diagnose":
		return cmdRulesDiagnose(cfg, subArgs)
	case "create":
		return cmdRulesCreate(cfg, subArgs)
	case "get":
		return cmdRulesGet(cfg, subArgs)
	case "enable":
		return cmdRulesEnable(cfg, subArgs)
	case "disable":
		return cmdRulesDisable(cfg, subArgs)
	case "delete":
		return cmdRulesDelete(cfg, subArgs)
	case "test":
		return cmdRulesTest(cfg, subArgs)
	case "fixture":
		return cmdRulesFixture(cfg, subArgs)
	case "from":
		return cmdRulesFrom(cfg, subArgs)
	case "draft":
		return cmdRulesDraft(cfg, subArgs)
	case "validate":
		return cmdRulesValidate(subArgs)
	case "migrate":
		return cmdRulesMigrate(subArgs)
	case "preview":
		return cmdRulesPreview(cfg, subArgs)
	default:
		return fmt.Errorf("unknown rules command: %s", sub)
	}
}

func cmdRulesDiagnose(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("rules diagnose", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: crabbro rules diagnose [--json]")
	}
	raw, err := gatewayFetch(cfg, http.MethodGet, "/v1/rules/diagnose", nil)
	if err != nil {
		return err
	}
	if *jsonOutput {
		fmt.Println(string(raw))
		return nil
	}
	var result rulesDiagnoseResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	printRulesDiagnose(os.Stdout, result)
	return nil
}

type rulesDiagnoseResponse struct {
	Complete bool              `json:"complete"`
	Files    []rule.FileStatus `json:"files"`
}

func printRulesDiagnose(w io.Writer, result rulesDiagnoseResponse) {
	fmt.Fprintf(w, "complete=%t files=%d\n", result.Complete, len(result.Files))
	const limit = 20
	for i, f := range result.Files {
		if i == limit {
			fmt.Fprintf(w, "  ... %d more files (use --json)\n", len(result.Files)-limit)
			break
		}
		fmt.Fprintf(w, "  %s live=%t draft=%t enabled=%t diagnostics=%d\n", boundedText(f.ID), f.Live, f.Draft, f.Enabled, len(f.Diagnostics))
		for j, d := range f.Diagnostics {
			if j == limit {
				fmt.Fprintf(w, "    ... %d more diagnostics (use --json)\n", len(f.Diagnostics)-limit)
				break
			}
			fmt.Fprintf(w, "    %s: %s\n", boundedText(d.Path), boundedText(d.Message))
		}
	}
}

// boundedText keeps diagnostic fields short and on one line.
func boundedText(s string) string {
	r := []rune(strings.Map(func(c rune) rune {
		if c < ' ' || c == 127 {
			return ' '
		}
		return c
	}, s))
	if len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return string(r)
}

// cmdRulesList handles `crabbro rules list [--json|--pretty]`
func cmdRulesList(cfg *clientConfig, args []string) error {
	pretty := false
	for _, a := range args {
		if a == "--pretty" {
			pretty = true
		}
	}

	resp, err := doRequest(cfg, http.MethodGet, "/v1/rules", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	if pretty {
		var data shared.ListRulesResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		printPrettyRules(data.Rules)
	} else {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func printPrettyRules(rules []*shared.Rule) {
	if len(rules) == 0 {
		fmt.Println("No rules configured.")
		return
	}
	fmt.Printf("%-30s %-20s %-10s %-8s %s\n", "ID", "Name", "Priority", "Enabled", "Updated")
	fmt.Println(strings.Repeat("-", 90))
	for _, r := range rules {
		enabled := "yes"
		if !r.Enabled {
			enabled = "no"
		}
		name := r.Name
		if name == "" {
			name = "-"
		}
		fmt.Printf("%-30s %-20s %-10d %-8s %s\n", string(r.ID), name, r.Priority, enabled, r.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
}

// cmdRulesCreate handles `crabbro rules create <file> [--name <name>] [--priority <n>]`
func cmdRulesCreate(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules create <script-file> [--name <name>] [--priority <n>]")
	}

	filePath := args[0]
	name := ""
	priority := 0

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case "--priority":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &priority)
			}
		}
	}

	script, err := readRuleFile(filePath)
	if err != nil {
		return err
	}

	body := shared.CreateRuleRequest{
		Name:     name,
		Priority: priority,
		Script:   string(script),
	}

	jsonBody, _ := json.Marshal(body)
	resp, err := doRequest(cfg, http.MethodPost, "/v1/rules", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	return nil
}

// cmdRulesGet handles `crabbro rules get <id> [--json|--pretty]`
func cmdRulesGet(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules get <id> [--pretty]")
	}

	id := args[0]
	pretty := false
	for _, a := range args[1:] {
		if a == "--pretty" {
			pretty = true
		}
	}

	resp, err := doRequest(cfg, http.MethodGet, "/v1/rules/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	if pretty {
		var data shared.GetRuleResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		printPrettyRule(&data.Rule)
	} else {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func printPrettyRule(r *shared.Rule) {
	enabled := "yes"
	if !r.Enabled {
		enabled = "no"
	}
	fmt.Printf("ID:        %s\n", string(r.ID))
	fmt.Printf("Name:      %s\n", r.Name)
	fmt.Printf("Priority:  %d\n", r.Priority)
	fmt.Printf("Enabled:   %s\n", enabled)
	fmt.Printf("Created:   %s\n", r.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("Updated:   %s\n", r.UpdatedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("Script:\n%s\n", r.Script)
}

// cmdRulesEnable handles `crabbro rules enable <id>`
func cmdRulesEnable(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules enable <id>")
	}

	return toggleRule(cfg, args[0], true)
}

// cmdRulesDisable handles `crabbro rules disable <id>`
func cmdRulesDisable(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules disable <id>")
	}

	return toggleRule(cfg, args[0], false)
}

func toggleRule(cfg *clientConfig, id string, enabled bool) error {
	body := shared.ToggleRuleRequest{Enabled: enabled}
	jsonBody, _ := json.Marshal(body)

	resp, err := doRequest(cfg, http.MethodPost, "/v1/rules/"+id+"/toggle", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	fmt.Printf("Rule %s %s\n", id, map[bool]string{true: "enabled", false: "disabled"}[enabled])
	return nil
}

// cmdRulesDelete handles `crabbro rules delete <id>`
func cmdRulesDelete(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules delete <id>")
	}

	resp, err := doRequest(cfg, http.MethodDelete, "/v1/rules/"+args[0], nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	fmt.Printf("Rule %s deleted\n", args[0])
	return nil
}

// cmdRulesTest handles `crabbro rules test <rule-file> --fixture <name> [--json|--pretty]`
func cmdRulesTest(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules test <rule-file> --fixture <name> [--pretty]")
	}

	filePath := args[0]
	fixtureName := ""
	pretty := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--fixture":
			if i+1 < len(args) {
				i++
				fixtureName = args[i]
			}
		case "--pretty":
			pretty = true
		}
	}

	if fixtureName == "" {
		return fmt.Errorf("--fixture <name> is required")
	}

	script, err := readRuleFile(filePath)
	if err != nil {
		return err
	}

	body := shared.TestRuleRequest{
		RuleScript: string(script),
		Fixture:    fixtureName,
	}

	jsonBody, _ := json.Marshal(body)
	resp, err := doRequest(cfg, http.MethodPost, "/v1/rules/test", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	if pretty {
		var data shared.TestRuleResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		printPrettyTestResult(data)
	} else {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func printPrettyTestResult(r shared.TestRuleResponse) {
	fmt.Printf("Matched:    %t\n", r.Matched)
	fmt.Printf("Duration:   %d ms\n", r.DurationMs)
	fmt.Printf("Valid:      %t\n", r.SchemaValid)
	if len(r.SchemaErrs) > 0 {
		fmt.Printf("Schema err: %s\n", strings.Join(r.SchemaErrs, "; "))
	}
	if r.Error != "" {
		fmt.Printf("Error:      %s\n", r.Error)
	}
	if len(r.Actions) > 0 {
		fmt.Println("Actions:")
		for _, a := range r.Actions {
			paramsJSON, _ := json.Marshal(a.Params)
			fmt.Printf("  - %s %s\n", a.Type, string(paramsJSON))
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture commands (under `crabbro rules fixture`)
// ---------------------------------------------------------------------------

func cmdRulesFixture(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules fixture <command> [options]\n\nCommands: save, list, delete")
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "save":
		return cmdFixtureSave(cfg, subArgs)
	case "list":
		return cmdFixtureList(cfg, subArgs)
	case "delete":
		return cmdFixtureDelete(cfg, subArgs)
	default:
		return fmt.Errorf("unknown fixture command: %s", sub)
	}
}

// cmdFixtureSave handles `crabbro rules fixture save <request-id> <name>`
func cmdFixtureSave(cfg *clientConfig, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: crabbro rules fixture save <request-id> <name>")
	}

	body := map[string]string{
		"requestId": args[0],
		"name":      args[1],
	}
	jsonBody, _ := json.Marshal(body)

	resp, err := doRequest(cfg, http.MethodPost, "/v1/fixtures", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	fmt.Printf("Fixture %q saved\n", args[1])
	return nil
}

// cmdFixtureList handles `crabbro rules fixture list [--json|--pretty]`
func cmdFixtureList(cfg *clientConfig, args []string) error {
	pretty := false
	for _, a := range args {
		if a == "--pretty" {
			pretty = true
		}
	}

	resp, err := doRequest(cfg, http.MethodGet, "/v1/fixtures", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	if pretty {
		var fixtures []shared.FixtureSummary
		if err := json.NewDecoder(resp.Body).Decode(&fixtures); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if len(fixtures) == 0 {
			fmt.Println("No fixtures saved.")
			return nil
		}
		fmt.Printf("%-30s %-8s %-30s %s\n", "Name", "Method", "Host", "Path")
		fmt.Println(strings.Repeat("-", 80))
		for _, f := range fixtures {
			fmt.Printf("%-30s %-8s %-30s %s\n", f.Name, f.Method, f.Host, f.Path)
		}
	} else {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

// cmdFixtureDelete handles `crabbro rules fixture delete <name>`
func cmdFixtureDelete(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules fixture delete <name>")
	}

	name := args[0]
	resp, err := doRequest(cfg, http.MethodDelete, "/v1/fixtures/"+name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	fmt.Printf("Fixture %q deleted\n", name)
	return nil
}

func cmdRulesFrom(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("rules from", flag.ContinueOnError)
	action := fs.String("action", "mock", "mock, block, header, rewrite, or tag")
	name := fs.String("name", "", "Rule name")
	apply := fs.Bool("apply", true, "Create the rule on the daemon")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: crabbro rules from <request-id> [--action mock] [--name n] [--apply=false]")
	}
	id := fs.Arg(0)
	resp, err := doRequest(cfg, http.MethodGet, "/v1/requests/"+id+"?include=body", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}
	var detail shared.RequestDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return err
	}
	body := ""
	if detail.ResponseBody != nil {
		body = *detail.ResponseBody
	}
	ctype := ""
	if detail.Request.ResponseHeaders != nil {
		ctype = detail.Request.ResponseHeaders["Content-Type"]
	}
	script := rule.Scaffold(&detail.Request, *action, body, ctype)
	ruleName := *name
	if ruleName == "" {
		ruleName = "from-" + string(detail.Request.ID)
	}
	if !*apply {
		fmt.Print(script)
		return nil
	}
	payload, _ := json.Marshal(shared.CreateRuleRequest{Name: ruleName, Script: script})
	return printAPI(cfg, http.MethodPost, "/v1/rules", bytes.NewReader(payload))
}

func cmdRulesDraft(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("rules draft", flag.ContinueOnError)
	from := fs.String("from", "", "Captured request id to use as context")
	apply := fs.Bool("apply", false, "Create the drafted rule")
	fs.Parse(args)
	desc := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if desc == "" {
		return fmt.Errorf("usage: crabbro rules draft \"<description>\" [--from <id>] [--apply]")
	}
	payload, _ := json.Marshal(map[string]any{
		"description": desc,
		"requestId":   *from,
		"apply":       *apply,
	})
	return printAPI(cfg, http.MethodPost, "/v1/rules/draft", bytes.NewReader(payload))
}

func cmdRulesValidate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro rules validate <file>")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	diags := rule.Validate(string(b), true, nil)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"ok": len(diags) == 0, "diagnostics": diags})
}

func cmdRulesMigrate(args []string) error {
	fs := flag.NewFlagSet("rules migrate", flag.ContinueOnError)
	id := fs.String("id", "migrated", "Rule id for the wrapper")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: crabbro rules migrate <file.js> [--id name]")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	out, err := rule.Migrate(*id, string(b))
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdRulesPreview(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("rules preview", flag.ContinueOnError)
	method := fs.String("method", "GET", "Request method")
	host := fs.String("host", "", "Host")
	path := fs.String("path", "/", "Path")
	fs.Parse(args)
	payload, _ := json.Marshal(map[string]any{"method": *method, "host": *host, "path": *path})
	return printAPI(cfg, http.MethodPost, "/v1/rules/preview", bytes.NewReader(payload))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readBody reads the full response body into a string.
func readBody(r io.Reader) string {
	data, _ := io.ReadAll(r)
	return strings.TrimSpace(string(data))
}

// readRuleFile reads a rule script file. Rules are YAML rule v1 documents;
// a .js file is legacy JavaScript, which no rule path accepts anymore —
// say so and point at the converter.
func readRuleFile(path string) (string, error) {
	if strings.HasSuffix(strings.ToLower(path), ".js") {
		return "", fmt.Errorf("%s is legacy JavaScript; rules are YAML — convert it first: crabbro rules migrate %s", path, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading rule file: %w", err)
	}
	return string(b), nil
}
