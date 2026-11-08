//go:build unix

package gitmirror

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcess puts git in its own process group and makes context
// cancellation terminate the whole group. git spawns helpers such as
// git-remote-https; signalling only the parent would orphan them (leaving
// network connections open) when the daemon shuts down.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
}

// reapGroup force-kills stragglers after a cancelled command.
func reapGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
