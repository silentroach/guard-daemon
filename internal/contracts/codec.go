package contracts

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var ErrInvalidReturnData = errors.New("invalid contract return data")

func decodeSingle(outputs abi.Arguments, data []byte) (any, error) {
	values, err := outputs.Unpack(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReturnData, err)
	}
	if len(values) != 1 {
		return nil, ErrInvalidReturnData
	}

	canonical, err := outputs.Pack(values...)
	if err != nil || !bytes.Equal(data, canonical) {
		return nil, ErrInvalidReturnData
	}
	return values[0], nil
}
