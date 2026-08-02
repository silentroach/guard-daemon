package main

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
)

var (
	errHandoffNetwork   = errors.New("работа относится к другой сети")
	errIncidentOrder    = errors.New("incident можно сохранить только после candidate")
	errIncidentConflict = errors.New("candidate связан с другим incident")
	errAckOrder         = errors.New("ack допустим только после сохранения incident")
	errNackOrder        = errors.New("nack допустим только для головы pending очереди после сохранения incident")
)

const (
	legacyMaxPending     = 1024
	legacyTombstoneLimit = 4096
)

type legacyHandoffEntry struct {
	candidate    domain.RescueCandidate
	acknowledged bool
	incident     domain.IncidentID
}

type delayedCandidate struct {
	id      domain.CandidateID
	retryAt time.Time
	order   uint64
}

type delayedCandidates []delayedCandidate

func (candidates delayedCandidates) Len() int { return len(candidates) }
func (candidates delayedCandidates) Less(i, j int) bool {
	if candidates[i].retryAt.Equal(candidates[j].retryAt) {
		return candidates[i].order < candidates[j].order
	}
	return candidates[i].retryAt.Before(candidates[j].retryAt)
}
func (candidates delayedCandidates) Swap(i, j int) {
	candidates[i], candidates[j] = candidates[j], candidates[i]
}
func (candidates *delayedCandidates) Push(value any) {
	*candidates = append(*candidates, value.(delayedCandidate))
}
func (candidates *delayedCandidates) Pop() any {
	old := *candidates
	last := old[len(old)-1]
	*candidates = old[:len(old)-1]
	return last
}

// legacyMemoryHandoff является только process-local адаптером совместимости.
// Он сохраняет pending и tombstone между reconnect одного процесса, но не
// переживает аварийную остановку. Persistent реализация принадлежит Tasks 06/07.
type legacyMemoryHandoff struct {
	mu         sync.Mutex
	network    domain.NetworkID
	clock      clock.Clock
	entries    map[domain.CandidateID]legacyHandoffEntry
	pending    []domain.CandidateID
	delayed    delayedCandidates
	delayOrder uint64
	incidents  map[domain.CandidateID]store.Incident
	tombstones []domain.CandidateID
	scanCursor store.Checkpoint
	checkpoint store.Checkpoint
	wake       chan struct{}
}

func newLegacyMemoryHandoff(network domain.NetworkID, serviceClock clock.Clock) *legacyMemoryHandoff {
	return &legacyMemoryHandoff{
		network:   network,
		clock:     serviceClock,
		entries:   make(map[domain.CandidateID]legacyHandoffEntry),
		incidents: make(map[domain.CandidateID]store.Incident),
		wake:      make(chan struct{}),
	}
}

func (handoff *legacyMemoryHandoff) Put(ctx context.Context, candidate domain.RescueCandidate) (store.PutResult, error) {
	if candidate.Network != handoff.network {
		return 0, errHandoffNetwork
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		handoff.mu.Lock()
		if entry, exists := handoff.entries[candidate.ID]; exists {
			handoff.mu.Unlock()
			if entry.acknowledged {
				return store.PutAlreadyAcknowledged, nil
			}
			return store.PutAlreadyPending, nil
		}
		if len(handoff.pending)+len(handoff.delayed) < legacyMaxPending {
			handoff.entries[candidate.ID] = legacyHandoffEntry{candidate: candidate}
			handoff.pending = append(handoff.pending, candidate.ID)
			handoff.signalLocked()
			handoff.mu.Unlock()
			return store.PutInserted, nil
		}
		wake := handoff.wake
		handoff.mu.Unlock()

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-wake:
		}
	}
}

