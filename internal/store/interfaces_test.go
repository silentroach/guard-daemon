package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

type durableState struct {
	candidates   map[domain.CandidateID]domain.RescueCandidate
	acknowledged map[domain.CandidateID]domain.IncidentID
	incidents    map[domain.CandidateID]Incident
	checkpoints  map[domain.NetworkID]Checkpoint
	transitions  []string
}

type durableHandoff struct {
	state *durableState
}

func newDurableState() *durableState {
	return &durableState{
		candidates:   make(map[domain.CandidateID]domain.RescueCandidate),
		acknowledged: make(map[domain.CandidateID]domain.IncidentID),
		incidents:    make(map[domain.CandidateID]Incident),
		checkpoints:  make(map[domain.NetworkID]Checkpoint),
	}
}

func (state *durableState) reopen() *durableState {
	reopened := newDurableState()
	for id, candidate := range state.candidates {
		reopened.candidates[id] = candidate
	}
	for id, incident := range state.acknowledged {
		reopened.acknowledged[id] = incident
	}
	for id, incident := range state.incidents {
		reopened.incidents[id] = incident
	}
	for network, checkpoint := range state.checkpoints {
		reopened.checkpoints[network] = checkpoint
	}
	reopened.transitions = append([]string(nil), state.transitions...)
	return reopened
}

func (h *durableHandoff) Put(_ context.Context, candidate domain.RescueCandidate) (PutResult, error) {
	if _, ok := h.state.acknowledged[candidate.ID]; ok {
		return PutAlreadyAcknowledged, nil
	}
	if _, ok := h.state.candidates[candidate.ID]; ok {
		return PutAlreadyPending, nil
	}
	h.state.candidates[candidate.ID] = candidate
	h.state.transitions = append(h.state.transitions, "Put")
	return PutInserted, nil
}

func (h *durableHandoff) Next(ctx context.Context, network domain.NetworkID) (domain.RescueCandidate, error) {
	candidates, err := h.Replay(ctx, network)
	if err != nil {
		return domain.RescueCandidate{}, err
	}
	if len(candidates) == 0 {
		return domain.RescueCandidate{}, errors.New("no pending candidate")
	}
	return candidates[0], nil
}

func (h *durableHandoff) Replay(_ context.Context, network domain.NetworkID) ([]domain.RescueCandidate, error) {
	var replay []domain.RescueCandidate
	for id, candidate := range h.state.candidates {
		if candidate.Network == network {
			if _, acknowledged := h.state.acknowledged[id]; !acknowledged {
				replay = append(replay, candidate)
			}
		}
	}
	return replay, nil
}

func (h *durableHandoff) Ack(_ context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID) error {
	if acknowledgedIncident, ok := h.state.acknowledged[candidateID]; ok {
		if acknowledgedIncident != incidentID {
			return errors.New("candidate acknowledged by another incident")
		}
		return nil
	}
	incident, ok := h.state.incidents[candidateID]
	if !ok || incident.ID != incidentID {
		return errors.New("incident must be persisted before acknowledgement")
	}
	h.state.acknowledged[candidateID] = incidentID
	h.state.transitions = append(h.state.transitions, "Ack")
	return nil
}

func (h *durableHandoff) Nack(_ context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID, _ time.Time) error {
	incident, ok := h.state.incidents[candidateID]
	if !ok || incident.ID != incidentID {
		return errors.New("incident must be persisted before negative acknowledgement")
	}
	if _, acknowledged := h.state.acknowledged[candidateID]; acknowledged {
		return errors.New("acknowledged candidate cannot be retried")
	}
	return nil
}

func (h *durableHandoff) PutIncident(_ context.Context, incident Incident) error {
	if persisted, ok := h.state.incidents[incident.Candidate]; ok {
		if persisted.ID != incident.ID || persisted.Candidate != incident.Candidate || persisted.Network != incident.Network {
			return errors.New("candidate is linked to another incident")
		}
		return nil
	}
	if _, ok := h.state.candidates[incident.Candidate]; !ok {
		return errors.New("candidate must be persisted before incident")
	}
	h.state.incidents[incident.Candidate] = incident
	h.state.transitions = append(h.state.transitions, "PutIncident")
	return nil
}

func (h *durableHandoff) IncidentByCandidate(_ context.Context, candidateID domain.CandidateID) (Incident, bool, error) {
	incident, ok := h.state.incidents[candidateID]
	return incident, ok, nil
}

func (h *durableHandoff) LoadCheckpoint(_ context.Context, network domain.NetworkID) (Checkpoint, bool, error) {
	checkpoint, ok := h.state.checkpoints[network]
	return checkpoint, ok, nil
}

