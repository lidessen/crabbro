// Command crabbrod is the proxy daemon. It starts an HTTP forward proxy
// with request/response capture and exposes a local control API for querying
// status, listing captured requests, and subscribing to live events.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"crypto/rand"
	"encoding/hex"
	"path/filepath"

	"github.com/lidessen/crabbro/internal/api"
	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/decision"
	"github.com/lidessen/crabbro/internal/fixture"
	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/inspect"
	"github.com/lidessen/crabbro/internal/mitm"
	"github.com/lidessen/crabbro/internal/plugin"
	"github.com/lidessen/crabbro/internal/proxy"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
	"github.com/lidessen/crabbro/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// CLI flags.
	proxyAddr := flag.String("proxy-addr", shared.DefaultProxyAddr, "Address for the HTTP proxy to listen on")
	udsPath := flag.String("uds-path", shared.DefaultUDSPath, "Unix domain socket path for the control API")
	tcpAddr := flag.String("tcp-addr", shared.DefaultTCPAddr, "TCP address for the control API")
	ringSize := flag.Int("ring-buffer-size", shared.DefaultRingBufferCapacity, "Number of requests to keep in the ring buffer")
	fixtureDir := flag.String("fixture-dir", "", "Directory for fixture storage (default ~/.crabbro/fixtures/)")

	// MITM flags (Milestone 3).
	mitmEnabled := flag.Bool("mitm", false, "Enable HTTPS MITM interception on startup")
	mitmAllow := flag.String("mitm-allow", "", "Comma-separated list of allowed domain patterns for MITM")
	mitmDeny := flag.String("mitm-deny", "", "Comma-separated list of denied domain patterns for MITM")
	historyPath := flag.String("history", "", "SQLite history path (default: history.db next to the socket; 'off' disables)")
	historyRetention := flag.Duration("history-retention", shared.DefaultHistoryRetention, "How long to keep captured exchanges")
	historyMax := flag.Int("history-max-rows", shared.DefaultHistoryMaxRows, "Maximum stored exchanges")
	gatewayAddr := flag.String("gateway-addr", shared.DefaultGatewayAddr, "Listen address for the multi-protocol LLM/Decision gateway ('off' disables)")
	gatewayToken := flag.String("gateway-token", "", "Bearer/API token for gateway requests (required when binding non-loopback)")
	gatewayCapture := flag.String("gateway-capture", "preview", "Gateway exchange capture mode: preview, metadata-only, or off")
	gatewayMaxBody := flag.Int64("gateway-max-request-body", shared.DefaultMaxGatewayRequestBody, "Maximum gateway request body size in bytes")
	gatewayUsageRetention := flag.Duration("gateway-usage-retention", shared.DefaultGatewayUsageRetention, "How long to retain gateway usage records")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Printf("crabbrod starting (version %s)", version.String())
	log.Printf("proxy address:  %s", *proxyAddr)
	log.Printf("control API:    UDS=%s, TCP=%s", *udsPath, *tcpAddr)
	log.Printf("ring buffer:    %d entries", *ringSize)

	// Create shared components.
	rb := ring.NewRequestBuffer(*ringSize)
	ruleStore := rule.NewRuleStore()
	ruleEngine := rule.NewRuleEngine()
	homeDir := filepath.Dir(expandDaemonPath(*udsPath))
	secretStore := secret.New(filepath.Join(homeDir, "secrets"))
	rulesDir := rule.OpenDir(filepath.Join(homeDir, "rules"))
	if diags, complete := rulesDir.Sync(ruleStore); !complete {
		log.Printf("rules: %d file diagnostics", len(diags))
	}
	pluginMgr, err := plugin.NewManager(filepath.Join(homeDir, "plugins"), secretStore)
	if err != nil {
		log.Printf("plugins: %v", err)
	} else {
		ruleEngine.Runtime = pluginMgr
		defer pluginMgr.Close()
	}

	// -----------------------------------------------------------------------
	// MITM initialization (Milestone 3)
	// -----------------------------------------------------------------------
	var mitmHandler *mitm.MITMHandler
	if *mitmEnabled || *mitmAllow != "" || *mitmDeny != "" {
		certDir := mitm.DefaultCertDir
		certPath := certDir + "/" + mitm.DefaultCertFile
		keyPath := certDir + "/" + mitm.DefaultKeyFile

		// Try loading existing CA, or generate a new one.
		var ca *mitm.CAStore
		ca, loadErr := mitm.LoadPEM(certPath, keyPath)
		if loadErr != nil {
			log.Printf("MITM: no existing CA found (%v), generating a new one", loadErr)
			ca, loadErr = mitm.GenerateCA()
			if loadErr != nil {
				return fmt.Errorf("generating root CA: %w", loadErr)
			}
			if err := mitm.SavePEM(ca, certPath, keyPath); err != nil {
				return fmt.Errorf("saving root CA: %w", err)
			}
			log.Printf("MITM: root CA generated and saved to %s", certDir)
		} else {
			log.Printf("MITM: root CA loaded from %s", certDir)
		}

		mitmHandler = mitm.NewMITMHandler(ca, 1024)
		mitmHandler.SetEnabled(*mitmEnabled)

		// Apply initial domain patterns.
		if *mitmAllow != "" {
			for _, p := range strings.Split(*mitmAllow, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					mitmHandler.Matcher.AddAllow(p)
				}
			}
		}
		if *mitmDeny != "" {
			for _, p := range strings.Split(*mitmDeny, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					mitmHandler.Matcher.AddDeny(p)
				}
			}
		}

		log.Printf("MITM: enabled=%t, allow=%q, deny=%q",
			*mitmEnabled, mitmHandler.Matcher.AllowPatterns(), mitmHandler.Matcher.DenyPatterns())
	}

	// Shared exchange observation sink.
	bodyCache := capture.NewBodyCache(shared.MaxBodyCacheBytes, shared.MaxBodySize)
	eventHub := capture.NewEventHub()
	sessions := session.New(1024)
	sessions.SetObserver(func(a shared.Activity) {
		eventHub.Emit(&shared.Event{
			Type:      shared.EventActivity,
			RequestID: shared.RequestID(a.RequestID),
			Timestamp: a.At,
			Payload:   a,
		})
	})
	sink := capture.NewDefaultSink(rb, bodyCache, eventHub, sessions, nil)

	// Start proxy.
	pxy := proxy.NewWithSink(*proxyAddr, sink, ruleStore, ruleEngine)

	// Wire MITM to proxy (if configured) and publish one policy snapshot.
	// An empty allowlist denies interception.
	if mitmHandler != nil {
		pxy.MITMHandler = mitmHandler
	}
	if mitmHandler != nil {
		pxy.PublishFromMITM()
	} else {
		pxy.PublishPolicy(proxy.NetworkPolicy{Enabled: false, EmptyAllowDenies: true})
	}
	if mitmHandler != nil && *mitmEnabled && len(mitmHandler.Matcher.AllowPatterns()) == 0 {
		log.Printf("MITM: enabled with an empty allowlist; no host will be intercepted until a pattern is added")
	}

	if err := pxy.Start(); err != nil {
		return fmt.Errorf("starting proxy: %w", err)
	}
	log.Printf("proxy listening on %s", pxy.Addr())

	// Create stores.
	fixtureStore, err := fixture.New(*fixtureDir)
	if err != nil {
		pxy.Stop()
		return fmt.Errorf("creating fixture store: %w", err)
	}
	log.Printf("fixture store:  %s", fixtureStore.BaseDir())

	// Use the rule store for the RuleCountFn.
	ruleCountFn := ruleStore.Count

	// Start control API.
	apiSrv := api.New(*udsPath, *tcpAddr, rb, bodyCache, eventHub, fixtureStore, ruleStore, ruleEngine, pxy.Addr(), mitmHandler)
	apiSrv.Version = "0.1.0"
	apiSrv.RequestCountFn = pxy.RequestCount
	apiSrv.RuleCountFn = ruleCountFn
	apiSrv.Sessions = sessions
	apiSrv.Secrets = secretStore
	apiSrv.PublishMITM = func() { pxy.PublishFromMITM() }
	apiSrv.Replay = func(req shared.VerifyRequest) (shared.VerifyResult, error) {
		return pxy.Replay(string(req.RequestID), req.ExpectStatus, req.ExpectBodyContains)
	}
	apiSrv.RulesDir = rulesDir
	inspectRT := inspect.NewRuntime(filepath.Join(homeDir, "scope"))
	inspectRT.Scopes = sessions
	pxy.Inspect = inspectRT
	apiSrv.Inspect = inspectRT
	if pluginMgr != nil {
		apiSrv.Plugins = pluginMgr
	}
	if histPath := resolveHistoryPath(*historyPath, *udsPath); histPath != "" {
		hist, err := history.Open(histPath, *historyRetention, *historyMax)
		if err != nil {
			log.Printf("history: %v", err)
		} else {
			sink.History = hist
			apiSrv.History = hist
			log.Printf("history: %s (retention %s, max %d)", histPath, historyRetention.String(), *historyMax)
			defer hist.Close()
		}
	}
	tokenPath := filepath.Join(filepath.Dir(expandDaemonPath(*udsPath)), "api.token")
	token, err := loadOrCreateToken(tokenPath)
	if err != nil {
		log.Printf("control API token: %v", err)
	} else {
		apiSrv.Token = token
		log.Printf("control API token: %s", tokenPath)
	}

	var gw *gateway.Gateway
	if gAddr := strings.TrimSpace(*gatewayAddr); gAddr != "" && !strings.EqualFold(gAddr, "off") && !strings.EqualFold(gAddr, "disabled") {
		switch *gatewayCapture {
		case "preview", "metadata-only", "off":
		default:
			pxy.Stop()
			return fmt.Errorf("invalid --gateway-capture %q (expected preview|metadata-only|off)", *gatewayCapture)
		}
		var gErr error
		gw, gErr = gateway.New(gateway.Options{
			ListenAddr:     gAddr,
			Token:          *gatewayToken,
			CaptureMode:    *gatewayCapture,
			MaxRequestBody: *gatewayMaxBody,
			UsageRetention: *gatewayUsageRetention,
			RootDir:        filepath.Join(homeDir, "gateway"),
		}, secretStore, sink)
		if gErr != nil {
			pxy.Stop()
			return fmt.Errorf("initializing gateway: %w", gErr)
		}
		if err := gw.Start(); err != nil {
			_ = gw.Stop()
			pxy.Stop()
			return fmt.Errorf("starting gateway: %w", err)
		}
		apiSrv.Gateway = gw
		decClient := decision.New(gw, "decision")
		apiSrv.Decision = decClient
		if pluginMgr != nil {
			pluginMgr.Decision = decClient
			st := decClient.Status()
			sinkPath := filepath.Join(homeDir, "decision-sink.json")
			if err := decision.WriteSink(sinkPath, decision.Sink{
				Instance: "local", Principal: "crabbrod", Endpoint: "gateway://route/decision",
				Model: st.Model, Provider: st.Provider,
			}); err != nil {
				log.Printf("decision sink: %v", err)
			}
		}
		log.Printf("gateway addr:   %s (capture=%s)", gw.Addr(), *gatewayCapture)
	} else {
		log.Printf("gateway:        disabled")
	}

	if err := apiSrv.Start(); err != nil {
		if gw != nil {
			_ = gw.Stop()
		}
		pxy.Stop()
		return fmt.Errorf("starting control API: %w", err)
	}

	log.Printf("crabbrod is ready")

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %v, shutting down", sig)

	// Graceful shutdown.
	if gw != nil {
		if err := gw.Stop(); err != nil {
			log.Printf("gateway shutdown error: %v", err)
		}
	}
	if err := pxy.Stop(); err != nil {
		log.Printf("proxy shutdown error: %v", err)
	}
	if err := apiSrv.Stop(); err != nil {
		log.Printf("API server shutdown error: %v", err)
	}
	log.Printf("crabbrod stopped")

	return nil
}

func resolveHistoryPath(flagPath, uds string) string {
	switch strings.ToLower(strings.TrimSpace(flagPath)) {
	case "off", "none", "disabled":
		return ""
	}
	if flagPath != "" {
		return flagPath
	}
	return filepath.Join(filepath.Dir(expandDaemonPath(uds)), "history.db")
}

func expandDaemonPath(path string) string {
	if len(path) > 1 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(b))
		if tok != "" {
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0600); err != nil {
		return "", err
	}
	return tok, nil
}
