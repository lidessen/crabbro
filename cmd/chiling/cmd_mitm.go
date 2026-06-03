package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lidessen/chiling/internal/shared"
)

// cmdMITM dispatches mitm subcommands.
func cmdMITM(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling mitm <command> [options]\n\nCommands: enable, disable, status, cert, install-ca, uninstall-ca, allow, deny")
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "enable":
		return cmdMITMEnable(cfg, subArgs)
	case "disable":
		return cmdMITMDisable(cfg, subArgs)
	case "status":
		return cmdMITMStatus(cfg, subArgs)
	case "cert":
		return cmdMITMCert(cfg, subArgs)
	case "install-ca":
		return cmdMITMInstallCA(cfg, subArgs)
	case "uninstall-ca":
		return cmdMITMUninstallCA(cfg, subArgs)
	case "allow":
		return cmdMITMAllowDeny(cfg, "allow", subArgs)
	case "deny":
		return cmdMITMAllowDeny(cfg, "deny", subArgs)
	default:
		return fmt.Errorf("unknown mitm command: %s", sub)
	}
}

// defaultCertPath returns the default path to the root CA PEM file.
func defaultCertPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.chiling/certs/rootCA.pem"
	}
	return filepath.Join(home, ".chiling", "certs", "rootCA.pem")
}

// caCertDir returns the directory where the root CA cert is stored.
func caCertDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.chiling/certs"
	}
	return filepath.Join(home, ".chiling", "certs")
}

// confirm prompts the user for confirmation (Y/n). Returns true if confirmed.
func confirm(prompt string) bool {
	fmt.Print(prompt + " [y/N] ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}

// osInstallCertCommand returns the OS-specific command to install a root CA.
func osInstallCertCommand(certPath string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "security", []string{"add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", certPath}
	case "linux":
		return "sudo", []string{"sh", "-c",
			fmt.Sprintf("cp %s /usr/local/share/ca-certificates/chiling-root.crt && update-ca-certificates", certPath)}
	default:
		return "", nil
	}
}

// osUninstallCertCommand returns the OS-specific command to remove a root CA.
func osUninstallCertCommand(certPath string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "security", []string{"remove-trusted-cert", "-d", certPath}
	case "linux":
		return "sudo", []string{"sh", "-c",
			fmt.Sprintf("rm -f %s && update-ca-certificates", certPath)}
	default:
		return "", nil
	}
}

// cmdMITMInstallCA installs the root CA into the system trust store.
func cmdMITMInstallCA(cfg *clientConfig, args []string) error {
	certPath := defaultCertPath()

	// Check if the cert file exists locally. If not, try to download from daemon.
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		fmt.Printf("CA certificate not found at %s\n", certPath)
		fmt.Println("Attempting to download from daemon...")

		// Ensure cert directory exists.
		if err := os.MkdirAll(caCertDir(), 0755); err != nil {
			return fmt.Errorf("creating cert directory: %w", err)
		}

		// Download cert from daemon.
		resp, err := doRequest(cfg, http.MethodGet, "/v1/mitm/cert", nil)
		if err != nil {
			return fmt.Errorf("cannot reach daemon to download certificate: %w\n\nStart the daemon with --mitm first, or place the certificate at %s", err, certPath)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("daemon returned %d: cannot download certificate\n\nStart the daemon with --mitm first, or place the certificate at %s", resp.StatusCode, certPath)
		}

		certPEM, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading certificate: %w", err)
		}
		if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
			return fmt.Errorf("writing certificate to %s: %w", certPath, err)
		}
		fmt.Printf("Certificate saved to %s\n", certPath)
	} else if err != nil {
		return fmt.Errorf("checking certificate at %s: %w", certPath, err)
	} else {
		fmt.Printf("Certificate found at %s\n", certPath)
	}

	cmdName, cmdArgs := osInstallCertCommand(certPath)
	if cmdName == "" {
		return fmt.Errorf("unsupported platform: %s\n\nInstall the certificate manually:\n  %s", runtime.GOOS, certPath)
	}

	fmt.Println()
	fmt.Println("The following command will modify your system trust store:")
	fmt.Printf("  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
	fmt.Println()
	fmt.Println("WARNING: Installing a custom root CA authorizes the chiling proxy to")
	fmt.Println("decrypt HTTPS traffic. Only proceed if you understand the security")
	fmt.Println("implications.")

	if !confirm("Proceed with installation?") {
		fmt.Println("Installation cancelled.")
		fmt.Printf("To install manually, run:\n  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
		return nil
	}

	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Installation command failed: %v\n", err)
		fmt.Printf("\nTo install manually, run:\n  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
		return nil
	}

	fmt.Println("Root CA certificate installed successfully.")

	// Notify daemon best-effort.
	resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`{"installed":true}`))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not notify daemon: %v\n", err)
		return nil
	}
	resp.Body.Close()

	return nil
}