func (h *durableHandoff) AdvanceCheckpoint(_ context.Context, checkpoint Checkpoint) error {
	if persisted, ok := h.state.checkpoints[checkpoint.Network]; ok {
		if persisted != checkpoint {
			return errors.New("checkpoint already advanced to another block")
		}
		return nil
	}
	for id, candidate := range h.state.candidates {
		incident, incidentPersisted := h.state.incidents[id]
		incidentID, acknowledged := h.state.acknowledged[id]
		if candidate.Network == checkpoint.Network &&
			candidate.BlockNumber == checkpoint.BlockNumber &&
			candidate.BlockHash == checkpoint.BlockHash &&
			incidentPersisted && acknowledged && incident.ID == incidentID {
			h.state.checkpoints[checkpoint.Network] = checkpoint
			h.state.transitions = append(h.state.transitions, "AdvanceCheckpoint")
			return nil
		}
	}
	return errors.New("candidate must be acknowledged before checkpoint")
}

var (
	_ CandidateQueue  = (*durableHandoff)(nil)
	_ IncidentStore   = (*durableHandoff)(nil)
	_ CheckpointStore = (*durableHandoff)(nil)
)

func TestHandoffReplayAndDuplicateCoalescingAcrossCrashes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		persistedSteps   int
		wantReplay       bool
		wantDuplicatePut PutResult
	}{
		{name: "after candidate Put", persistedSteps: 1, wantReplay: true, wantDuplicatePut: PutAlreadyPending},
		{name: "after incident persistence", persistedSteps: 2, wantReplay: true, wantDuplicatePut: PutAlreadyPending},
		{name: "after Ack before checkpoint", persistedSteps: 3, wantDuplicatePut: PutAlreadyAcknowledged},
		{name: "after checkpoint advancement", persistedSteps: 4, wantDuplicatePut: PutAlreadyAcknowledged},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			candidate, incident, checkpoint := handoffFixture()
			state := newDurableState()
			beforeCrash := &durableHandoff{state: state}
			persistHandoffThrough(t, ctx, beforeCrash, beforeCrash, beforeCrash, candidate, incident, checkpoint, testCase.persistedSteps)

			state = state.reopen()
			afterCrash := &durableHandoff{state: state}
			var queue CandidateQueue = afterCrash
			var incidents IncidentStore = afterCrash
			var checkpoints CheckpointStore = afterCrash
			firstReplay, err := queue.Replay(ctx, candidate.Network)
			if err != nil {
				t.Fatalf("first Replay: %v", err)
			}
			secondReplay, err := queue.Replay(ctx, candidate.Network)
			if err != nil {
				t.Fatalf("second Replay: %v", err)
			}
			if !reflect.DeepEqual(firstReplay, secondReplay) {
				t.Fatalf("Replay changed without a durable transition: first %v, second %v", firstReplay, secondReplay)
			}
			if testCase.wantReplay {
				if len(firstReplay) != 1 || firstReplay[0] != candidate {
					t.Fatalf("Replay = %v, want the persisted candidate", firstReplay)
				}
			} else if len(firstReplay) != 0 {
				t.Fatalf("Replay returned acknowledged candidates: %v", firstReplay)
			}

			duplicate := handoffCandidate()
			if duplicate.ID != candidate.ID {
				t.Fatalf("stable candidate ID changed across restart: got %s, want %s", duplicate.ID, candidate.ID)
			}
			putResult, err := queue.Put(ctx, duplicate)
			if err != nil {
				t.Fatalf("duplicate Put: %v", err)
			}
			if putResult != testCase.wantDuplicatePut {
				t.Fatalf("duplicate Put = %v, want %v", putResult, testCase.wantDuplicatePut)
			}

			if err := incidents.PutIncident(ctx, incident); err != nil {
				t.Fatalf("PutIncident after restart: %v", err)
			}
			if err := incidents.PutIncident(ctx, incident); err != nil {
				t.Fatalf("duplicate PutIncident: %v", err)
			}
			if err := queue.Ack(ctx, candidate.ID, incident.ID); err != nil {
				t.Fatalf("Ack after restart: %v", err)
			}
			if err := queue.Ack(ctx, candidate.ID, incident.ID); err != nil {
				t.Fatalf("duplicate Ack: %v", err)
			}
			if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err != nil {
				t.Fatalf("AdvanceCheckpoint after restart: %v", err)
			}
			if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err != nil {
				t.Fatalf("duplicate AdvanceCheckpoint: %v", err)
			}

			assertCompletedHandoff(t, ctx, queue, incidents, checkpoints, state, candidate, incident, checkpoint)
		})
	}
}

func TestHandoffRequiresDurableTransitionOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	candidate, incident, checkpoint := handoffFixture()
	handoff := &durableHandoff{state: newDurableState()}
	var queue CandidateQueue = handoff
	var incidents IncidentStore = handoff
	var checkpoints CheckpointStore = handoff

	if err := incidents.PutIncident(ctx, incident); err == nil {
		t.Fatal("PutIncident before candidate Put succeeded")
	}
	if _, err := queue.Put(ctx, candidate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := queue.Ack(ctx, candidate.ID, incident.ID); err == nil {
		t.Fatal("Ack before incident persistence succeeded")
	}
	if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err == nil {
		t.Fatal("AdvanceCheckpoint before incident persistence and Ack succeeded")
	}
	if err := incidents.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident: %v", err)
	}
	if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err == nil {
		t.Fatal("AdvanceCheckpoint before Ack succeeded")
	}
	if err := queue.Ack(ctx, candidate.ID, incident.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err != nil {
		t.Fatalf("AdvanceCheckpoint: %v", err)
	}

	wantTransitions := []string{"Put", "PutIncident", "Ack", "AdvanceCheckpoint"}
	if !reflect.DeepEqual(handoff.state.transitions, wantTransitions) {
		t.Fatalf("durable transitions = %v, want %v", handoff.state.transitions, wantTransitions)
	}
}

func persistHandoffThrough(
	t *testing.T,
	ctx context.Context,
	queue CandidateQueue,
	incidents IncidentStore,
	checkpoints CheckpointStore,
	candidate domain.RescueCandidate,
	incident Incident,
	checkpoint Checkpoint,
	steps int,
) {
	t.Helper()

	result, err := queue.Put(ctx, candidate)
	if err != nil || result != PutInserted {
		t.Fatalf("initial Put = (%v, %v), want (%v, nil)", result, err, PutInserted)
	}
	if steps >= 2 {
		if err := incidents.PutIncident(ctx, incident); err != nil {
			t.Fatalf("PutIncident before crash: %v", err)
		}
	}
	if steps >= 3 {
		if err := queue.Ack(ctx, candidate.ID, incident.ID); err != nil {
			t.Fatalf("Ack before crash: %v", err)
		}
	}
	if steps >= 4 {
		if err := checkpoints.AdvanceCheckpoint(ctx, checkpoint); err != nil {
			t.Fatalf("AdvanceCheckpoint before crash: %v", err)
		}
	}
}

func assertCompletedHandoff(
	t *testing.T,
	ctx context.Context,
	queue CandidateQueue,
	incidents IncidentStore,
	checkpoints CheckpointStore,
	state *durableState,
	candidate domain.RescueCandidate,
	incident Incident,
	checkpoint Checkpoint,
) {
	t.Helper()

	replay, err := queue.Replay(ctx, candidate.Network)
	if err != nil || len(replay) != 0 {
		t.Fatalf("Replay after completion = (%v, %v), want no candidates", replay, err)
	}
	persistedIncident, found, err := incidents.IncidentByCandidate(ctx, candidate.ID)
	if err != nil || !found || persistedIncident != incident {
		t.Fatalf("IncidentByCandidate = (%v, %v, %v), want (%v, true, nil)", persistedIncident, found, err, incident)
	}
	persistedCheckpoint, found, err := checkpoints.LoadCheckpoint(ctx, candidate.Network)
	if err != nil || !found || persistedCheckpoint != checkpoint {
		t.Fatalf("LoadCheckpoint = (%v, %v, %v), want (%v, true, nil)", persistedCheckpoint, found, err, checkpoint)
	}
	wantTransitions := []string{"Put", "PutIncident", "Ack", "AdvanceCheckpoint"}
	if !reflect.DeepEqual(state.transitions, wantTransitions) {
		t.Fatalf("durable transitions = %v, want %v", state.transitions, wantTransitions)
	}
}

func handoffFixture() (domain.RescueCandidate, Incident, Checkpoint) {
	candidate := handoffCandidate()
	incident := Incident{
		ID:        domain.NewIncidentID(candidate.ID),
		Candidate: candidate.ID,
		Network:   candidate.Network,
		CreatedAt: time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
	}
	checkpoint := Checkpoint{
		Network:     candidate.Network,
		BlockNumber: candidate.BlockNumber,
		BlockHash:   candidate.BlockHash,
	}
	return candidate, incident, checkpoint
}

func handoffCandidate() domain.RescueCandidate {
	var source common.Address
	source[len(source)-1] = 1
	var blockHash common.Hash
	blockHash[len(blockHash)-1] = 2
	return domain.NewBlockCandidate(31337, domain.CandidateNative, source, blockHash, 42)
}
