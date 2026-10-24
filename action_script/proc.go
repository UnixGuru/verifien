package actionscript

import (
	"context"
	"os/exec"
	"syscall"
)

// command builds the shell command for a script. It puts the script in its own
// process group and makes a cancel (the timeout) kill the whole group, so a
// child the script started (sleep, curl, ...) does not outlive it.
func command(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
