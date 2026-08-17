//go:build unix

package main

import (
	"os/exec"
	"testing"
)

func TestDaemonProcessStartsNewSessionOnUnix(t *testing.T) {
	cmd := exec.Command("ragrep")
	configureDaemonProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("daemon process does not start a new session")
	}
}
