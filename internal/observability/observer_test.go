package observability

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

func TestLoggerWritesClosedSafeSchema(t *testing.T) {
	t.Parallel()

	classified, err := NewSafeError(ErrorRPCTransient, ErrorRPCUnavailable, true, true)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := NewPublicTxHashAfterBroadcast(common.HexToHash("0x1234"))
	if err != nil {
		t.Fatal(err)
	}
	incident := domain.IncidentID{1, 2, 3}
	var output bytes.Buffer
	logger := NewLogger(&output)
	if err := logger.Write(SafeEvent{
		Level:    LevelWarning,
		Code:     LogBroadcastAccepted,
		ChainID:  8453,
		Incident: incident,
		State:    DurableAmbiguous,
		Result:   ResultAmbiguous,
		TxHash:   hash,
		Error:    &classified,
	}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &got); err != nil {
		t.Fatalf("записан невалидный JSON: %v", err)
	}
	want := map[string]any{
		"level":         "warning",
		"code":          "broadcast_accepted",
		"chain_id":      float64(8453),
		"incident":      incident.String(),
		"durable_state": "ambiguous",
		"result":        "ambiguous",
		"tx_hash":       common.HexToHash("0x1234").Hex(),
		"error_class":   "rpc_transient",
		"error_code":    "rpc_unavailable",
		"retryable":     true,
		"ambiguous":     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %#v, want %#v", got, want)
	}
}

func TestLoggerLegacyPathRedactsSyntheticCanaries(t *testing.T) {
	t.Parallel()

	canaries := []string{
		"wss://user:private-canary@example.invalid/rpc?token=private-canary",
		"signature-private-canary",
		"signed-transaction-private-canary",
		"token-metadata-private-canary",
		"987654321-private-amount-canary",
	}
	var output bytes.Buffer
	logger := NewLogger(&output)
	logger.Record(Event{
		Level:       LevelError,
		Code:        "safe_event",
		NetworkName: canaries[0],
		Candidate:   domain.CandidateID{1},
		TokenSymbol: canaries[3],
		Amount:      canaries[4],
		TxHash:      common.HexToHash("0x9876"),
		ErrorCode:   domain.ErrorCode(canaries[1]),
	})
	logger.Record(Event{Level: LevelInfo, Code: EventCode(canaries[3])})

	line := output.String()
	for _, record := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if !json.Valid(record) {
			t.Fatalf("legacy record не является JSON: %q", line)
		}
	}
	for _, canary := range canaries {
		if strings.Contains(line, canary) {
			t.Fatalf("журнал раскрыл synthetic canary %q: %q", canary, line)
		}
	}
	if strings.Contains(line, common.HexToHash("0x9876").Hex()) {
		t.Fatalf("legacy tx hash попал в журнал до typed broadcast boundary: %q", line)
	}
	for _, expected := range []string{"safe_event", "event_redacted", "error_redacted"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("журнал потерял безопасную классификацию %q: %q", expected, line)
		}
	}
}

func TestLoggerRejectsInvalidTypedValuesWithoutOutput(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := NewLogger(&output)
	err := logger.Write(SafeEvent{Level: LevelInfo, Code: LogCode(255), ChainID: 1})
	if !errors.Is(err, ErrInvalidLogEvent) {
		t.Fatalf("error = %v, want ErrInvalidLogEvent", err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid event создал output: %q", output.String())
	}
	if _, err := NewSafeError(ErrorClass(255), ErrorInternalFailure, false, false); !errors.Is(err, ErrInvalidLogEvent) {
		t.Fatalf("NewSafeError error = %v, want ErrInvalidLogEvent", err)
	}
	if _, err := NewPublicTxHashAfterBroadcast(common.Hash{}); !errors.Is(err, ErrInvalidTxHash) {
		t.Fatalf("NewPublicTxHashAfterBroadcast error = %v, want ErrInvalidTxHash", err)
	}
}

func TestLoggerAcceptsStructuredAlertState(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output)
	if err := logger.Write(SafeEvent{
		Level: LevelWarning, Code: LogAlertState, ChainID: 1,
		Result: ResultAccepted, Alert: AlertBudgetBlocked,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"alert_state"`) || !strings.Contains(output.String(), `"alert_code":"budget_blocked"`) {
		t.Fatalf("alert record = %q", output.String())
	}
}

func TestLoggerConcurrentWritesRemainNDJSONRecords(t *testing.T) {
	t.Parallel()

	const writers = 64
	var output bytes.Buffer
	logger := NewLogger(&output)
	errorsChannel := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			incident := domain.IncidentID{byte(index + 1)}
			errorsChannel <- logger.Write(SafeEvent{
				Level:    LevelInfo,
				Code:     LogCandidateQueued,
				ChainID:  domain.NetworkID(index%2 + 1),
				Incident: incident,
				State:    DurablePending,
				Result:   ResultAccepted,
			})
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	lines := 0
	for scanner.Scan() {
		lines++
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("строка %d повреждена concurrent write: %v", lines, err)
		}
		if record["code"] != "candidate_queued" {
			t.Fatalf("строка %d имеет code %#v", lines, record["code"])
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != writers {
		t.Fatalf("lines = %d, want %d", lines, writers)
	}
}
