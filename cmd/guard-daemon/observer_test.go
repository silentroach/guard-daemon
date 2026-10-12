package main

import (
	"bytes"
	"strings"
	"testing"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"

	"github.com/ethereum/go-ethereum/common"
)

func TestSafeConsoleObserverOmitsIncidentSpecificAndUntrustedFields(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	observer := newSafeConsoleObserver(&output)
	observer.Record(observability.Event{
		Level:       observability.LevelError,
		Code:        "safe_event",
		NetworkName: "local-network",
		Candidate:   domain.CandidateID{1},
		TokenSymbol: "private-marker",
		Amount:      "sensitive-amount",
		TxHash:      common.Hash{2},
		ErrorCode:   "raw detail with spaces",
	})

	got := output.String()
	for _, forbidden := range []string{"private-marker", "sensitive-amount", common.Hash{2}.Hex(), "raw detail with spaces"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("console output exposed %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe_event") || !strings.Contains(got, "error_redacted") {
		t.Fatalf("console output lost safe event classification: %q", got)
	}
}
