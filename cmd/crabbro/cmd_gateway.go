package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

func cmdGateway(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro gw <status|plans|routes|quotas|usage|cooldown-clear|preview|reload>")
	}
	switch args[0] {
	case "status":
		return gwGetJSON(cfg, args[1:], "/v1/gateway/status", func(raw []byte) {
			var st shared.GatewayStatusResponse
			if json.Unmarshal(raw, &st) != nil {
				fmt.Println(string(raw))
				return
			}
			fmt.Printf("enabled=%t addr=%s gen=%d plans=%d routes=%d healthy=%d cooling=%d\n",
				st.Enabled, st.ListenAddr, st.SnapshotGeneration, st.PlanCount, st.RouteCount, st.HealthyPlans, st.CoolingPlans)
			for _, d := range st.Diagnostics {
				fmt.Printf("  diag %s %s: %s\n", d.File, d.Path, d.Message)
			}
		})
	case "plans":
		return gwPlansJSON(cfg, args[1:], func(raw []byte) {
			var out shared.GatewayPlanListResponse
			if json.Unmarshal(raw, &out) != nil {
				fmt.Println(string(raw))
				return
			}
			if len(out.Plans) == 0 {
				fmt.Println("no gateway plans configured")
				return
			}
			for _, p := range out.Plans {
				health := "healthy"
				if !p.Healthy && p.CooldownUntil != nil {
					health = fmt.Sprintf("cooling(%s until %s)", p.CooldownScope, p.CooldownUntil.Local().Format("15:04:05"))
				}
				sec := "missing"
				if p.HasSecret {
					sec = "set"
				}
				direct := "off"
				if p.DirectEnabled {
					direct = "on"
				}
				if len(p.DirectProfiles) > 0 {
					direct += " (profiles: " + strings.Join(p.DirectProfiles, ",") + ")"
				}
				fmt.Printf("%-18s enabled=%t secret=%-7s health=%-24s endpoints=%s models=%s\n",
					p.ID, p.Enabled, sec, health, strings.Join(p.Endpoints, ","), strings.Join(p.Models, ","))
				fmt.Printf("  direct: %s\n", direct)
				if ar := p.AutoReset; ar != nil {
					horizon := ar.ExpiringWithin
					if horizon == "" {
						horizon = "any"
					}
					interval := ar.MinInterval
					if interval == "" {
						interval = "5m"
					}
					fmt.Printf("  autoReset: on scopes=%s expiringWithin=%s minInterval=%s\n",
						strings.Join(ar.Scopes, ","), horizon, interval)
				}
				if q := p.RemoteQuota; q != nil {
					fmt.Printf("  remote balance=%s resets=%d source=%s stale=%t", q.Balance, q.ResetsRemaining, q.SyncSource, q.Stale)
					if q.SoonestResetExpiry != nil {
						fmt.Printf(" expires=%s", q.SoonestResetExpiry.Format(time.RFC3339))
					}
					fmt.Println()
					for _, w := range q.Windows {
						if w.UsagePct != nil {
							fmt.Printf("  remote window %s used=%.0f%%\n", w.Name, *w.UsagePct)
						}
					}
				}
				for _, w := range p.Windows {
					fmt.Printf("  window %-8s (%s %s) tokens=%d/%d reqs=%d/%d rem=%.0f%% surplus=%+.2f src=%s\n",
						w.Name, w.Duration, w.Mode, w.UsedTokens, w.MaxTokens, w.UsedRequests, w.MaxRequests,
						w.RemainingRatio*100, w.Surplus, w.Source)
				}
			}
		})
	case "routes":
		return gwGetJSON(cfg, args[1:], "/v1/gateway/routes", func(raw []byte) {
			var out shared.GatewayRouteListResponse
			if json.Unmarshal(raw, &out) != nil {
				fmt.Println(string(raw))
				return
			}
			if len(out.Routes) == 0 {
				fmt.Println("no gateway routes configured")
				return
			}
			for _, rt := range out.Routes {
				var tgts []string
				for _, t := range rt.Targets {
					tgts = append(tgts, t.Plan+"/"+t.Model)
				}
				fmt.Printf("%-18s family=%-10s preset=%-16s enabled=%t models=%v targets=%s\n",
					rt.ID, rt.Family, rt.Preset, rt.Enabled, rt.Models, strings.Join(tgts, ", "))
			}
		})
	case "quotas":
		return gwQuotas(cfg, args[1:])
	case "usage":
		fs := flag.NewFlagSet("gw usage", flag.ContinueOnError)
		window := fs.String("window", "24h", "Rolling duration window (e.g. 1h, 24h, 7d)")
		plan := fs.String("plan", "", "Filter by plan ID")
		pretty := fs.Bool("pretty", false, "Human-readable output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		q := url.Values{}
		q.Set("window", *window)
		if *plan != "" {
			q.Set("plan", *plan)
		}
		path := "/v1/gateway/usage?" + q.Encode()
		if !*pretty {
			return printAPI(cfg, http.MethodGet, path, nil)
		}
		raw, err := gwFetch(cfg, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var out shared.GatewayUsageResponse
		if json.Unmarshal(raw, &out) != nil {
			fmt.Println(string(raw))
			return nil
		}
		fmt.Printf("window=%s total_reqs=%d errors=%d total_tokens=%d in=%d out=%d cache_read=%d avg_ttft=%.0fms\n",
			out.Window, out.Totals.Requests, out.Totals.Errors, out.Totals.TotalTokens,
			out.Totals.InputTokens, out.Totals.OutputTokens, out.Totals.CacheReadTokens, out.Totals.AvgTTFTMs)
		for _, b := range out.Buckets {
			fmt.Printf("  %-18s %-24s reqs=%d err=%d tokens=%d hit=%.0f%% ttft=%.0fms\n",
				b.Plan, b.Model, b.Requests, b.Errors, b.TotalTokens, b.CacheHitRatio*100, b.AvgTTFTMs)
		}
		return nil
	case "cooldown-clear":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro gw cooldown-clear <plan>")
		}
		path := "/v1/gateway/plans/" + url.PathEscape(args[1]) + "/reset-cooldown"
		return printAPI(cfg, http.MethodPost, path, nil)
	case "preview":
		fs := flag.NewFlagSet("gw preview", flag.ContinueOnError)
		profile := fs.String("profile", "", "Profile name")
		proto := fs.String("protocol", "anthropic", "Ingress protocol (openai-chat|openai-responses|anthropic|gemini|typesafe-jev|openai-decisions)")
		model := fs.String("model", "", "Requested model name")
		tokens := fs.Int("tokens", 0, "Estimated input tokens")
		turnPhase := fs.String("turn-phase", "", "Turn phase override")
		sessionKey := fs.String("session-key", "", "Session affinity key")
		tools := fs.Bool("tools", false, "Request includes tool declarations")
		vision := fs.Bool("vision", false, "Request includes image parts")
		thinking := fs.Bool("thinking", false, "Request enables extended thinking")
		pretty := fs.Bool("pretty", false, "Human-readable output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *model == "" {
			return fmt.Errorf("--model is required for gw preview")
		}
		body, _ := json.Marshal(shared.GatewayPreviewRequest{
			Profile:        *profile,
			Protocol:       *proto,
			Model:          *model,
			EstInputTokens: *tokens,
			TurnPhase:      *turnPhase,
			SessionKey:     *sessionKey,
			Tools:          *tools,
			Vision:         *vision,
			Thinking:       *thinking,
		})
		if !*pretty {
			return printAPI(cfg, http.MethodPost, "/v1/gateway/preview", bytes.NewReader(body))
		}
		raw, err := gwFetch(cfg, http.MethodPost, "/v1/gateway/preview", bytes.NewReader(body))
		if err != nil {
			return err
		}
		var out shared.GatewayPreviewResponse
		if json.Unmarshal(raw, &out) != nil {
			fmt.Println(string(raw))
			return nil
		}
		fmt.Printf("matched=%t route=%s family=%s chosen=%s err=%s\n", out.Matched, out.RouteID, out.Family, out.Chosen, out.Error)
		if out.Decision != nil {
			fmt.Printf("  resolution=%s\n", out.Decision.Resolution)
			for _, c := range out.Decision.Candidates {
				fmt.Printf("  %-18s model=%-22s proto=%-16s eligible=%t total=%.3f (Q=%.2f C=%.2f U=%.2f P=%.2f H=%.2f) %s\n",
					c.Plan, c.Model, c.Protocol, c.Eligible, c.Scores.Total,
					c.Scores.Quality, c.Scores.Cache, c.Scores.Quota, c.Scores.Protocol, c.Scores.Health, c.RejectReason)
			}
		}
		return nil
	case "reload":
		return printAPI(cfg, http.MethodPost, "/v1/gateway/reload", nil)
	default:
		return fmt.Errorf("unknown gw subcommand: %s", args[0])
	}
}

