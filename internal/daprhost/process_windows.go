//go:build windows

package daprhost

import (
	"os/exec"
	"strconv"
	"time"
)

func stopChildProcess(cmd *exec.Cmd, done <-chan struct{}, _ time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	default:
	}

	// Kill the entire process tree cleanly on Windows so child handles to SQLite/etcd are released.
	killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := killCmd.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
	<-done
	return nil
}
