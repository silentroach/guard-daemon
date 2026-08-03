package main

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
)

func TestLegacyMemoryHandoffReplayDuplicateAndAckOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	network := domain.NetworkID(31337)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	first := handoffCandidate(network, 1)
	second := handoffCandidate(network, 2)

	if result, err := handoff.Put(ctx, first); err != nil || result != store.PutInserted {
		t.Fatalf("first Put() = (%v, %v)", result, err)
	}
	duplicate := first
	duplicate.Token.Symbol = "changed-metadata"
	if result, err := handoff.Put(ctx, duplicate); err != nil || result != store.PutAlreadyPending {
		t.Fatalf("duplicate pending Put() = (%v, %v)", result, err)
	}
	if _, err := handoff.Put(ctx, second); err != nil {
		t.Fatalf("second Put() error = %v", err)
	}

	wantReplay := []domain.RescueCandidate{first, second}
	for attempt := 0; attempt < 2; attempt++ {
		replay, err := handoff.Replay(ctx, network)
		if err != nil || !reflect.DeepEqual(replay, wantReplay) {
			t.Fatalf("Replay() = (%v, %v), want stable insertion order", replay, err)
		}
	}
	next, err := handoff.Next(ctx, network)
	if err != nil || next != first {
		t.Fatalf("Next() = (%v, %v), want first candidate", next, err)
	}

	incidentID := domain.NewIncidentID(first.ID)
	if err := handoff.Ack(ctx, first.ID, incidentID); !errors.Is(err, errAckOrder) {
		t.Fatalf("Ack() before PutIncident error = %v", err)
	}
	incident := store.Incident{ID: incidentID, Candidate: first.ID, Network: network, CreatedAt: time.Unix(1, 0)}
	if err := handoff.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident() error = %v", err)
	}
	incident.CreatedAt = time.Unix(2, 0)
	if err := handoff.PutIncident(ctx, incident); err != nil {
		t.Fatalf("idempotent PutIncident() error = %v", err)
	}
	if err := handoff.Ack(ctx, first.ID, incidentID); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if err := handoff.Ack(ctx, first.ID, incidentID); err != nil {
		t.Fatalf("idempotent Ack() error = %v", err)
	}
	if result, err := handoff.Put(ctx, first); err != nil || result != store.PutAlreadyAcknowledged {
		t.Fatalf("acknowledged duplicate Put() = (%v, %v)", result, err)
	}

	replay, err := handoff.Replay(ctx, network)
	if err != nil || !reflect.DeepEqual(replay, []domain.RescueCandidate{second}) {
		t.Fatalf("Replay() after Ack = (%v, %v)", replay, err)
	}
}

