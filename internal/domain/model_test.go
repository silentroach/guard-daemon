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
		t.Fatal("одинаковое наблюдение получило разные stable candidate ID")
	}
	if first.ID == NewLogCandidate(31337, source, token, blockHash, txHash, 10, 2).ID {
		t.Fatal("разные log index получили одинаковый candidate ID")
	}

	periodic := NewPeriodicCandidate(31337, source, 7, 1)
	if periodic.ID != NewPeriodicCandidate(31337, source, 8, 1).ID {
		t.Fatal("reconnect изменил stable ID одного periodic observation")
	}
	if periodic.ID == NewPeriodicCandidate(31337, source, 7, 2).ID {
		t.Fatal("последовательные periodic observations были ошибочно дедуплицированы")
	}
}
