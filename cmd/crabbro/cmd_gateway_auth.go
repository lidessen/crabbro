package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/siwc"
)

var gatewayAuthRequest = doRequest

const gatewayAuthUsage = "usage: crabbro gateway auth <providers|login|status|logout>"

func gatewayAuth(cfg *clientConfig, args []string) error {
	if len(args) == 0 {
		return errors.New(gatewayAuthUsage)
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(gatewayAuthUsage + "\n  providers [--pretty]\n  login <provider-or-plan> [--method <kind>] [--plan <plan-id>] [--stdin] [--no-browser]\n  status [--pretty]\n  logout <plan-id>")
		return nil
	case "providers":
		return gatewayAuthProviders(args[1:], os.Stdout)
	case "status":
		return gatewaySIWCStatus(cfg, args[1:])
	case "login":
		fs := flag.NewFlagSet("gateway auth login", flag.ContinueOnError)
		noBrowser := fs.Bool("no-browser", false, "Print the URL without opening a browser")
		method := fs.String("method", "", "Authentication method")
		planID := fs.String("plan", "", "Plan ID for a provider")
		stdin := fs.Bool("stdin", false, "Read API key from standard input")
		fs.SetOutput(io.Discard)
		// The public syntax permits flags on either side of the plan ID.
		loginArgs := args[1:]
		if len(loginArgs) > 0 && len(loginArgs[0]) > 0 && loginArgs[0][0] != '-' {
			loginArgs = append(append([]string{}, loginArgs[1:]...), loginArgs[0])
		}
		if err := fs.Parse(loginArgs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Println("usage: crabbro gateway auth login <provider-or-plan> [--method <kind>] [--plan <plan-id>] [--stdin] [--no-browser]")
				return nil
			}
			return errors.New("invalid gateway auth login options")
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			return errors.New("usage: crabbro gateway auth login <provider-or-plan> [--method <kind>] [--plan <plan-id>] [--stdin] [--no-browser]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return gatewayAuthProvision(ctx, cfg, fs.Arg(0), *planID, provider.AuthKind(*method), *stdin, os.Stdin, os.Stdout, func(refs gatewaySIWCStatusEntry) error {
			return gatewayAuthLogin(ctx, siwc.Client{}, gatewayAuthStore(), refs, os.Stdout, !*noBrowser, openGatewayAuthBrowser, net.Listen)
		})
	case "logout":
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			fmt.Println("usage: crabbro gateway auth logout <plan-id>")
			return nil
		}
		if len(args) != 2 || args[1] == "" || args[1][0] == '-' {
			return errors.New("usage: crabbro gateway auth logout <plan-id>")
		}
		refs, err := gatewayAuthPlan(cfg, args[1])
		if err != nil {
			return err
		}
		if err := gatewayAuthLogout(gatewayAuthStore(), refs); err != nil {
			return err
		}
		fmt.Println("Gateway authentication session removed; host ID preserved.")
		return nil
	default:
		return errors.New(gatewayAuthUsage)
	}
}

func gatewayAuthProviders(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gateway auth providers", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pretty := fs.Bool("pretty", false, "Human-readable output")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(out, "usage: crabbro gateway auth providers [--pretty]")
		return err
	} else if err != nil || fs.NArg() != 0 {
		return errors.New("usage: crabbro gateway auth providers [--pretty]")
	}
	type entry struct {
		ID               string              `json:"id"`
		DisplayName      string              `json:"displayName"`
		AuthMethods      []provider.AuthKind `json:"authMethods"`
		DefaultBaseURL   string              `json:"defaultBaseUrl"`
		DefaultEndpoints []string            `json:"defaultEndpoints"`
	}
	list := make([]entry, 0)
	for _, p := range provider.List() {
		e := entry{ID: p.ID, DisplayName: p.DisplayName, DefaultBaseURL: p.DefaultBaseURL, DefaultEndpoints: append([]string{}, p.DefaultEndpoints...)}
		for _, m := range p.SupportedAuth {
			e.AuthMethods = append(e.AuthMethods, m.Kind)
		}
		list = append(list, e)
	}
	enc := json.NewEncoder(out)
	if *pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(list)
}

var errGatewayPlanNotFound = errors.New("gateway plan not found")

func gatewayAuthLogout(store siwc.Store, refs gatewaySIWCStatusEntry) error {
	if refs.SessionRef == refs.HostIDRef || secret.ValidRef(refs.SessionRef) != nil || secret.ValidRef(refs.HostIDRef) != nil {
		return errors.New("invalid gateway authentication references")
	}
	if err := store.Delete(refs.SessionRef); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("delete gateway authentication session failed")
	}
	return nil
}

