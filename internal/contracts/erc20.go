package contracts

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const erc20ABI = `[
	{"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"symbol","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
	{"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
	{"name":"Transfer","type":"event","anonymous":false,"inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"value","type":"uint256","indexed":false}]}
]`

type ERC20Codec struct {
	contractABI abi.ABI
}

func NewERC20Codec() (*ERC20Codec, error) {
	contractABI, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse ERC-20 ABI: %w", err)
	}
	return &ERC20Codec{contractABI: contractABI}, nil
}

func (codec *ERC20Codec) PackBalanceOf(account common.Address) ([]byte, error) {
	return codec.contractABI.Pack("balanceOf", account)
}

func (codec *ERC20Codec) DecodeBalanceOf(data []byte) (*big.Int, error) {
	value, err := decodeSingle(codec.contractABI.Methods["balanceOf"].Outputs, data)
	if err != nil {
		return nil, err
	}
	balance, ok := value.(*big.Int)
	if !ok {
		return nil, ErrInvalidReturnData
	}
	return new(big.Int).Set(balance), nil
}

func (codec *ERC20Codec) PackSymbol() ([]byte, error) {
	return codec.contractABI.Pack("symbol")
}

func (codec *ERC20Codec) DecodeSymbol(data []byte) (string, error) {
	value, err := decodeSingle(codec.contractABI.Methods["symbol"].Outputs, data)
	if err != nil {
		return "", err
	}
	symbol, ok := value.(string)
	if !ok {
		return "", ErrInvalidReturnData
	}
	return symbol, nil
}

func (codec *ERC20Codec) PackDecimals() ([]byte, error) {
	return codec.contractABI.Pack("decimals")
}

func (codec *ERC20Codec) DecodeDecimals(data []byte) (uint8, error) {
	value, err := decodeSingle(codec.contractABI.Methods["decimals"].Outputs, data)
	if err != nil {
		return 0, err
	}
	decimals, ok := value.(uint8)
	if !ok {
		return 0, ErrInvalidReturnData
	}
	return decimals, nil
}

func (codec *ERC20Codec) TransferTopic() common.Hash {
	return codec.contractABI.Events["Transfer"].ID
}
