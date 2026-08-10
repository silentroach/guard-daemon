package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
)

// AcquireBudgetFence получает общую для всех настроенных сетей блокировку спонсора
// на уровне хоста. Благодаря ей разные каталоги состояния не могут создавать отдельные
// реестры для одного горячего кошелька на одном хосте.
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