// reorderFlags moves flag tokens ahead of positional arguments so invocations
// like `gw quotas reset zcode --scope fiveHour` parse the same way as
// `gw quotas reset --scope fiveHour zcode`.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	boolFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if f.DefValue == "true" || f.DefValue == "false" {
			boolFlags[f.Name] = true
		}
	})
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			name := strings.TrimPrefix(strings.TrimPrefix(arg, "--"), "-")
			if !strings.Contains(arg, "=") && !boolFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}

// gwQuotas implements `crabbro gw quotas <refresh|reset>`.
func gwQuotas(cfg *clientConfig, args []string) error {
	if len(args) == 0 || args[0] == "trend" || args[0] == "--json" {
		if len(args) > 0 && args[0] == "trend" {
			args = args[1:]
		}
		fs := flag.NewFlagSet("gw quotas", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "JSON output")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected quota arguments")
		}
		if *asJSON {
			return printAPI(cfg, http.MethodGet, "/v1/gateway/plans", nil)
		}
		raw, err := gwFetch(cfg, http.MethodGet, "/v1/gateway/plans", nil)
		if err != nil {
			return err
		}
		var out shared.GatewayPlanListResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PLAN\tWINDOW\tUSED%\tELAPSED%\tPROJECTED%\tRESET IN\tSTATUS\tBALANCE / COUPONS")
		pct := func(p *float64) string {
			if p == nil {
				return "-"
			}
			return fmt.Sprintf("%.1f", *p)
		}
		for _, p := range out.Plans {
			if !p.Enabled {
				continue
			}
			q := p.RemoteQuota
			if q == nil {
				fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\tunknown\t-\n", p.ID)
				continue
			}
			details := q.Balance
			if q.ResetsRemaining > 0 {
				details += fmt.Sprintf(" coupons=%d", q.ResetsRemaining)
				if q.SoonestResetExpiry != nil {
					details += " expires=" + q.SoonestResetExpiry.Format(time.RFC3339)
				}
			}
			if q.Stale {
				details += " stale: " + q.LastError
			}
			if details == "" {
				details = "-"
			}
			if len(q.Windows) == 0 {
				fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\tunknown\t%s\n", p.ID, details)
			}
			for _, w := range q.Windows {
				elapsed, projected, status, reset := "-", "-", "unknown", "-"
				if w.Trend != nil {
					elapsed = fmt.Sprintf("%.1f", w.Trend.ElapsedPct)
					projected = pct(w.Trend.ProjectedEndPct)
					status = w.Trend.Status
				}
				if w.ResetAt != nil {
					d := time.Until(*w.ResetAt)
					if d < 0 {
						d = 0
					}
					reset = d.Round(time.Minute).String()
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, w.Name, pct(w.UsagePct), elapsed, projected, reset, status, details)
			}
		}
		return tw.Flush()
	}
	switch args[0] {
	case "refresh":
		return printAPI(cfg, http.MethodPost, "/v1/gateway/quotas/refresh", nil)
	case "reset":
		fs := flag.NewFlagSet("gw quotas reset", flag.ContinueOnError)
		scope := fs.String("scope", "", "Reset window scope (fiveHour|weekly; default soonest-expiring)")
		dryRun := fs.Bool("dry-run", false, "Select a coupon without redeeming it")
		pretty := fs.Bool("pretty", false, "Human-readable output")
		if err := fs.Parse(reorderFlags(fs, args[1:])); err != nil {
			return err
		}
		if *scope != "" && *scope != "fiveHour" && *scope != "weekly" {
			return fmt.Errorf("--scope must be fiveHour or weekly")
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: crabbro gw quotas reset <plan> [--scope fiveHour|weekly] [--dry-run]")
		}
		body, _ := json.Marshal(shared.GatewayQuotaResetRequest{
			Plan:   fs.Arg(0),
			Scope:  *scope,
			DryRun: *dryRun,
		})
		if !*pretty {
			return printAPI(cfg, http.MethodPost, "/v1/gateway/quotas/reset", bytes.NewReader(body))
		}
		raw, err := gwFetch(cfg, http.MethodPost, "/v1/gateway/quotas/reset", bytes.NewReader(body))
		if err != nil {
			return err
		}
		var out shared.GatewayQuotaResetResponse
		if json.Unmarshal(raw, &out) != nil {
			fmt.Println(string(raw))
			return nil
		}
		action := "redeemed"
		if out.DryRun {
			action = "selected"
		} else if !out.Redeemed {
			action = "not redeemed"
		}
		fmt.Printf("plan=%s scope=%s %s coupons=%d", out.Plan, out.Scope, action, out.CouponsAvailable)
		if out.CouponExpiresAt != nil {
			fmt.Printf(" expires=%s", out.CouponExpiresAt.Format(time.RFC3339))
		}
		fmt.Println()
		if out.Error != "" {
			fmt.Printf("  error: %s\n", out.Error)
		}
		if q := out.Status; q != nil && q.RemoteQuota != nil {
			fmt.Printf("  remote resets=%d source=%s stale=%t\n", q.RemoteQuota.ResetsRemaining, q.RemoteQuota.SyncSource, q.RemoteQuota.Stale)
			for _, w := range q.RemoteQuota.Windows {
				if w.UsagePct != nil {
					fmt.Printf("  remote window %s used=%.0f%%\n", w.Name, *w.UsagePct)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown gw quotas subcommand: %s", args[0])
	}
}

func gwPlansJSON(cfg *clientConfig, args []string, render func([]byte)) error {
	fs := flag.NewFlagSet("gw plans", flag.ContinueOnError)
	pretty := fs.Bool("pretty", false, "Human-readable output")
	refresh := fs.Bool("refresh", false, "Refresh provider quotas")
	if err := fs.Parse(args); err != nil {
		return err
	}
	method, path := http.MethodGet, "/v1/gateway/plans"
	if *refresh {
		method, path = http.MethodPost, "/v1/gateway/quotas/refresh"
	}
	if !*pretty {
		return printAPI(cfg, method, path, nil)
	}
	raw, err := gwFetch(cfg, method, path, nil)
	if err != nil {
		return err
	}
	render(raw)
	return nil
}

func gwGetJSON(cfg *clientConfig, args []string, path string, render func([]byte)) error {
	fs := flag.NewFlagSet("gw", flag.ContinueOnError)
	pretty := fs.Bool("pretty", false, "Human-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*pretty {
		return printAPI(cfg, http.MethodGet, path, nil)
	}
	raw, err := gwFetch(cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	render(raw)
	return nil
}

func gwFetch(cfg *clientConfig, method, path string, body io.Reader) ([]byte, error) {
	resp, err := doRequest(cfg, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
