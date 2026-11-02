//go:build !linux

package system

// HardenProcess is a no-op outside Linux.
func HardenProcess() error { return nil }
