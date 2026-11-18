// Package proc manages the detached daemon process via a pid file.
package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Paths struct{ StateDir string }

func (p Paths) PID() string { return filepath.Join(p.StateDir, "daemon.pid") }
func (p Paths) Log() string { return filepath.Join(p.StateDir, "daemon.log") }

// Running returns the daemon pid when it is alive.
func (p Paths) Running() (int, bool) {
	raw, err := os.ReadFile(p.PID())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0, false
	}
	return pid, true
}

// WritePID records the current process.
func (p Paths) WritePID() error {
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.PID(), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// RemovePID deletes the pid file if it still names this process.
func (p Paths) RemovePID() {
	if pid, ok := p.Running(); ok && pid != os.Getpid() {
		return
	}
	_ = os.Remove(p.PID())
}

// Spawn starts `self daemon` detached, stdout/stderr appended to the log.
func (p Paths) Spawn() (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(p.Log(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// Stop terminates the daemon and waits up to 5s.
func (p Paths) Stop() error {
	pid, ok := p.Running()
	if !ok {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		if _, ok := p.Running(); !ok {
			_ = os.Remove(p.PID())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(p.PID())
	return fmt.Errorf("daemon %d did not exit on SIGTERM; killed", pid)
}
