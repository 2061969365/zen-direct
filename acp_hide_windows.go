//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func hideAcpWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// hideSidecarWindow：bun 是控制台程序，双加 CREATE_NO_WINDOW，
// 否则 Hidden 启动（无控制台）时仍可能闪一个空窗口。
func hideSidecarWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
