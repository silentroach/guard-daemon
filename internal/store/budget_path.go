package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
)

const budgetLedgerRoot = "/var/lib/guard-daemon"

// CanonicalBudgetPath binds one sponsor to one host-wide ledger independently
// of operator-selected state directories.
func CanonicalBudgetPath(sponsor common.Address) (string, error) {
	if sponsor == (common.Address{}) {
		return "", errInvalidInput
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/canonical-budget/v1\x00"))
	_, _ = hash.Write(sponsor[:])
	return filepath.Join(budgetLedgerRoot, hex.EncodeToString(hash.Sum(nil))+".db"), nil
}

func CanonicalAdmissionPath(sponsor common.Address) (string, error) {
	if sponsor == (common.Address{}) {
		return "", errInvalidInput
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/canonical-admission/v1\x00"))
	_, _ = hash.Write(sponsor[:])
	return filepath.Join(budgetLedgerRoot, hex.EncodeToString(hash.Sum(nil))+".admission.db"), nil
}

func CanonicalAlertPath(sponsor common.Address) (string, error) {
	if sponsor == (common.Address{}) {
		return "", errInvalidInput
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/canonical-alerts/v1\x00"))
	_, _ = hash.Write(sponsor[:])
	return filepath.Join(budgetLedgerRoot, hex.EncodeToString(hash.Sum(nil))+".alerts.json"), nil
}
