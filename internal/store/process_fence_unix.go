//go:build linux || darwin

package store

import (
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

type unixProcessFence struct {
	mu       sync.Mutex
	fd       int
	path     string
	identity unixFileIdentity
	released bool
}

type unixFileIdentity struct {
	device uint64
	inode  uint64
}

func processFenceSupported() bool { return true }

func acquireProcessFenceFile(path string) (ProcessFence, error) {
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, errFenceUnavailable
	}
	fail := func(public error) (ProcessFence, error) {
		_ = unix.Close(fd)
		return nil, public
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(errFenceUnavailable)
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fail(errFenceUnavailable)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail(ErrFenceHeld)
		}
		return fail(errFenceUnavailable)
	}
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil || pathStat.Mode&unix.S_IFMT != unix.S_IFREG || fileIdentity(pathStat) != fileIdentity(stat) {
		return fail(errFenceUnavailable)
	}
	return &unixProcessFence{fd: fd, path: path, identity: fileIdentity(stat)}, nil
}

func (fence *unixProcessFence) Validate() error {
	if fence == nil {
		return ErrFenceLost
	}
	fence.mu.Lock()
	defer fence.mu.Unlock()
	if fence.released || fence.fd < 0 {
		return ErrFenceLost
	}

	var fdStat unix.Stat_t
	if err := unix.Fstat(fence.fd, &fdStat); err != nil || fdStat.Mode&unix.S_IFMT != unix.S_IFREG || fileIdentity(fdStat) != fence.identity {
		return ErrFenceLost
	}
	var pathStat unix.Stat_t
	if err := unix.Lstat(fence.path, &pathStat); err != nil || pathStat.Mode&unix.S_IFMT != unix.S_IFREG || fileIdentity(pathStat) != fence.identity {
		return ErrFenceLost
	}
	probe, err := unix.Open(fence.path, unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_RDONLY, 0)
	if err != nil {
		return ErrFenceLost
	}
	var probeStat unix.Stat_t
	if err := unix.Fstat(probe, &probeStat); err != nil || probeStat.Mode&unix.S_IFMT != unix.S_IFREG || fileIdentity(probeStat) != fence.identity {
		_ = unix.Close(probe)
		return ErrFenceLost
	}
	probeLockErr := unix.Flock(probe, unix.LOCK_EX|unix.LOCK_NB)
	if probeLockErr == nil {
		_ = unix.Flock(probe, unix.LOCK_UN)
		_ = unix.Close(probe)
		return ErrFenceLost
	}
	if (!errors.Is(probeLockErr, unix.EWOULDBLOCK) && !errors.Is(probeLockErr, unix.EAGAIN)) || unix.Close(probe) != nil {
		return ErrFenceLost
	}
	if err := unix.Flock(fence.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return ErrFenceLost
	}
	if err := unix.Lstat(fence.path, &pathStat); err != nil || pathStat.Mode&unix.S_IFMT != unix.S_IFREG || fileIdentity(pathStat) != fence.identity {
		return ErrFenceLost
	}
	return nil
}

func fileIdentity(stat unix.Stat_t) unixFileIdentity {
	return unixFileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func (fence *unixProcessFence) Release() error {
	if fence == nil {
		return nil
	}
	fence.mu.Lock()
	defer fence.mu.Unlock()
	if fence.released {
		return nil
	}

	unlockErr := unix.Flock(fence.fd, unix.LOCK_UN)
	closeErr := unix.Close(fence.fd)
	fence.fd = -1
	fence.released = true
	if unlockErr != nil || closeErr != nil {
		return ErrFenceLost
	}
	return nil
}