func TestLegacyMemoryHandoffCopiesSignedTransactionsAndRaisesNonceFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	network := domain.NetworkID(31349)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	id := domain.IncidentID{31: 1}
	createdAt := time.Unix(1, 0)
	incident := store.RescueIncident{
		ID: id, Network: network, Kind: domain.CandidateNative, Status: store.RescuePending,
		Policy: store.RescuePolicySnapshot{MaxAttempts: 3}, SignedTransaction: []byte{1, 2, 3}, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	stored, err := handoff.PutRescueIncident(ctx, incident)
	if err != nil {
		t.Fatal(err)
	}
	incident.SignedTransaction[0] = 9
	stored.SignedTransaction[1] = 9
	got, found, err := handoff.RescueIncident(ctx, id)
	if err != nil || !found || !reflect.DeepEqual(got.SignedTransaction, []byte{1, 2, 3}) {
		t.Fatalf("RescueIncident() = (%v, %t, %v)", got.SignedTransaction, found, err)
	}

	prepared := got
	prepared.Status = store.RescuePrepared
	prepared.Attempts = 1
	prepared.UpdatedAt = createdAt.Add(time.Second)
	prepared.SignedTransaction = []byte{4, 5, 6}
	if err := handoff.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	prepared.SignedTransaction[0] = 9
	incidents, err := handoff.RescueIncidents(ctx, network)
	if err != nil || len(incidents) != 1 || !reflect.DeepEqual(incidents[0].SignedTransaction, []byte{4, 5, 6}) {
		t.Fatalf("RescueIncidents() = (%v, %v)", incidents, err)
	}
	incidents[0].SignedTransaction[1] = 9
	got, _, _ = handoff.RescueIncident(ctx, id)
	if !reflect.DeepEqual(got.SignedTransaction, []byte{4, 5, 6}) {
		t.Fatalf("stored signed transaction was aliased: %v", got.SignedTransaction)
	}

	if err := handoff.RaiseNonceFloor(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := handoff.RaiseNonceFloor(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if floor, err := handoff.NonceFloor(ctx); err != nil || floor != 7 {
		t.Fatalf("NonceFloor() = (%d, %v)", floor, err)
	}
}

func TestLegacyMemoryHandoffNextWakesAndCancels(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31338)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	waitContext, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	result := make(chan domain.RescueCandidate, 1)
	errorsChannel := make(chan error, 1)
	go func() {
		candidate, err := handoff.Next(waitContext, network)
		result <- candidate
		errorsChannel <- err
	}()

	candidate := handoffCandidate(network, 3)
	if _, err := handoff.Put(context.Background(), candidate); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	select {
	case got := <-result:
		if err := <-errorsChannel; err != nil || got != candidate {
			t.Fatalf("woken Next() = (%v, %v)", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Next() was not woken by Put()")
	}

	incidentID := domain.NewIncidentID(candidate.ID)
	if err := handoff.PutIncident(context.Background(), store.Incident{ID: incidentID, Candidate: candidate.ID, Network: network}); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Ack(context.Background(), candidate.ID, incidentID); err != nil {
		t.Fatal(err)
	}
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handoff.Next(canceledContext, network); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Next() error = %v", err)
	}
}

func TestLegacyMemoryHandoffBoundsPendingAndTombstones(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31342)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	ctx := context.Background()
	for index := uint64(1); index <= legacyTombstoneLimit+32; index++ {
		candidate := boundedHandoffCandidate(network, index)
		if _, err := handoff.Put(ctx, candidate); err != nil {
			t.Fatal(err)
		}
		incidentID := domain.NewIncidentID(candidate.ID)
		if err := handoff.PutIncident(ctx, store.Incident{ID: incidentID, Candidate: candidate.ID, Network: network}); err != nil {
			t.Fatal(err)
		}
		if err := handoff.Ack(ctx, candidate.ID, incidentID); err != nil {
			t.Fatal(err)
		}
	}

	if len(handoff.pending) != 0 || len(handoff.tombstones) != legacyTombstoneLimit {
		t.Fatalf("handoff sizes = pending:%d tombstones:%d", len(handoff.pending), len(handoff.tombstones))
	}
	if len(handoff.entries) > legacyTombstoneLimit || len(handoff.incidents) > legacyTombstoneLimit {
		t.Fatalf("retained state is unbounded: entries:%d incidents:%d", len(handoff.entries), len(handoff.incidents))
	}
}

func TestLegacyMemoryHandoffAppliesBackpressureAtPendingLimit(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31343)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for index := uint64(1); index <= legacyMaxPending; index++ {
		if _, err := handoff.Put(ctx, boundedHandoffCandidate(network, index)); err != nil {
			t.Fatal(err)
		}
	}

	extra := boundedHandoffCandidate(network, legacyMaxPending+1)
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := handoff.Put(ctx, extra)
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		t.Fatalf("Put() bypassed backpressure: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	first := boundedHandoffCandidate(network, 1)
	incidentID := domain.NewIncidentID(first.ID)
	if err := handoff.PutIncident(ctx, store.Incident{ID: incidentID, Candidate: first.ID, Network: network}); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Ack(ctx, first.ID, incidentID); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("blocked Put() error = %v", err)
	}
}

