package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func cmdPlugin(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro plugin <install|list|inspect|tools|invoke|uninstall|instance|host>")
	}
	switch args[0] {
	case "list":
		return printAPI(cfg, http.MethodGet, "/v1/plugins", nil)
	case "install":
		fs := flag.NewFlagSet("plugin install", flag.ContinueOnError)
		enable := fs.Bool("enable", true, "Enable the package")
		fs.Parse(args[1:])
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: crabbro plugin install <path> [--enable]")
		}
		body, _ := json.Marshal(map[string]any{"path": fs.Arg(0), "enable": *enable})
		return printAPI(cfg, http.MethodPost, "/v1/plugins", bytes.NewReader(body))
	case "inspect":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro plugin inspect <id>")
		}
		return printAPI(cfg, http.MethodGet, "/v1/plugins/"+args[1], nil)
	case "tools":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro plugin tools <id>")
		}
		return printAPI(cfg, http.MethodGet, "/v1/plugins/"+args[1]+"/tools", nil)
	case "uninstall":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro plugin uninstall <id>")
		}
		return printAPI(cfg, http.MethodDelete, "/v1/plugins/"+args[1], nil)
	case "invoke":
		return cmdPluginInvoke(cfg, args[1:])
	case "instance":
		return cmdPluginInstance(cfg, args[1:])
	case "host":
		return printAPI(cfg, http.MethodGet, "/v1/plugins/host", nil)
	default:
		return fmt.Errorf("unknown plugin command: %s", args[0])
	}
}

func cmdPluginInvoke(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("plugin invoke", flag.ContinueOnError)
	raw := fs.String("arguments", "{}", "JSON arguments")
	fs.Parse(args)
	if fs.NArg() < 3 {
		return fmt.Errorf("usage: crabbro plugin invoke <plugin> <instance> <tool> --arguments <json>")
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(*raw), &arguments); err != nil {
		return fmt.Errorf("arguments must be JSON")
	}
	body, _ := json.Marshal(map[string]any{"arguments": arguments})
	path := fmt.Sprintf("/v1/plugins/%s/instances/%s/tools/%s", fs.Arg(0), fs.Arg(1), fs.Arg(2))
	return printAPI(cfg, http.MethodPost, path, bytes.NewReader(body))
}

func cmdPluginInstance(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro plugin instance <add|list|remove>")
	}
	switch args[0] {
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro plugin instance list <plugin>")
		}
		return printAPI(cfg, http.MethodGet, "/v1/plugins/"+args[1]+"/instances", nil)
	case "add":
		fs := flag.NewFlagSet("plugin instance add", flag.ContinueOnError)
		configPath := fs.String("config", "", "JSON config file")
		fs.Parse(args[1:])
		if fs.NArg() < 2 {
			return fmt.Errorf("usage: crabbro plugin instance add <plugin> <instance> [--config file]")
		}
		cfgObj := map[string]any{}
		if *configPath != "" {
			b, err := os.ReadFile(*configPath)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &cfgObj); err != nil {
				return fmt.Errorf("config must be JSON")
			}
		}
		body, _ := json.Marshal(map[string]any{"name": fs.Arg(1), "config": cfgObj})
		return printAPI(cfg, http.MethodPost, "/v1/plugins/"+fs.Arg(0)+"/instances", bytes.NewReader(body))
	case "enable", "disable":
		if len(args) < 3 {
			return fmt.Errorf("usage: crabbro plugin instance %s <plugin> <instance>", args[0])
		}
		return printAPI(cfg, http.MethodPost, "/v1/plugins/"+args[1]+"/instances/"+args[2]+"/"+args[0], nil)
	case "remove":
		if len(args) < 3 {
			return fmt.Errorf("usage: crabbro plugin instance remove <plugin> <instance>")
		}
		return printAPI(cfg, http.MethodDelete, "/v1/plugins/"+args[1]+"/instances/"+args[2], nil)
	default:
		return fmt.Errorf("unknown instance command: %s", args[0])
	}
}

func cmdConfig(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro config <status|draft|validate|commit|discard>")
	}
	switch args[0] {
	case "status":
		kind := strings.TrimPrefix(configKind(args[1:]), "&")
		path := "/v1/config/status"
		if kind != "" {
			path += "?" + kind
		}
		return printAPI(cfg, http.MethodGet, path, nil)
	case "validate":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro config validate <id> [--gateway-plan|--gateway-route|--scope]")
		}
		return printAPI(cfg, http.MethodPost, "/v1/config/validate?id="+args[1]+configKind(args[2:]), nil)
	case "commit":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro config commit <id> [--gateway-plan|--gateway-route|--scope]")
		}
		return printAPI(cfg, http.MethodPost, "/v1/config/commit?id="+args[1]+configKind(args[2:]), nil)
	case "discard":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro config discard <id> [--gateway-plan|--gateway-route|--scope]")
		}
		return printAPI(cfg, http.MethodPost, "/v1/config/discard?id="+args[1]+configKind(args[2:]), nil)
	case "draft":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro config draft <file> [--gateway-plan|--gateway-route|--scope]")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		id := strings.TrimSuffix(strings.TrimSuffix(fileBase(args[1]), ".draft"), ".yaml")
		kind := configKind(args[2:])
		if kind == "" {
			switch {
			case strings.Contains(args[1], "/gateway/plans/") || strings.Contains(args[1], "/plans/"):
				kind = "&kind=gateway-plan"
			case strings.Contains(args[1], "/gateway/routes/") || strings.Contains(args[1], "/routes/"):
				kind = "&kind=gateway-route"
			case strings.Contains(args[1], "/scope/"):
				kind = "&kind=scope"
			}
		}
		return printAPI(cfg, http.MethodPost, "/v1/config/draft?id="+id+kind, bytes.NewReader(b))
	default:
		return fmt.Errorf("unknown config command: %s", args[0])
	}
}

func configKind(args []string) string {
	for _, a := range args {
		switch a {
		case "--gateway-plan":
			return "&kind=gateway-plan"
		case "--gateway-route":
			return "&kind=gateway-route"
		case "--gateway":
			return "&kind=gateway"
		case "--scope", "allow", "deny":
			return "&kind=scope"
		}
	}
	return ""
}

func fileBase(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
