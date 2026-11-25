package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lidessen/crabbro/internal/shared"
)

func cmdSecrets(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro secrets <list|set|delete>")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("secrets list", flag.ContinueOnError)
		pretty := fs.Bool("pretty", false, "Human-readable output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		resp, err := doRequest(cfg, http.MethodGet, "/v1/secrets", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
		}
		if !*pretty {
			fmt.Println(string(raw))
			return nil
		}
		var out shared.SecretListResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			fmt.Println(string(raw))
			return nil
		}
		if len(out.Secrets) == 0 {
			fmt.Println("no secrets stored")
			return nil
		}
		for _, s := range out.Secrets {
			fmt.Printf("%-28s %6dB  updated=%s\n", s.Name, s.SizeBytes, s.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
		}
		return nil
	case "set":
		fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
		fromEnv := fs.String("from-env", "", "Read secret value from environment variable")
		setArgs := args[1:]
		name := ""
		nameFirst := false
		if len(setArgs) > 0 && !strings.HasPrefix(setArgs[0], "-") {
			name, setArgs = setArgs[0], setArgs[1:]
			nameFirst = true
		}
		if err := fs.Parse(setArgs); err != nil {
			return err
		}
		if !nameFirst && fs.NArg() == 1 {
			name = fs.Arg(0)
		} else if !nameFirst || fs.NArg() != 0 {
			return fmt.Errorf("usage: crabbro secrets set <name> [--from-env VAR]")
		}
		if name == "" {
			return fmt.Errorf("usage: crabbro secrets set <name> [--from-env VAR]")
		}
		var val string
		if *fromEnv != "" {
			val = os.Getenv(*fromEnv)
		} else {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
			if err != nil {
				return err
			}
			val = string(b)
		}
		val = strings.TrimSpace(val)
		if val == "" {
			return fmt.Errorf("secret value is empty (pipe value via stdin or pass --from-env VAR)")
		}
		body, _ := json.Marshal(map[string]string{"name": name, "value": val})
		return printAPI(cfg, http.MethodPost, "/v1/secrets", bytes.NewReader(body))
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: crabbro secrets delete <name>")
		}
		return printAPI(cfg, http.MethodDelete, "/v1/secrets/"+args[1], nil)
	default:
		return fmt.Errorf("unknown secrets command: %s", args[0])
	}
}
