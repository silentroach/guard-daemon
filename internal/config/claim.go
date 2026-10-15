package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

type claimDocument struct {
	ChainID            int64  `json:"chainId"`
	Target             string `json:"target"`
	Data               string `json:"data"`
	ValueWei           string `json:"valueWei"`
	Token              string `json:"token"`
	MinimumReceivedRaw string `json:"minimumReceivedRaw"`
	GasLimit           uint64 `json:"gasLimit"`
	MaximumCostWei     string `json:"maximumCostWei"`
	ValidUntil         uint64 `json:"validUntil"`
}

func loadClaim(lookup func(string) (string, bool), suffix string, chain domain.NetworkID, tokens []domain.Token, allowUnknown bool) (*domain.ClaimPlan, error) {
	name := "CLAIM_REQUEST_" + suffix
	path, present := lookup(name)
	if !present {
		return nil, nil
	}
	failure := fmt.Errorf("environment variable %s must reference a valid claim request for this network and an allowed token", name)
	file, err := os.Open(path)
	if err != nil {
		return nil, failure
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(content) > 64<<10 {
		return nil, failure
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var document claimDocument
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document.ChainID != int64(chain) ||
		!common.IsHexAddress(document.Target) || !common.IsHexAddress(document.Token) || len(document.Data) < 2 || document.Data[:2] != "0x" {
		return nil, failure
	}
	data, err := hex.DecodeString(document.Data[2:])
	if err != nil {
		return nil, failure
	}
	plan := domain.ClaimPlan{Target: common.HexToAddress(document.Target), Token: common.HexToAddress(document.Token), Data: data, GasLimit: document.GasLimit, ValidUntil: document.ValidUntil}
	for _, entry := range []struct {
		encoded string
		value   *uint256.Int
	}{{document.ValueWei, &plan.Value}, {document.MinimumReceivedRaw, &plan.MinimumReceived}, {document.MaximumCostWei, &plan.MaximumCost}} {
		value, ok := new(big.Int).SetString(entry.encoded, 10)
		if !decimalDigits(entry.encoded) || !ok || value.Sign() < 0 || entry.value.SetFromBig(value) {
			return nil, failure
		}
	}
	if plan.Validate() != nil {
		return nil, failure
	}
	if allowUnknown {
		return &plan, nil
	}
	for _, token := range tokens {
		if token.Address == plan.Token {
			return &plan, nil
		}
	}
	return nil, failure
}
