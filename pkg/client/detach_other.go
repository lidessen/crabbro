//go:build !unix

package client

import "os/exec"

func detachDaemon(cmd *exec.Cmd) {}