func TestConsumeCandidatesPersistsHandlesAndAcknowledgesInOrder(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31339)
	candidate := handoffCandidate(network, 4)
	recorder := &transitionRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	handoff := &orderedHandoff{candidate: candidate, recorder: recorder, afterAck: cancel}
	session := &recordingHandler{generation: 2, recorder: recorder}

	err := consumeCandidates(ctx, network, handoff, handoff, session, clock.Real{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeCandidates() error = %v", err)
	}
	want := []string{"Next", "PutIncident", "Handle", "Ack"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	if handoff.incident.ID != domain.NewIncidentID(candidate.ID) {
		t.Fatal("consumer did not derive the stable incident ID")
	}
}

func TestConsumeCandidatesDoesNotAckAfterHandlingCancellation(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31340)
	recorder := &transitionRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	handoff := &orderedHandoff{candidate: handoffCandidate(network, 5), recorder: recorder}
	session := &recordingHandler{generation: 1, recorder: recorder, handle: func(context.Context, domain.RescueCandidate) error {
		cancel()
		return nil
	}}

	err := consumeCandidates(ctx, network, handoff, handoff, session, clock.Real{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeCandidates() error = %v", err)
	}
	want := []string{"Next", "PutIncident", "Handle"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
}

func TestConsumeCandidatesAcknowledgesTerminalHandlingError(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31345)
	recorder := &transitionRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	handoff := &orderedHandoff{candidate: handoffCandidate(network, 9), recorder: recorder, afterAck: cancel}
	session := &recordingHandler{
		generation: 1,
		recorder:   recorder,
		handle: func(context.Context, domain.RescueCandidate) error {
			return domain.NewError("test.terminal", domain.ErrorPostcondition, "test_terminal", false, false, nil)
		},
	}

	err := consumeCandidates(ctx, network, handoff, handoff, session, clock.Real{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeCandidates() error = %v", err)
	}
	want := []string{"Next", "PutIncident", "Handle", "Ack"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal transitions = %v, want %v", got, want)
	}
}

func TestConsumeCandidatesReplaysRetryableAndAmbiguousFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		retryable bool
		ambiguous bool
	}{
		{name: "retryable", retryable: true},
		{name: "ambiguous", ambiguous: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			network := domain.NetworkID(31341)
			candidate := handoffCandidate(network, 6)
			handoff := newLegacyMemoryHandoff(network, clock.Real{})
			if _, err := handoff.Put(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			attempted := make(chan struct{})
			var attemptedOnce sync.Once
			handler := &recordingHandler{
				generation: 1,
				recorder:   &transitionRecorder{},
				handle: func(context.Context, domain.RescueCandidate) error {
					attemptedOnce.Do(func() { close(attempted) })
					return domain.NewError("test.handle", domain.ErrorRPCTransient, "test_failure", test.retryable, test.ambiguous, nil)
				},
			}

			done := make(chan error, 1)
			go func() {
				done <- consumeCandidates(ctx, network, handoff, handoff, handler, clock.Real{})
			}()
			<-attempted
			waitForDelayed(t, handoff, 1)
			replay, replayError := handoff.Replay(context.Background(), network)
			if replayError != nil || !reflect.DeepEqual(replay, []domain.RescueCandidate{candidate}) {
				t.Fatalf("Replay() = (%v, %v), want failed candidate", replay, replayError)
			}
			if _, found, incidentError := handoff.IncidentByCandidate(context.Background(), candidate.ID); incidentError != nil || !found {
				t.Fatalf("incident persistence = (found %v, error %v)", found, incidentError)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("consumeCandidates() error = %v", err)
			}
		})
	}
}

