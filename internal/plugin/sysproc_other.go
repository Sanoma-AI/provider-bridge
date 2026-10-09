// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package plugin

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the provider in its own process group. There is no
// parent-death signal outside Linux; the bridge kills providers on exit.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
