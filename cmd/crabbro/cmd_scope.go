package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

func cmdSources(cfg *clientConfig, args []string) error {
	if len(args) < 1 || args[0] == "list" {
		return printAPI(cfg, http.MethodGet, "/v1/sources", nil)
	}
	if args[0] == "get" && len(args) >= 2 {
		return printAPI(cfg, http.MethodGet, "/v1/sources/"+args[1], nil)
	}
	return fmt.Errorf("usage: crabbro sources <list|get> [id]")
}

func cmdScope(cfg *clientConfig, args []string) error {
	if len(args) < 1 || args[0] == "status" {
		return printAPI(cfg, http.MethodGet, "/v1/scope", nil)
	}
	op := args[0]
	expr := args[1:]
	switch op {
	case "pick", "omit", "off", "on", "reset", "bypass":
	default:
		return fmt.Errorf("usage: crabbro scope <status|pick|omit|off|on|reset|bypass> [expr...]")
	}
	body, _ := json.Marshal(map[string]any{"op": op, "expr": expr})
	return printAPI(cfg, http.MethodPost, "/v1/scope", bytes.NewReader(body))
}
