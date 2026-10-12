package rescue

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

func TestAdmissionControllerIsIdempotentAndRejectsIdentityConflict(t *testing.T) {
	clock := &admissionTestClock{now: time.Unix(100, 0)}
	controller := newTestAdmissionController(t, clock, AdmissionConfig{
		Window: time.Minute, RateLimit: 10, MaxAttemptsPerToken: 10,
		MaxAttemptsPerSourceEvent: 10, MaxNewUnknownTokens: 10, Capacity: 10,
	})
	request := testAdmissionRequest(1, 1, 1)
	first, err := controller.Admit(request)
	if err != nil || !first.Allowed {
		t.Fatalf("first Admit() returned %#v, %v", first, err)
	}
	replay, err := controller.Admit(request)
	if err != nil || replay != first || controller.TrackedAttempts() != 1 {
		t.Fatalf("duplicate Admit() returned %#v, %v, tracked=%d", replay, err, controller.TrackedAttempts())
	}

	request.Token = common.HexToAddress("0xff")
	decision, err := controller.Admit(request)
	assertAdmissionError(t, decision, err, AdmissionAttemptIdentityConflict, time.Time{}, false)
	if !errors.Is(err, ErrInvalidAdmissionRequest) {
		t.Fatalf("identifier conflict error = %v, want ErrInvalidAdmissionRequest", err)
	}
}

func TestAdmissionControllerRollingLimits(t *testing.T) {
	start := time.Unix(200, 0)
	tests := []struct {
		name       string
		config     AdmissionConfig
		first      AdmissionRequest
		second     AdmissionRequest
		wantReason AdmissionReason
	}{
		{
			name: "global rate", config: testAdmissionConfig(),
			first: testAdmissionRequest(1, 1, 1), second: testAdmissionRequest(2, 2, 2),
			wantReason: AdmissionGlobalRateLimited,
		},
		{
			name: "per token", config: testAdmissionConfig(),
			first: testAdmissionRequest(1, 1, 1), second: testAdmissionRequest(2, 1, 2),
			wantReason: AdmissionTokenAttemptsLimited,
		},
		{
			name: "per source event", config: testAdmissionConfig(),
			first: testAdmissionRequest(1, 1, 1), second: testAdmissionRequest(2, 2, 1),
			wantReason: AdmissionSourceEventLimited,
		},
		{
			name: "capacity", config: testAdmissionConfig(),
			first: testAdmissionRequest(1, 1, 1), second: testAdmissionRequest(2, 2, 2),
			wantReason: AdmissionCapacityLimited,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := test.config
			switch test.wantReason {
			case AdmissionGlobalRateLimited:
				config.RateLimit = 1
			case AdmissionTokenAttemptsLimited:
				config.MaxAttemptsPerToken = 1
			case AdmissionSourceEventLimited:
				config.MaxAttemptsPerSourceEvent = 1
			case AdmissionCapacityLimited:
				config.Capacity = 1
			}
			clock := &admissionTestClock{now: start}
			controller := newTestAdmissionController(t, clock, config)
			if _, err := controller.Admit(test.first); err != nil {
				t.Fatalf("first Admit() returned an error: %v", err)
			}
			decision, err := controller.Admit(test.second)
			assertAdmissionError(t, decision, err, test.wantReason, start.Add(time.Minute), true)

			clock.advance(time.Minute)
			decision, err = controller.Admit(test.second)
			if err != nil || !decision.Allowed || controller.TrackedAttempts() != 1 {
				t.Fatalf("Admit() after window returned %#v, %v, tracked=%d", decision, err, controller.TrackedAttempts())
			}
		})
	}
}

