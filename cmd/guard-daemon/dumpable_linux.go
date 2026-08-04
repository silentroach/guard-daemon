//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func disableProcessDumps() error {
	limit := unix.Rlimit{}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &limit); err != nil {
		return fmt.Errorf("setrlimit RLIMIT_CORE: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %w", err)
	}
	return nil
}
