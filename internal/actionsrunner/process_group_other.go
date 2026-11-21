//go:build !linux

package actionsrunner

import "os/exec"

func setRunnerProcessGroup(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
