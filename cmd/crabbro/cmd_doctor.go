package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/lidessen/crabbro/internal/shared"
)

// doctorResult holds all diagnostic information collected by the doctor.
type doctorResult struct {
	DaemonRunning     bool     `json:"daemonRunning"`
	ProxyListening    bool     `json:"proxyListening"`
	ProxyAddr         string   `json:"proxyAddr,omitempty"`
	MITMConfigured    bool     `json:"mitmConfigured"`
	MITMEnabled       bool     `json:"mitmEnabled"`
	MITMCertExists    bool     `json:"mitmCertExists"`
	MITMCertInstalled bool     `json:"mitmCertInstalled"`
	RulesCount        int      `json:"rulesCount,omitempty"`
	RequestsCaptured  int64    `json:"requestsCaptured,omitempty"`
	Issues            []string `json:"issues"`
}

// cmdDoctor implements the "doctor" command.
func cmdDoctor(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "Attempt to fix detected issues")
	pretty := fs.Bool("pretty", false, "Human-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	result := doctorResult{
		Issues: []string{},
	}

	// Check daemon connectivity.
	status, err := fetchStatus(cfg)
	if err != nil {
		result.Issues = append(result.Issues, fmt.Sprintf(
			"Cannot reach daemon: %v. Start it with 'crabbrod' in another terminal.", err))
	} else {
		result.DaemonRunning = true
		result.ProxyAddr = status.ProxyAddr
		result.ProxyListening = status.UptimeSeconds > 0
		result.MITMEnabled = status.MITMEnabled
		result.RulesCount = status.RuleCount
		result.RequestsCaptured = status.RequestCount

		// MITM status from daemon status response includes configured and installed.
		// If MITMEnabled or MITMCertInstalled were reported, MITM is configured.
		if status.MITMEnabled || status.MITMCertInstalled {
			result.MITMConfigured = true
		}
	}

	// Check CA cert file existence.
	certPath := defaultCertPath()
	if _, err := os.Stat(certPath); err == nil {
		result.MITMCertExists = true
	}

	// Check if CA is installed in system trust store (when daemon unreachable).
	if !result.DaemonRunning {
		result.MITMCertInstalled = checkCertInstalledDirect()
	}

	// Detect issues.
	if !result.DaemonRunning {
		// Already added the connection issue above.
	} else {
		if status.MITMEnabled && !result.MITMCertInstalled {
			result.Issues = append(result.Issues,
				"MITM is enabled but the CA certificate is not installed in the system trust store. Run 'crabbro mitm install-ca' to install it.")
		}
		if !status.MITMEnabled && result.MITMCertInstalled {
			result.Issues = append(result.Issues,
				"CA certificate is installed but MITM is disabled. Run 'crabbro mitm enable' to enable interception.")
		}
	}

	if !result.MITMCertExists && result.MITMConfigured {
		result.Issues = append(result.Issues,
			"MITM CA certificate file not found. Start the daemon with --mitm to generate it, or run 'crabbro mitm cert --output "+certPath+"' to download it.")
	}

	if len(result.Issues) == 0 {
		if result.DaemonRunning {
			result.Issues = append(result.Issues, "All checks passed.")
		} else {
			result.Issues = append(result.Issues, "No issues detected locally. Start the daemon for a full check.")
		}
	}

	if *pretty {
		printDoctorPretty(result)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	}

	// Handle --fix flag.
	if *fix && len(result.Issues) > 0 {
		return runDoctorFixes(cfg, result)
	}

	return nil
}

// fetchStatus queries the daemon status endpoint.
func fetchStatus(cfg *clientConfig) (*shared.StatusResponse, error) {
	resp, err := doRequest(cfg, "GET", "/v1/status", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("daemon returned status %d", resp.StatusCode)
	}

	var status shared.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding daemon response: %w", err)
	}
	return &status, nil
}

// checkCertInstalledDirect checks the system trust store directly for the crabbro root CA.
func checkCertInstalledDirect() bool {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("security", "find-certificate", "-c", "crabbro root CA", "-p")
		output, err := cmd.Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(output), "BEGIN CERTIFICATE")
	default:
		// Linux: check if the cert file exists in the system ca-certificates directory.
		if _, err := os.Stat("/usr/local/share/ca-certificates/crabbro-root.crt"); err == nil {
			return true
		}
		return false
	}
}

// printDoctorPretty prints the doctor results in human-readable format.
func printDoctorPretty(r doctorResult) {
	fmt.Println("=== crabbro Doctor ===")
	fmt.Println()
	fmt.Printf("Daemon Running:   %t\n", r.DaemonRunning)
	fmt.Printf("Proxy Listening:  %t\n", r.ProxyListening)
	if r.ProxyAddr != "" {
		fmt.Printf("Proxy Address:    %s\n", r.ProxyAddr)
	}
	fmt.Printf("MITM Configured:  %t\n", r.MITMConfigured)
	fmt.Printf("MITM Enabled:     %t\n", r.MITMEnabled)
	fmt.Printf("MITM Cert Exists: %t\n", r.MITMCertExists)
	fmt.Printf("MITM Cert Installed: %t\n", r.MITMCertInstalled)
	if r.RulesCount > 0 {
		fmt.Printf("Rules:            %d\n", r.RulesCount)
	}
	if r.RequestsCaptured > 0 {
		fmt.Printf("Requests:         %d\n", r.RequestsCaptured)
	}
	fmt.Println()
	fmt.Println("Issues:")
	for _, issue := range r.Issues {
		fmt.Printf("  - %s\n", issue)
	}
}

// runDoctorFixes attempts to fix detected issues automatically.
func runDoctorFixes(cfg *clientConfig, result doctorResult) error {
	for _, issue := range result.Issues {
		if issue == "All checks passed." || issue == "No issues detected locally. Start the daemon for a full check." {
			continue
		}

		switch {
		case strings.Contains(issue, "CA certificate is installed but MITM is disabled"):
			if confirm("MITM is disabled. Enable it?") {
				resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/enable", nil)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to enable MITM: %v\n", err)
				} else {
					resp.Body.Close()
					fmt.Println("MITM enabled.")
				}
			}

		case strings.Contains(issue, "MITM is enabled but the CA certificate is not installed"):
			if confirm("Install the root CA certificate into the system trust store?") {
				if err := cmdMITMInstallCA(cfg, nil); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to install CA: %v\n", err)
				}
			}

		case strings.Contains(issue, "MITM CA certificate file not found"):
			fmt.Println("Start the daemon with --mitm to generate the CA certificate.")
			if confirm("Start daemon now?") {
				// Launch crabbrod in background with mitm enabled.
				cmd := exec.Command("crabbrod", "--mitm")
				if err := cmd.Start(); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
					fmt.Printf("Run manually: crabbrod --mitm\n")
				} else {
					fmt.Println("Daemon started. Check status with 'crabbro doctor'.")
				}
			}

		default:
			fmt.Printf("No auto-fix available for: %s\n", issue)
		}
	}
	return nil
}