func (handoff *legacyMemoryHandoff) Next(ctx context.Context, network domain.NetworkID) (domain.RescueCandidate, error) {
	if network != handoff.network {
		return domain.RescueCandidate{}, errHandoffNetwork
	}

	for {
		if err := ctx.Err(); err != nil {
			return domain.RescueCandidate{}, err
		}
		handoff.mu.Lock()
		if err := ctx.Err(); err != nil {
			handoff.mu.Unlock()
			return domain.RescueCandidate{}, err
		}
		handoff.promoteDelayedLocked()
		handoff.compactPendingLocked()
		if len(handoff.pending) > 0 {
			entry := handoff.entries[handoff.pending[0]]
			handoff.mu.Unlock()
			return entry.candidate, nil
		}
		wake := handoff.wake
		var timer clock.Timer
		if len(handoff.delayed) > 0 {
			delay := handoff.delayed[0].retryAt.Sub(handoff.clock.Now())
			if delay < 0 {
				delay = 0
			}
			timer = handoff.clock.NewTimer(delay)
		}
		handoff.mu.Unlock()

		if timer == nil {
			select {
			case <-ctx.Done():
				return domain.RescueCandidate{}, ctx.Err()
			case <-wake:
			}
			continue
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.RescueCandidate{}, ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C():
		}
	}
}

func (handoff *legacyMemoryHandoff) Replay(ctx context.Context, network domain.NetworkID) ([]domain.RescueCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != handoff.network {
		return nil, errHandoffNetwork
	}

	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	handoff.compactPendingLocked()
	pending := make([]domain.RescueCandidate, 0, len(handoff.pending)+len(handoff.delayed))
	for _, id := range handoff.pending {
		entry := handoff.entries[id]
		if !entry.acknowledged {
			pending = append(pending, entry.candidate)
		}
	}
	delayed := append(delayedCandidates(nil), handoff.delayed...)
	sort.Slice(delayed, func(i, j int) bool { return delayed.Less(i, j) })
	for _, delayedCandidate := range delayed {
		entry := handoff.entries[delayedCandidate.id]
		if !entry.acknowledged {
			pending = append(pending, entry.candidate)
		}
	}
	return pending, nil
}

func (handoff *legacyMemoryHandoff) Ack(ctx context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, exists := handoff.entries[candidateID]
	if !exists {
		return errAckOrder
	}
	if entry.acknowledged {
		if entry.incident != incidentID {
			return errIncidentConflict
		}
		return nil
	}
	incident, exists := handoff.incidents[candidateID]
	if !exists || incident.ID != incidentID || incidentID != domain.NewIncidentID(candidateID) {
		return errAckOrder
	}
	entry.acknowledged = true
	entry.incident = incidentID
	handoff.entries[candidateID] = entry
	handoff.tombstones = append(handoff.tombstones, candidateID)
	handoff.compactPendingLocked()
	handoff.trimTombstonesLocked()
	handoff.signalLocked()
	return nil
}

func (handoff *legacyMemoryHandoff) Nack(ctx context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID, retryAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	handoff.compactPendingLocked()
	entry, exists := handoff.entries[candidateID]
	incident, incidentExists := handoff.incidents[candidateID]
	if !exists || entry.acknowledged || !incidentExists || incident.ID != incidentID ||
		len(handoff.pending) == 0 || handoff.pending[0] != candidateID {
		return errNackOrder
	}
	entry.incident = incidentID
	handoff.entries[candidateID] = entry
	handoff.pending = handoff.pending[1:]
	handoff.delayOrder++
	heap.Push(&handoff.delayed, delayedCandidate{id: candidateID, retryAt: retryAt, order: handoff.delayOrder})
	handoff.signalLocked()
	return nil
}

func (handoff *legacyMemoryHandoff) PutIncident(ctx context.Context, incident store.Incident) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if incident.Network != handoff.network {
		return errHandoffNetwork
	}

	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := handoff.entries[incident.Candidate]; !exists {
		return errIncidentOrder
	}
	if incident.ID != domain.NewIncidentID(incident.Candidate) {
		return errIncidentConflict
	}
	if persisted, exists := handoff.incidents[incident.Candidate]; exists {
		if persisted.ID != incident.ID || persisted.Candidate != incident.Candidate || persisted.Network != incident.Network {
			return errIncidentConflict
		}
		return nil
	}
	handoff.incidents[incident.Candidate] = incident
	return nil
}

func (handoff *legacyMemoryHandoff) IncidentByCandidate(ctx context.Context, candidateID domain.CandidateID) (store.Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Incident{}, false, err
	}
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	incident, exists := handoff.incidents[candidateID]
	return incident, exists, nil
}

func (handoff *legacyMemoryHandoff) PutObserved(ctx context.Context, candidate domain.RescueCandidate) (store.PutResult, error) {
	return handoff.Put(ctx, candidate)
}