func TestAdmissionControllerLimitsDistinctUnknownTokens(t *testing.T) {
	start := time.Unix(300, 0)
	clock := &admissionTestClock{now: start}
	config := testAdmissionConfig()
	config.MaxNewUnknownTokens = 1
	controller := newTestAdmissionController(t, clock, config)

	first := testAdmissionRequest(1, 1, 1)
	first.Unknown = true
	if _, err := controller.Admit(first); err != nil {
		t.Fatalf("first Admit() for unknown token returned an error: %v", err)
	}
	clock.advance(10 * time.Second)
	sameToken := testAdmissionRequest(2, 1, 2)
	sameToken.Unknown = true
	if _, err := controller.Admit(sameToken); err != nil {
		t.Fatalf("Admit() for the same unknown token returned an error: %v", err)
	}
	newToken := testAdmissionRequest(3, 2, 3)
	newToken.Network = 2
	newToken.Unknown = true
	decision, err := controller.Admit(newToken)
	assertAdmissionError(t, decision, err, AdmissionUnknownTokenRateLimited, start.Add(70*time.Second), true)

	clock.advance(time.Minute)
	decision, err = controller.Admit(newToken)
	if err != nil || !decision.Allowed {
		t.Fatalf("new unknown token after full token window = %#v, %v", decision, err)
	}
}

func TestAdmissionControllerBoundsThousandsOfUnknownTokensWithoutGoroutines(t *testing.T) {
	clock := &admissionTestClock{now: time.Unix(400, 0)}
	controller := newTestAdmissionController(t, clock, AdmissionConfig{
		Window: time.Hour, RateLimit: 10_000, MaxAttemptsPerToken: 10,
		MaxAttemptsPerSourceEvent: 10, MaxNewUnknownTokens: 10_000, Capacity: 64,
	})
	before := runtime.NumGoroutine()
	for index := 1; index <= 5_000; index++ {
		request := testAdmissionRequest(index, index, index)
		request.Unknown = true
		decision, err := controller.Admit(request)
		if index <= 64 && (err != nil || !decision.Allowed) {
			t.Fatalf("Admit(%d) returned %#v, %v", index, decision, err)
		}
		if index > 64 {
			assertAdmissionError(t, decision, err, AdmissionCapacityLimited, time.Unix(400, 0).Add(time.Hour), true)
		}
	}
	runtime.Gosched()
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("goroutines after load = %d, want %d", after, before)
	}
	if tracked := controller.TrackedAttempts(); tracked != 64 {
		t.Fatalf("TrackedAttempts() returned %d, want capacity 64", tracked)
	}
	if len(controller.attempts) != 64 || len(controller.tokens) != 64 || len(controller.events) != 64 || len(controller.unknown) != 64 || len(controller.latest) != 64 {
		t.Fatalf("bounded indexes: attempts=%d, tokens=%d, events=%d, unknown=%d, latest=%d", len(controller.attempts), len(controller.tokens), len(controller.events), len(controller.unknown), len(controller.latest))
	}

	clock.advance(time.Hour)
	if tracked := controller.TrackedAttempts(); tracked != 0 {
		t.Fatalf("TrackedAttempts() after expiration returned %d, want 0", tracked)
	}
	if len(controller.attempts) != 0 || len(controller.tokens) != 0 || len(controller.events) != 0 || len(controller.unknown) != 0 || len(controller.latest) != 0 {
		t.Fatalf("indexes retained expired state: attempts=%d, tokens=%d, events=%d, unknown=%d, latest=%d", len(controller.attempts), len(controller.tokens), len(controller.events), len(controller.unknown), len(controller.latest))
	}
}

func TestNewAdmissionControllerRejectsInvalidConfig(t *testing.T) {
	_, err := NewAdmissionController(AdmissionConfig{}, &admissionTestClock{})
	if !errors.Is(err, ErrInvalidAdmissionConfig) {
		t.Fatalf("NewAdmissionController() returned error %v, want ErrInvalidAdmissionConfig", err)
	}
}

