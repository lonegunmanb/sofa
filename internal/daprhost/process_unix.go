//go:build !windows

package daprhost

import (
	"os/exec"
	"syscall"
	"time"
)

func stopChildProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	default:
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	if grace <= 0 {
		grace = 3 * time.Second
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return nil
	}
}
