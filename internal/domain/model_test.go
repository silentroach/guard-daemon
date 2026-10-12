package domain

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestCandidateIDIsStableAndVersionedByObservation(t *testing.T) {
	source := common.Address{19: 1}
	token := Token{Address: common.Address{19: 2}, Symbol: "TEST", Decimals: 18}
	blockHash := common.Hash{31: 3}
	txHash := common.Hash{31: 4}

	first := NewLogCandidate(31337, source, token, blockHash, txHash, 10, 1)
	duplicate := NewLogCandidate(31337, source, token, blockHash, txHash, 10, 1)
	if first.ID != duplicate.ID {
		t.Fatal("identical observation produced different stable candidate IDs")
	}
	if first.ID == NewLogCandidate(31337, source, token, blockHash, txHash, 10, 2).ID {
		t.Fatal("logs with different indexes produced the same candidate ID")
	}

	periodic := NewPeriodicCandidate(31337, source, 7, 1)
	if periodic.ID != NewPeriodicCandidate(31337, source, 8, 1).ID {
		t.Fatal("reconnect changed the stable ID of the same polling observation")
	}
	if periodic.ID == NewPeriodicCandidate(31337, source, 7, 2).ID {
		t.Fatal("successive polling observations were incorrectly deduplicated")
	}
}

func TestCandidateValidationAndTokenReconciliationIdentity(t *testing.T) {
	source := common.Address{19: 1}
	token := Token{Address: common.Address{19: 2}, Symbol: "UNTRUSTED", Decimals: 18}
	first := NewTokenReconciliationCandidate(31337, source, token, 1, 7)
	second := NewTokenReconciliationCandidate(31337, source, token, 99, 7)
	if first.ID != second.ID {
		t.Fatal("RPC generation changed the stable reconciliation ID")
	}
	if err := ValidateCandidate(first); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	first.ID[0] ^= 1
	if err := ValidateCandidate(first); err == nil {
		t.Fatal("candidate with a corrupted ID accepted")
	}
}