func TestPersistentAdmissionIsGlobalAcrossNetworksAndRestart(t *testing.T) {
	clock := &admissionTestClock{now: time.Unix(500, 0)}
	config := testAdmissionConfig()
	config.RateLimit = 1
	path := filepath.Join(t.TempDir(), "admission.db")
	controller, err := OpenAdmissionController(path, config, clock)
	if err != nil {
		t.Fatal(err)
	}
	first := testAdmissionRequest(1, 1, 1)
	if _, err := controller.Admit(first); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAdmissionController(path, config, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	second := testAdmissionRequest(2, 2, 2)
	second.Network = 2
	decision, err := reopened.Admit(second)
	assertAdmissionError(t, decision, err, AdmissionGlobalRateLimited, clock.now.Add(time.Minute), true)
	clock.advance(time.Minute)
	if decision, err := reopened.Admit(second); err != nil || !decision.Allowed {
		t.Fatalf("Admit after persistent window returned %#v, %v", decision, err)
	}
}

func TestPersistentAdmissionRejectsExistingMissingState(t *testing.T) {
	clock := &admissionTestClock{now: time.Unix(500, 0)}
	config := testAdmissionConfig()
	t.Run("zero length", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admission.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenAdmissionController(path, config, clock); !errors.Is(err, ErrAdmissionPersistence) {
			t.Fatalf("OpenAdmissionController() returned an error: %v", err)
		}
	})
	t.Run("missing buckets", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admission.db")
		db, err := bolt.Open(path, 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte("unrelated")); return err }); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenAdmissionController(path, config, clock); !errors.Is(err, ErrAdmissionPersistence) {
			t.Fatalf("OpenAdmissionController() returned an error: %v", err)
		}
	})
}

func testAdmissionConfig() AdmissionConfig {
	return AdmissionConfig{
		Window: time.Minute, RateLimit: 10, MaxAttemptsPerToken: 10,
		MaxAttemptsPerSourceEvent: 10, MaxNewUnknownTokens: 10, Capacity: 10,
	}
}

func testAdmissionRequest(identity, token, sourceEvent int) AdmissionRequest {
	var incident, parent domain.IncidentID
	var event domain.CandidateID
	incident[30] = byte(identity >> 8)
	incident[31] = byte(identity)
	parent[29] = 1
	parent[30] = byte(identity >> 8)
	parent[31] = byte(identity)
	event[30] = byte(sourceEvent >> 8)
	event[31] = byte(sourceEvent)
	return AdmissionRequest{
		Network: 1, Incident: incident, Attempt: 1, Token: common.BigToAddress(bigInt(token)),
		Parent: parent, SourceEvent: event,
	}
}

func bigInt(value int) *big.Int {
	return big.NewInt(int64(value))
}

func newTestAdmissionController(t *testing.T, clock AdmissionClock, config AdmissionConfig) *AdmissionController {
	t.Helper()
	controller, err := NewAdmissionController(config, clock)
	if err != nil {
		t.Fatalf("NewAdmissionController() returned an error: %v", err)
	}
	return controller
}

func assertAdmissionError(t *testing.T, decision AdmissionDecision, err error, reason AdmissionReason, retryAt time.Time, retryable bool) {
	t.Helper()
	var admissionErr *AdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("Admit() returned error %v, want *AdmissionError", err)
	}
	if decision.Allowed || decision.Reason != reason || !decision.RetryAt.Equal(retryAt) || admissionErr.Reason != reason ||
		!admissionErr.RetryAt.Equal(retryAt) || admissionErr.Retryable != retryable {
		t.Fatalf("Admit() returned %#v, %#v, want reason=%s, retry at=%s, retryable=%t", decision, admissionErr, reason, retryAt, retryable)
	}
}

type admissionTestClock struct {
	now time.Time
}

func (clock *admissionTestClock) Now() time.Time {
	return clock.now
}

func (clock *admissionTestClock) advance(duration time.Duration) {
	clock.now = clock.now.Add(duration)
}
