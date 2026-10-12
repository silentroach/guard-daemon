//go:build !linux

package main

func disableProcessDumps() error {
	return nil
}
