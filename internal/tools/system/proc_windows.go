//go:build windows

package system

import (
	"os/exec"
)

// configureProcessGroup is a no-op on Windows; CommandContext cancel uses
// process.Kill which is enough for the direct child. Nested shells may still
// outlive the parent on timeout — acceptable for local Windows dev.
func configureProcessGroup(cmd *exec.Cmd) {
	// Intentionally empty: Setpgid / negative-PID Kill are Unix-only.
}
