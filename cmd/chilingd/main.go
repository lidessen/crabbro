// Command chilingd is the proxy daemon. It starts an HTTP forward proxy
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

	"github.com/lidessen/chiling/internal/api"
	"github.com/lidessen/chiling/internal/decision"
	"github.com/lidessen/chiling/internal/fixture"
	"github.com/lidessen/chiling/internal/mitm"
	"github.com/lidessen/chiling/internal/proxy"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/shared"
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
	fixtureDir := flag.String("fixture-dir", "", "Directory for fixture storage (default ~/.chiling/fixtures/)")

	// MITM flags (Milestone 3).
	mitmEnabled := flag.Bool("mitm", false, "Enable HTTPS MITM interception on startup")
	mitmAllow := flag.String("mitm-allow", "", "Comma-separated list of allowed domain patterns for MITM")
	mitmDeny := flag.String("mitm-deny", "", "Comma-separated list of denied domain patterns for MITM")
	decisionProvider := flag.String("decision-provider", "", "Decision provider: jev or openai (default from env)")
	decisionBase := flag.String("decision-base-url", "", "Decision API base URL (default from env)")
	decisionModel := flag.String("decision-model", "", "Decision model name (default from env)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Printf("chilingd starting (version %s)", "0.1.0")
	log.Printf("proxy address:  %s", *proxyAddr)
	log.Printf("control API:    UDS=%s, TCP=%s", *udsPath, *tcpAddr)
	log.Printf("ring buffer:    %d entries", *ringSize)

	// Create shared components.
	rb := ring.NewRequestBuffer(*ringSize)
	ruleStore := rule.NewRuleStore()
	ruleEngine := rule.NewRuleEngine()

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

	// Start proxy.
	pxy := proxy.New(*proxyAddr, rb, ruleStore, ruleEngine)

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
	apiSrv := api.New(*udsPath, *tcpAddr, rb, pxy.BodyCache, pxy.EventHub, fixtureStore, ruleStore, ruleEngine, pxy.Addr(), mitmHandler)
	apiSrv.Version = "0.1.0"
	apiSrv.RequestCountFn = pxy.RequestCount
	apiSrv.RuleCountFn = ruleCountFn
	apiSrv.Sessions = pxy.Sessions
	apiSrv.PublishMITM = func() { pxy.PublishFromMITM() }
	apiSrv.Replay = func(req shared.VerifyRequest) (shared.VerifyResult, error) {
		return pxy.Replay(string(req.RequestID), req.ExpectStatus, req.ExpectBodyContains)
	}
	decCfg := decision.FromEnv()
	if *decisionProvider != "" {
		decCfg.Provider = *decisionProvider
	}
	if *decisionBase != "" {
		decCfg.BaseURL = *decisionBase
	}
	if *decisionModel != "" {
		decCfg.Model = *decisionModel
	}
	apiSrv.Decision = decision.New(decCfg)
	tokenPath := filepath.Join(filepath.Dir(expandDaemonPath(*udsPath)), "api.token")
	token, err := loadOrCreateToken(tokenPath)
	if err != nil {
		log.Printf("control API token: %v", err)
	} else {
		apiSrv.Token = token
		log.Printf("control API token: %s", tokenPath)
	}

	if err := apiSrv.Start(); err != nil {
		pxy.Stop()
		return fmt.Errorf("starting control API: %w", err)
	}

	log.Printf("chilingd is ready")

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %v, shutting down", sig)

	// Graceful shutdown.
	if err := pxy.Stop(); err != nil {
		log.Printf("proxy shutdown error: %v", err)
	}
	if err := apiSrv.Stop(); err != nil {
		log.Printf("API server shutdown error: %v", err)
	}
	log.Printf("chilingd stopped")

	return nil
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
