package contracts

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const rescuerABI = `[
	{"name":"executeAndSweep","type":"function","stateMutability":"payable","inputs":[{"name":"target","type":"address"},{"name":"data","type":"bytes"},{"name":"tokens","type":"address[]"}],"outputs":[]},
	{"name":"sweepAll","type":"function","stateMutability":"nonpayable","inputs":[{"name":"tokens","type":"address[]"}],"outputs":[]},
	{"name":"sweepEth","type":"function","stateMutability":"nonpayable","inputs":[],"outputs":[]},
	{"name":"destination","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`

type RescuerCodec struct {
	contractABI abi.ABI
}

func NewRescuerCodec() (*RescuerCodec, error) {
	contractABI, err := abi.JSON(strings.NewReader(rescuerABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse Rescuer ABI: %w", err)
	}
	return &RescuerCodec{contractABI: contractABI}, nil
}

func (codec *RescuerCodec) PackSweepAll(tokens []common.Address) ([]byte, error) {
	return codec.contractABI.Pack("sweepAll", tokens)
}

func (codec *RescuerCodec) PackSweepEth() ([]byte, error) {
	return codec.contractABI.Pack("sweepEth")
}

func (codec *RescuerCodec) PackExecuteAndSweep(target common.Address, data []byte, tokens []common.Address) ([]byte, error) {
	return codec.contractABI.Pack("executeAndSweep", target, data, tokens)
}

func (codec *RescuerCodec) PackDestination() ([]byte, error) {
	return codec.contractABI.Pack("destination")
}

func (codec *RescuerCodec) DecodeDestination(data []byte) (common.Address, error) {
	value, err := decodeSingle(codec.contractABI.Methods["destination"].Outputs, data)
	if err != nil {
		return common.Address{}, err
	}
	destination, ok := value.(common.Address)
	if !ok {
		return common.Address{}, ErrInvalidReturnData
	}
	return destination, nil
}
