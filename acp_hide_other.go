//go:build !windows

package main

import "os/exec"

func hideAcpWindow(cmd *exec.Cmd) {}

func hideSidecarWindow(cmd *exec.Cmd) {}