func gatewayAuthStore() siwc.Store {
	return siwc.Store{Secrets: secret.New(filepath.Join(expandPath("~/.crabbro"), "secrets"))}
}

func gatewayAuthPlan(cfg *clientConfig, id string) (gatewaySIWCStatusEntry, error) {
	resp, err := gatewayAuthRequest(cfg, http.MethodGet, "/v1/gateway/plans", nil)
	if err != nil {
		return gatewaySIWCStatusEntry{}, errors.New("fetch gateway plans failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gatewaySIWCStatusEntry{}, fmt.Errorf("fetch gateway plans: HTTP %d", resp.StatusCode)
	}
	var data struct {
		Plans *[]struct {
			ID   string          `json:"id"`
			SIWC json.RawMessage `json:"siwc"`
		} `json:"plans"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil || data.Plans == nil {
		return gatewaySIWCStatusEntry{}, errors.New("invalid gateway plans response")
	}
	var found *gatewaySIWCStatusEntry
	for _, p := range *data.Plans {
		if p.ID != id {
			continue
		}
		if found != nil {
			return gatewaySIWCStatusEntry{}, errors.New("duplicate gateway plan")
		}
		if len(p.SIWC) == 0 {
			return gatewaySIWCStatusEntry{}, errors.New("gateway plans response does not include SIWC metadata; daemon update required")
		}
		var refs struct {
			SessionRef string `json:"sessionRef"`
			HostIDRef  string `json:"hostIdRef"`
		}
		if string(p.SIWC) == "null" {
			return gatewaySIWCStatusEntry{}, errors.New("gateway plan does not use account authentication")
		}
		if json.Unmarshal(p.SIWC, &refs) != nil || secret.ValidRef(refs.SessionRef) != nil || secret.ValidRef(refs.HostIDRef) != nil || refs.SessionRef == refs.HostIDRef {
			return gatewaySIWCStatusEntry{}, errors.New("invalid gateway authentication references")
		}
		found = &gatewaySIWCStatusEntry{PlanID: p.ID, SessionRef: refs.SessionRef, HostIDRef: refs.HostIDRef}
	}
	if found == nil {
		return gatewaySIWCStatusEntry{}, errGatewayPlanNotFound
	}
	return *found, nil
}

func openGatewayAuthBrowser(ctx context.Context, authURL string) error {
	var command string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return errors.New("browser opening is unavailable")
	}
	cmd := exec.CommandContext(ctx, command, authURL)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func gatewayAuthLogin(ctx context.Context, client siwc.Client, store siwc.Store, refs gatewaySIWCStatusEntry, out io.Writer, launch bool, openBrowser func(context.Context, string) error, listen func(string, string) (net.Listener, error)) error {
	hostID, err := store.HostID(refs.HostIDRef)
	if err != nil {
		return errors.New("prepare gateway host ID failed")
	}
	d, err := client.Discover(ctx)
	if err != nil {
		return errors.New("gateway authentication discovery failed")
	}
	listener, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("start authentication callback listener failed")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	// An empty client ID triggers SIWC dynamic registration during authorization.
	authURL, attempt, err := d.AuthorizationURL(redirect, hostID, "Crabbro", "")
	if err != nil {
		return errors.New("prepare gateway authorization failed")
	}
	type result struct {
		callback siwc.Callback
		err      error
	}
	callbacks := make(chan result, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb, err := attempt.ValidateCallback(r)
		if err != nil {
			http.Error(w, "Invalid authorization callback", http.StatusBadRequest)
			return
		}
		select {
		case callbacks <- result{callback: cb}:
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, "Authorization received. You may close this window.")
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Close()
	if _, err := fmt.Fprintf(out, "Open this URL to authorize Crabbro:\n%s\n", authURL); err != nil {
		return errors.New("print authorization URL failed")
	}
	if launch && openBrowser(ctx, authURL) != nil {
		fmt.Fprintln(out, "Could not open a browser; open the URL above manually.")
	}
	select {
	case <-ctx.Done():
		return errors.New("gateway authentication canceled or timed out")
	case <-serveErr:
		return errors.New("authentication callback listener stopped")
	case result := <-callbacks:
		tokens, err := client.Exchange(ctx, d, attempt, result.callback)
		if err != nil || tokens.IDToken == "" || !tokens.GrantedDirect() {
			return errors.New("gateway authentication exchange or verification failed")
		}
		if err := store.Save(refs.SessionRef, tokens); err != nil {
			return errors.New("save gateway authentication session failed")
		}
		if _, err = fmt.Fprintln(out, "Gateway authentication session saved."); err != nil {
			return errors.New("print gateway authentication status failed")
		}
		return nil
	}
}
