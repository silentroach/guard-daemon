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
	ErrFenceHeld        = errors.New("store: process fence already held")
	ErrFenceLost        = errors.New("store: process fence lost")
	ErrFenceUnsupported = errors.New("store: process fence is not supported")
	errFenceUnavailable = errors.New("store: process fence unavailable")
)

const processFenceRoot = "/var/tmp/guard-daemon-leases-v1"

// ProcessFence обеспечивает на одном хосте исключительное владение для заданных
// сети и спонсора. Между хостами исключительность по-прежнему обеспечивается тем,
// что активен только один хост или один экземпляр оператора.
type ProcessFence interface {
	Validate() error
	Release() error
}

// AcquireProcessFence получает неблокирующую файловую блокировку ядра по фиксированному
// пути, общему для хоста и не зависящему от окружения процесса и файлов хранилища.
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
