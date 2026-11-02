//go:build linux

package system

import (
	"syscall"
)

const (
	prSetDumpable = 4 // PR_SET_DUMPABLE
	prGetDumpable = 3 // PR_GET_DUMPABLE
)

// HardenProcess marks the agent process non-dumpable (and disables core dumps).
//
// Why: scrubbing a child's environment (ScrubEnv) is pointless if the child can
// simply read the agent's own /proc/<pid>/environ.
//
// What it guarantees, as verified on a Linux 6.x kernel:
//   - agent running as a NON-ROOT user: commands it spawns (same uid, no
//     CAP_SYS_PTRACE) can no longer read /proc/<agent pid>/environ or .../mem.
//   - agent running as ROOT (the stock Dockerfile): NOT sufficient on its own —
//     root-owned children could still read it in testing even with
//     CAP_SYS_PTRACE dropped from their bounding set. Run the container as a
//     non-root user for that protection (see README, "Hardening").
//
// Children are unaffected: execve resets the flag for them.
func HardenProcess() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// processDumpable reports PR_GET_DUMPABLE (1 = normal, 0 = disabled).
func processDumpable() (int, error) {
	r, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}
