package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
)

var (
	ErrFenceHeld        = errors.New("хранилище: process fence уже удерживается")
	ErrFenceLost        = errors.New("хранилище: process fence потерян")
	ErrFenceUnsupported = errors.New("хранилище: process fence не поддерживается")
	errFenceUnavailable = errors.New("хранилище: process fence недоступен")
)

const processFenceRoot = "/var/tmp/guard-daemon-leases-v1"

// ProcessFence is a host-local guard for one chain+sponsor pair. Multi-host
// exclusivity still requires one active host/operator deployment (Task10).
type ProcessFence interface {
	Validate() error
	Release() error
}

// AcquireProcessFence obtains a nonblocking kernel-owned advisory lock at a
// fixed host-wide path independent of process user environment and store files.
func AcquireProcessFence(key LeaseKey) (ProcessFence, error) {
	if !processFenceSupported() {
		return nil, ErrFenceUnsupported
	}
	if key.Network <= 0 || key.Sponsor == (common.Address{}) {
		return nil, errInvalidInput
	}

	path, err := processFencePath(key)
	if err != nil {
		return nil, errFenceUnavailable
	}
	return acquireProcessFenceFile(path)
}

func processFencePath(key LeaseKey) (string, error) {
	if err := ensurePrivateFenceDirectory(processFenceRoot); err != nil {
		return "", errFenceUnavailable
	}

	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/process-fence/v1\x00"))
	var network [8]byte
	binary.BigEndian.PutUint64(network[:], uint64(key.Network))
	_, _ = hash.Write(network[:])
	_, _ = hash.Write(key.Sponsor[:])
	return filepath.Join(processFenceRoot, hex.EncodeToString(hash.Sum(nil))), nil
}

func ensurePrivateFenceDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errFenceUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errFenceUnavailable
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return errFenceUnavailable
	}
	return nil
}
