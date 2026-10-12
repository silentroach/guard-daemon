package contracts_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"guard-daemon/internal/contracts"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestERC20Packing(t *testing.T) {
	t.Parallel()

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		t.Fatal(err)
	}
	account := deterministicTestAddress(0xa1)

	balanceOf, err := codec.PackBalanceOf(account)
	if err != nil {
		t.Fatal(err)
	}
	wantBalanceOf := mustDecodeHex(t, "70a0823100000000000000000000000000000000000000000000000000000000000000a1")
	if !bytes.Equal(balanceOf, wantBalanceOf) {
		t.Fatalf("balanceOf input = %x, want %x", balanceOf, wantBalanceOf)
	}

	symbolCall, err := codec.PackSymbol()
	if err != nil {
		t.Fatal(err)
	}
	assertBytes(t, "symbol calldata", symbolCall, selector("symbol()"))
	decimalsCall, err := codec.PackDecimals()
	if err != nil {
		t.Fatal(err)
	}
	assertBytes(t, "decimals calldata", decimalsCall, selector("decimals()"))
	if got, want := codec.TransferTopic(), common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"); got != want {
		t.Fatalf("Transfer topic = %s, want %s", got, want)
	}
}

func TestERC20StrictDecoding(t *testing.T) {
	t.Parallel()

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		t.Fatal(err)
	}

	balanceData := make([]byte, 32)
	big.NewInt(123456).FillBytes(balanceData)
	balance, err := codec.DecodeBalanceOf(balanceData)
	if err != nil || balance.Cmp(big.NewInt(123456)) != 0 {
		t.Fatalf("DecodeBalanceOf() returned %v, %v", balance, err)
	}

	symbolData := mustDecodeHex(t, "000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000034c4f430000000000000000000000000000000000000000000000000000000000")
	symbol, err := codec.DecodeSymbol(symbolData)
	if err != nil || symbol != "LOC" {
		t.Fatalf("DecodeSymbol() returned %q, %v", symbol, err)
	}

	decimalsData := make([]byte, 32)
	decimalsData[31] = 6
	decimals, err := codec.DecodeDecimals(decimalsData)
	if err != nil || decimals != 6 {
		t.Fatalf("DecodeDecimals() returned %d, %v", decimals, err)
	}

	for name, decode := range map[string]func([]byte) error{
		"balance trailing byte": func(data []byte) error { _, err := codec.DecodeBalanceOf(data); return err },
		"symbol trailing byte":  func(data []byte) error { _, err := codec.DecodeSymbol(data); return err },
		"decimals too short":    func(data []byte) error { _, err := codec.DecodeDecimals(data); return err },
	} {
		name, decode := name, decode
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var data []byte
			switch name {
			case "balance trailing byte":
				data = append(append([]byte(nil), balanceData...), 0)
			case "symbol trailing byte":
				data = append(append([]byte(nil), symbolData...), 0)
			default:
				data = decimalsData[:31]
			}
			if err := decode(data); !errors.Is(err, contracts.ErrInvalidReturnData) {
				t.Fatalf("error = %v, want ErrInvalidReturnData", err)
			}
		})
	}
}

func TestRescuerPackingAndDestinationDecoding(t *testing.T) {
	t.Parallel()

	codec, err := contracts.NewRescuerCodec()
	if err != nil {
		t.Fatal(err)
	}
	tokens := []common.Address{
		deterministicTestAddress(0xa1),
		deterministicTestAddress(0xb2),
	}

	sweepAll, err := codec.PackSweepAll(tokens)
	if err != nil {
		t.Fatal(err)
	}
	wantSweepAll := append(selector("sweepAll(address[])"), mustDecodeHex(t,
		"0000000000000000000000000000000000000000000000000000000000000020"+
			"0000000000000000000000000000000000000000000000000000000000000002"+
			"00000000000000000000000000000000000000000000000000000000000000a1"+
			"00000000000000000000000000000000000000000000000000000000000000b2")...)
	assertBytes(t, "sweepAll calldata", sweepAll, wantSweepAll)
	sweepEth, err := codec.PackSweepEth()
	if err != nil {
		t.Fatal(err)
	}
	assertBytes(t, "sweepEth calldata", sweepEth, selector("sweepEth()"))
	destinationCall, err := codec.PackDestination()
	if err != nil {
		t.Fatal(err)
	}
	assertBytes(t, "destination calldata", destinationCall, selector("destination()"))

	destinationData := make([]byte, 32)
	destination := deterministicTestAddress(0xd1)
	copy(destinationData[12:], destination[:])
	got, err := codec.DecodeDestination(destinationData)
	if err != nil || got != destination {
		t.Fatalf("DecodeDestination() returned %s, %v", got, err)
	}
	if _, err := codec.DecodeDestination(append(destinationData, 0)); !errors.Is(err, contracts.ErrInvalidReturnData) {
		t.Fatalf("trailing data error = %v, want ErrInvalidReturnData", err)
	}
}

func selector(signature string) []byte {
	return crypto.Keccak256([]byte(signature))[:4]
}

func deterministicTestAddress(lastByte byte) common.Address {
	var address common.Address
	address[len(address)-1] = lastByte
	return address
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func assertBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %x, want %x", name, got, want)
	}
}
