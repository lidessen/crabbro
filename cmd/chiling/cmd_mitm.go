package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lidessen/chiling/internal/shared"
)

// cmdMITM dispatches mitm subcommands.
func cmdMITM(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling mitm <command> [options]\n\nCommands: enable, disable, status, cert, allow, deny")
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
	case "allow":
		return cmdMITMAllowDeny(cfg, "allow", subArgs)
	case "deny":
		return cmdMITMAllowDeny(cfg, "deny", subArgs)
	default:
		return fmt.Errorf("unknown mitm command: %s", sub)
	}
}

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