// cmdMITMUninstallCA removes the root CA from the system trust store.
func cmdMITMUninstallCA(cfg *clientConfig, args []string) error {
	certPath := defaultCertPath()

	// Check if the cert file exists.
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		fmt.Printf("CA certificate not found at %s. Attempting uninstall anyway.\n", certPath)
	}

	cmdName, cmdArgs := osUninstallCertCommand(certPath)
	if cmdName == "" {
		return fmt.Errorf("unsupported platform: %s\n\nRemove the certificate manually:\n  %s", runtime.GOOS, certPath)
	}

	fmt.Println()
	fmt.Println("The following command will modify your system trust store:")
	fmt.Printf("  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
	fmt.Println()
	fmt.Println("WARNING: The chiling proxy will no longer be able to decrypt HTTPS")
	fmt.Println("traffic without browser certificate warnings.")

	if !confirm("Proceed with uninstallation?") {
		fmt.Println("Uninstallation cancelled.")
		fmt.Printf("To uninstall manually, run:\n  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
		return nil
	}

	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Uninstall command failed: %v\n", err)
		fmt.Printf("\nTo uninstall manually, run:\n  %s %s\n", cmdName, strings.Join(cmdArgs, " "))
		return nil
	}

	fmt.Println("Root CA certificate uninstalled successfully.")

	// Notify daemon best-effort.
	resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`{"installed":false}`))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not notify daemon: %v\n", err)
		return nil
	}
	resp.Body.Close()

	return nil
}

// ---------------------------------------------------------------------------
// Original MITM commands (Milestone 3)
// ---------------------------------------------------------------------------

func cmdMITMEnable(cfg *clientConfig, args []string) error {
	resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/enable", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}
	fmt.Println("MITM enabled")
	return nil
}

func cmdMITMDisable(cfg *clientConfig, args []string) error {
	resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/disable", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}
	fmt.Println("MITM disabled")
	return nil
}

func cmdMITMStatus(cfg *clientConfig, args []string) error {
	pretty := false
	for _, a := range args {
		if a == "--pretty" {
			pretty = true
		}
	}

	resp, err := doRequest(cfg, http.MethodGet, "/v1/mitm/status", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	if pretty {
		var status shared.MITMStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		fmt.Printf("Enabled:       %t\n", status.Enabled)
		fmt.Printf("Cert Installed: %t\n", status.CertInstalled)
		fmt.Printf("Allow Patterns: %s\n", formatPatterns(status.AllowPatterns))
		fmt.Printf("Deny Patterns:  %s\n", formatPatterns(status.DenyPatterns))
	} else {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func cmdMITMCert(cfg *clientConfig, args []string) error {
	outputPath := ""
	for i, a := range args {
		if a == "--output" && i+1 < len(args) {
			outputPath = args[i+1]
		}
	}

	resp, err := doRequest(cfg, http.MethodGet, "/v1/mitm/cert", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	certPEM, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read cert response: %w", err)
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, certPEM, 0644); err != nil {
			return fmt.Errorf("write cert to %s: %w", outputPath, err)
		}
		fmt.Printf("Root CA certificate saved to %s\n", outputPath)
	} else {
		os.Stdout.Write(certPEM)
	}
	return nil
}

func cmdMITMAllowDeny(cfg *clientConfig, listType string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling mitm %s <command> [options]\n\nCommands: list, add <pattern>, remove <pattern>", listType)
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "list":
		return cmdMITMList(cfg, listType)
	case "add":
		if len(subArgs) < 1 {
			return fmt.Errorf("usage: chiling mitm %s add <pattern>", listType)
		}
		return cmdMITMAdd(cfg, listType, subArgs[0])
	case "remove":
		if len(subArgs) < 1 {
			return fmt.Errorf("usage: chiling mitm %s remove <pattern>", listType)
		}
		return cmdMITMRemove(cfg, listType, subArgs[0])
	default:
		return fmt.Errorf("unknown %s command: %s", listType, sub)
	}
}

func cmdMITMList(cfg *clientConfig, listType string) error {
	resp, err := doRequest(cfg, http.MethodGet, "/v1/mitm/"+listType, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}

	var result map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	patterns := result["patterns"]
	if len(patterns) == 0 {
		fmt.Printf("No %s patterns configured.\n", listType)
		return nil
	}
	fmt.Printf("%s patterns:\n", listType)
	for _, p := range patterns {
		fmt.Printf("  - %s\n", p)
	}
	return nil
}

func cmdMITMAdd(cfg *clientConfig, listType string, pattern string) error {
	body := shared.MITMDomainRequest{Pattern: pattern}
	jsonBody, _ := json.Marshal(body)

	resp, err := doRequest(cfg, http.MethodPost, "/v1/mitm/"+listType, strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}
	fmt.Printf("Pattern %q added to %s list\n", pattern, listType)
	return nil
}

func cmdMITMRemove(cfg *clientConfig, listType, pattern string) error {
	path := fmt.Sprintf("/v1/mitm/%s?pattern=%s", listType, pattern)
	resp, err := doRequest(cfg, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error: %s", readBody(resp.Body))
	}
	fmt.Printf("Pattern %q removed from %s list\n", pattern, listType)
	return nil
}

func formatPatterns(patterns []string) string {
	if len(patterns) == 0 {
		return "(none)"
	}
	return strings.Join(patterns, ", ")
}
