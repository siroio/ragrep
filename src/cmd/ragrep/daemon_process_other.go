//go:build !windows && !unix

package main

import "os/exec"

func configureDaemonProcess(_ *exec.Cmd) {}

func daemonProcessConfigured(cmd *exec.Cmd) bool { return cmd.SysProcAttr == nil }
