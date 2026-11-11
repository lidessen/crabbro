// Chiling's desktop UI: a native window MyGo draws from Go, showing the
// traffic, rules, and trajectory of chilingd. It is a client of the same
// control socket the CLI uses; the daemon owns the proxy, and closing the
// window leaves capture, rules, and MITM running.
//
//	go run ./cmd/chiling-ui
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
	"github.com/lidessen/chiling/pkg/client"
)

const (
	titleBarH  = 42
	statusBarH = 24
)

// expandPath turns a leading ~ into the home folder.
func expandPath(p string) string {
	if p == "~" || len(p) > 1 && p[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// readToken is the api.token file beside the socket, which the TCP fallback
// presents as its bearer token. The CLI reads the same file.
func readToken(udsPath string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(expandPath(udsPath)), "api.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func main() {
	log.SetFlags(0)
	udsPath := flag.String("uds-path", api.DefaultUDSPath, "control API Unix socket")
	tcpAddr := flag.String("tcp-addr", api.DefaultTCPAddr, "control API TCP fallback")
	token := flag.String("api-token", "", "bearer token for the TCP fallback (default: api.token beside the socket)")
	flag.Parse()

	*token = strings.TrimSpace(*token)
	if *token == "" {
		*token = readToken(*udsPath)
	}

	ctx := context.Background()
	a := newApp(client.New(client.Options{
		UDS:   expandPath(*udsPath),
		TCP:   *tcpAddr,
		Token: *token,
	}), ctx)
	go a.run(ctx)
	// BISECT: no SetMenu

	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:                "Chiling",
			Width:                1280,
			Height:               820,
			MinWidth:             860,
			MinHeight:            540,
			TitleBarStyle:        mygo.TitleBarHidden,
			TitleBarHeight:       titleBarH,
			TrafficLightPosition: &mygo.Point{X: 14, Y: 15},
			Content:              ui.View(a.view),
		})
		a.win = win
		win.OnClosed(func() { mygo.App.Quit() })
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
