package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// ClaimPlan describes one operator-supplied claim and the token to sweep in the
// same transaction. Its digest binds the durable operation to every parameter.
type ClaimPlan struct {
	Target          common.Address
	Data            []byte
	Value           uint256.Int
	Token           common.Address
	MinimumReceived uint256.Int
	GasLimit        uint64
	MaximumCost     uint256.Int
	ValidUntil      uint64
}

func (plan ClaimPlan) Validate() error {
	if plan.Target == (common.Address{}) || plan.Token == (common.Address{}) || len(plan.Data) < 4 || len(plan.Data) > 16<<10 ||
		plan.MinimumReceived.IsZero() || plan.GasLimit == 0 || plan.MaximumCost.IsZero() || plan.Value.Gt(&plan.MaximumCost) || plan.ValidUntil == 0 {
		return errors.New("invalid claim request")
	}
	return nil
}

func (plan ClaimPlan) Clone() ClaimPlan {
	plan.Data = append([]byte(nil), plan.Data...)
	return plan
}

func (plan ClaimPlan) Digest() common.Hash {
	hash := sha256.New()
	hash.Write([]byte("guard-daemon/claim/v1"))
	hash.Write(plan.Target[:])
	hash.Write(plan.Token[:])
	for _, value := range []uint256.Int{plan.Value, plan.MinimumReceived, plan.MaximumCost} {
		encoded := value.Bytes32()
		hash.Write(encoded[:])
	}
	var encoded [8]byte
	for _, value := range []uint64{plan.GasLimit, plan.ValidUntil, uint64(len(plan.Data))} {
		binary.BigEndian.PutUint64(encoded[:], value)
		hash.Write(encoded[:])
	}
	hash.Write(plan.Data)
	return common.BytesToHash(hash.Sum(nil))
}

func NewClaimCandidate(network NetworkID, source common.Address, plan ClaimPlan) RescueCandidate {
	candidate := RescueCandidate{Network: network, Kind: CandidateClaim, Source: source, Token: Token{Address: plan.Token}, TxHash: plan.Digest()}
	candidate.ID = candidateID(candidate)
	return candidate
}
