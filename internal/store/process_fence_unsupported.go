//go:build !linux && !darwin

package store

func processFenceSupported() bool { return false }

func acquireProcessFenceFile(string) (ProcessFence, error) {
	return nil, ErrFenceUnsupported
}
