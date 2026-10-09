// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the provider in its own process group and has the
// kernel SIGKILL it if the bridge dies without cleaning up.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
