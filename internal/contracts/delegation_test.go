package contracts_test

import (
	"errors"
	"testing"

	"guard-daemon/internal/contracts"
)

func TestParseDelegation(t *testing.T) {
	t.Parallel()

	target := deterministicTestAddress(0xc1)
	code := append([]byte{0xef, 0x01, 0x00}, target[:]...)

	got, err := contracts.ParseDelegation(code)
	if err != nil || got != target {
		t.Fatalf("ParseDelegation() = %s, %v", got, err)
	}
}

func TestParseDelegationRejectsMalformedCode(t *testing.T) {
	t.Parallel()

	target := deterministicTestAddress(0xc1)
	valid := append([]byte{0xef, 0x01, 0x00}, target[:]...)

	tests := map[string][]byte{
		"empty":          nil,
		"short":          valid[:len(valid)-1],
		"trailing byte":  append(append([]byte(nil), valid...), 0),
		"wrong prefix":   append([]byte{0xef, 0x01, 0x01}, target[:]...),
		"cleared target": make([]byte, contracts.DelegationCodeLength),
	}
	tests["cleared target"][0] = 0xef
	tests["cleared target"][1] = 0x01

	for name, code := range tests {
		name, code := name, code
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := contracts.ParseDelegation(code); !errors.Is(err, contracts.ErrInvalidDelegation) {
				t.Fatalf("error = %v, want ErrInvalidDelegation", err)
			}
		})
	}
}