func TestRetryableCandidateDoesNotStarveFollowingWork(t *testing.T) {
	t.Parallel()

	network := domain.NetworkID(31344)
	first := handoffCandidate(network, 7)
	second := handoffCandidate(network, 8)
	handoff := newLegacyMemoryHandoff(network, clock.Real{})
	if _, err := handoff.Put(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstAttempted := make(chan struct{})
	secondHandled := make(chan struct{})
	var firstOnce sync.Once
	var secondOnce sync.Once
	handler := &recordingHandler{
		generation: 1,
		recorder:   &transitionRecorder{},
		handle: func(ctx context.Context, candidate domain.RescueCandidate) error {
			if candidate.ID == first.ID {
				firstOnce.Do(func() { close(firstAttempted) })
				return domain.NewError("test.handle", domain.ErrorRPCTransient, "test_failure", true, true, nil)
			}
			secondOnce.Do(func() { close(secondHandled) })
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- consumeCandidates(ctx, network, handoff, handoff, handler, clock.Real{})
	}()
	<-firstAttempted
	waitForDelayed(t, handoff, 1)
	if _, err := handoff.Put(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	<-secondHandled
	deadline := time.After(time.Second)
	for {
		replay, err := handoff.Replay(context.Background(), network)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(replay, []domain.RescueCandidate{first}) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("second candidate was not acknowledged: replay=%v", replay)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("second consumeCandidates() error = %v", err)
	}
}

func waitForDelayed(t *testing.T, handoff *legacyMemoryHandoff, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		handoff.mu.Lock()
		got := len(handoff.delayed)
		handoff.mu.Unlock()
		if got == count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("delayed candidate count = %d, want %d", got, count)
		case <-time.After(time.Millisecond):
		}
	}
}

type transitionRecorder struct {
	mu          sync.Mutex
	transitions []string
}

func (recorder *transitionRecorder) add(transition string) {
	recorder.mu.Lock()
	recorder.transitions = append(recorder.transitions, transition)
	recorder.mu.Unlock()
}

func (recorder *transitionRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.transitions...)
}

type orderedHandoff struct {
	candidate domain.RescueCandidate
	recorder  *transitionRecorder
	afterAck  context.CancelFunc
	delivered bool
	incident  store.Incident
}

func (*orderedHandoff) Put(context.Context, domain.RescueCandidate) (store.PutResult, error) {
	return 0, errors.New("unexpected Put")
}

func (handoff *orderedHandoff) Next(ctx context.Context, _ domain.NetworkID) (domain.RescueCandidate, error) {
	if !handoff.delivered {
		handoff.delivered = true
		handoff.recorder.add("Next")
		return handoff.candidate, nil
	}
	<-ctx.Done()
	return domain.RescueCandidate{}, ctx.Err()
}

func (*orderedHandoff) Replay(context.Context, domain.NetworkID) ([]domain.RescueCandidate, error) {
	return nil, errors.New("unexpected Replay")
}

func (handoff *orderedHandoff) Ack(context.Context, domain.CandidateID, domain.IncidentID) error {
	handoff.recorder.add("Ack")
	if handoff.afterAck != nil {
		handoff.afterAck()
	}
	return nil
}

func (*orderedHandoff) Nack(context.Context, domain.CandidateID, domain.IncidentID, time.Time) error {
	return errors.New("unexpected Nack")
}

func (handoff *orderedHandoff) PutIncident(_ context.Context, incident store.Incident) error {
	handoff.recorder.add("PutIncident")
	handoff.incident = incident
	return nil
}

func (*orderedHandoff) IncidentByCandidate(context.Context, domain.CandidateID) (store.Incident, bool, error) {
	return store.Incident{}, false, nil
}

type recordingHandler struct {
	generation uint64
	recorder   *transitionRecorder
	handle     func(context.Context, domain.RescueCandidate) error
}

func (handler *recordingHandler) Generation() uint64 { return handler.generation }

func (handler *recordingHandler) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	handler.recorder.add("Handle")
	if handler.handle != nil {
		return handler.handle(ctx, candidate)
	}
	return nil
}

func handoffCandidate(network domain.NetworkID, value byte) domain.RescueCandidate {
	var source common.Address
	source[len(source)-1] = 1
	var blockHash common.Hash
	blockHash[len(blockHash)-1] = value
	return domain.NewBlockCandidate(network, domain.CandidateNative, source, blockHash, uint64(value))
}

func boundedHandoffCandidate(network domain.NetworkID, value uint64) domain.RescueCandidate {
	source := common.Address{19: 1}
	return domain.NewBlockCandidate(network, domain.CandidateNative, source, common.BigToHash(new(big.Int).SetUint64(value)), value)
}
