//go:build unix

package ffrun

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the child in its own process group so that killTree
// can take down the child and any helpers it spawned in one signal, and so
// a terminal SIGINT aimed at the server does not reach ffmpeg first (the
// server cancels its contexts instead).
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree is the exec.Cmd.Cancel hook: SIGKILL the child's process group
// (pgid == pid thanks to Setpgid), then the child itself as a fallback.
// Reporting os.ErrProcessDone when nothing was left to kill lets exec treat
// a process that exited on its own as a clean exit.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	gerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	perr := cmd.Process.Kill()
	if gerr == nil || perr == nil {
		return nil
	}
	if errors.Is(gerr, syscall.ESRCH) || errors.Is(perr, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return gerr
}
