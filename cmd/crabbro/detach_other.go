//go:build !unix

package main

import "os/exec"

func detachDaemon(cmd *exec.Cmd) {}
