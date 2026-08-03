package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
)

// AcquireBudgetFence obtains one host-wide fence for the sponsor shared by all
// configured networks. It prevents independent state directories from creating
// separate ledgers for the same hot wallet on one host.
func AcquireBudgetFence(sponsor common.Address) (ProcessFence, error) {
	if !processFenceSupported() {
		return nil, ErrFenceUnsupported
	}
	if sponsor == (common.Address{}) {
		return nil, errInvalidInput
	}
	if err := ensurePrivateFenceDirectory(processFenceRoot); err != nil {
		return nil, errFenceUnavailable
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/budget-fence/v1\x00"))
	_, _ = hash.Write(sponsor[:])
	path := filepath.Join(processFenceRoot, hex.EncodeToString(hash.Sum(nil)))
	return acquireProcessFenceFile(path)
}