func (handoff *legacyMemoryHandoff) MarkRemoved(ctx context.Context, candidateID domain.CandidateID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	entry, exists := handoff.entries[candidateID]
	if !exists || entry.acknowledged {
		return errAckOrder
	}
	delete(handoff.entries, candidateID)
	handoff.compactPendingLocked()
	handoff.signalLocked()
	return nil
}

func (handoff *legacyMemoryHandoff) LoadScanCursor(ctx context.Context, network domain.NetworkID) (store.Checkpoint, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Checkpoint{}, false, err
	}
	if network != handoff.network {
		return store.Checkpoint{}, false, errHandoffNetwork
	}
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	return handoff.scanCursor, handoff.scanCursor != (store.Checkpoint{}), nil
}

func (handoff *legacyMemoryHandoff) LoadCheckpoint(ctx context.Context, network domain.NetworkID) (store.Checkpoint, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Checkpoint{}, false, err
	}
	if network != handoff.network {
		return store.Checkpoint{}, false, errHandoffNetwork
	}
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	return handoff.checkpoint, handoff.checkpoint != (store.Checkpoint{}), nil
}

func (handoff *legacyMemoryHandoff) CommitCanonicalBlock(ctx context.Context, block store.CanonicalBlock) error {
	if block.Network != handoff.network {
		return errHandoffNetwork
	}
	for _, candidate := range block.Candidates {
		if _, err := handoff.Put(ctx, candidate); err != nil {
			return err
		}
	}
	handoff.mu.Lock()
	handoff.scanCursor = block.Checkpoint
	if len(block.Candidates) == 0 {
		handoff.checkpoint = block.Checkpoint
	}
	handoff.mu.Unlock()
	return nil
}

func (handoff *legacyMemoryHandoff) DiscoveredTokens(ctx context.Context, network domain.NetworkID) ([]common.Address, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != handoff.network {
		return nil, errHandoffNetwork
	}
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	seen := make(map[common.Address]struct{})
	for _, entry := range handoff.entries {
		if entry.candidate.Kind == domain.CandidateToken {
			seen[entry.candidate.Token.Address] = struct{}{}
		}
	}
	result := make([]common.Address, 0, len(seen))
	for address := range seen {
		result = append(result, address)
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i][:], result[j][:]) < 0 })
	return result, nil
}

func (handoff *legacyMemoryHandoff) DiscoveryOverflowed(ctx context.Context, network domain.NetworkID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if network != handoff.network {
		return false, errHandoffNetwork
	}
	return false, nil
}

func (*legacyMemoryHandoff) Close() error { return nil }

func (handoff *legacyMemoryHandoff) signalLocked() {
	close(handoff.wake)
	handoff.wake = make(chan struct{})
}

func (handoff *legacyMemoryHandoff) compactPendingLocked() {
	for len(handoff.pending) > 0 {
		entry, exists := handoff.entries[handoff.pending[0]]
		if exists && !entry.acknowledged {
			break
		}
		handoff.pending = handoff.pending[1:]
	}
	if len(handoff.pending) == 0 {
		handoff.pending = nil
	}
}

func (handoff *legacyMemoryHandoff) promoteDelayedLocked() {
	now := handoff.clock.Now()
	for len(handoff.delayed) > 0 && !handoff.delayed[0].retryAt.After(now) {
		candidate := heap.Pop(&handoff.delayed).(delayedCandidate)
		entry, exists := handoff.entries[candidate.id]
		if exists && !entry.acknowledged {
			handoff.pending = append(handoff.pending, candidate.id)
		}
	}
}

func (handoff *legacyMemoryHandoff) trimTombstonesLocked() {
	for len(handoff.tombstones) > legacyTombstoneLimit {
		id := handoff.tombstones[0]
		handoff.tombstones = handoff.tombstones[1:]
		if entry, exists := handoff.entries[id]; exists && entry.acknowledged {
			delete(handoff.entries, id)
			delete(handoff.incidents, id)
		}
	}
}

var (
	_ store.CandidateQueue = (*legacyMemoryHandoff)(nil)
	_ store.IncidentStore  = (*legacyMemoryHandoff)(nil)
	_ store.HandoffStore   = (*legacyMemoryHandoff)(nil)
)
