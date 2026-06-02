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
	"syscall"

	"github.com/lidessen/chiling/internal/api"
	"github.com/lidessen/chiling/internal/proxy"
	"github.com/lidessen/chiling/internal/ring"
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
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Printf("chilingd starting (version %s)", "0.1.0")
	log.Printf("proxy address:  %s", *proxyAddr)
	log.Printf("control API:    UDS=%s, TCP=%s", *udsPath, *tcpAddr)
	log.Printf("ring buffer:    %d entries", *ringSize)

	// Create shared components.
	rb := ring.NewRequestBuffer(*ringSize)

	// Start proxy.
	pxy := proxy.New(*proxyAddr, rb)
	if err := pxy.Start(); err != nil {
		return fmt.Errorf("starting proxy: %w", err)
	}
	log.Printf("proxy listening on %s", pxy.Addr())

	// Start control API.
	apiSrv := api.New(*udsPath, *tcpAddr, rb, pxy.BodyCache, pxy.EventHub, pxy.Addr())
	apiSrv.Version = "0.1.0"
	apiSrv.RequestCountFn = pxy.RequestCount
	apiSrv.RuleCountFn = func() int { return 0 }

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
