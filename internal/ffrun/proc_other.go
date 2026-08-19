//go:build !unix

package ffrun

import "os/exec"

// setSysProcAttr is a no-op where process groups are unavailable (Windows
// builds are for local development only; the runtime image is Linux).
func setSysProcAttr(*exec.Cmd) {}

// killTree kills just the child process; grandchildren are not tracked.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
