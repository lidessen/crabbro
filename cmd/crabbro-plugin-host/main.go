// Command crabbro-plugin-host is the Node supervisor entry. The daemon starts
// the same runtime directly; this command extracts it and execs node.
package main

import (
	"fmt"
	"os"

	"github.com/lidessen/crabbro/internal/plugin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: crabbro-plugin-host <socket-path>")
		os.Exit(2)
	}
	if err := plugin.ExecHost(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
