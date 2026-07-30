package contracts

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

const DelegationCodeLength = 23

var ErrInvalidDelegation = errors.New("invalid EIP-7702 delegation indicator")

func ParseDelegation(code []byte) (common.Address, error) {
	if len(code) != DelegationCodeLength || code[0] != 0xef || code[1] != 0x01 || code[2] != 0x00 {
		return common.Address{}, ErrInvalidDelegation
	}

	target := common.BytesToAddress(code[3:])
	if target == (common.Address{}) {
		return common.Address{}, ErrInvalidDelegation
	}
	return target, nil
}
