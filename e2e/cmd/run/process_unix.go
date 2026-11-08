//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func configureProcess(cmd *exec.Cmd)  { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func interruptProcess(cmd *exec.Cmd)  { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
func killProcess(cmd *exec.Cmd)       { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
