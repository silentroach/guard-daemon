//go:build unix

package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestBudgetFenceIsHostWidePerSponsor(t *testing.T) {
	sponsor := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	first, err := AcquireBudgetFence(sponsor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })
	if second, err := AcquireBudgetFence(sponsor); !errors.Is(err, ErrFenceHeld) {
		if second != nil {
			_ = second.Release()
		}
		t.Fatalf("повторный budget fence = (%v, %v)", second, err)
	}
	other, err := AcquireBudgetFence(common.HexToAddress("0x00000000000000000000000000000000000000a2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBudgetPathDependsOnlyOnSponsor(t *testing.T) {
	sponsor := common.HexToAddress("0x00000000000000000000000000000000000000b1")
	first, err := CanonicalBudgetPath(sponsor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalBudgetPath(sponsor)
	if err != nil || first != second {
		t.Fatalf("canonical paths = %q, %q, error=%v", first, second, err)
	}
	if !strings.HasPrefix(first, "/var/lib/guard-daemon/") {
		t.Fatalf("canonical budget path is not durable: %q", first)
	}
	other, err := CanonicalBudgetPath(common.HexToAddress("0x00000000000000000000000000000000000000b2"))
	if err != nil || other == first {
		t.Fatalf("other sponsor path = %q, error=%v", other, err)
	}
}
