package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

func TestBoltStorePersistsCrashBoundariesAndCheckpoint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	candidate := testBlockCandidate(options, 42, testHash(42))
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 42, BlockHash: candidate.BlockHash},
		ParentHash: testHash(41),
		Candidates: []domain.RescueCandidate{candidate},
	}
	incident := testIncident(candidate, 1)

	store := openTestStore(t, path, options)
	result, err := store.Put(ctx, candidate)
	if err != nil || result != PutInserted {
		t.Fatalf("Put = (%v, %v), want (%v, nil)", result, err, PutInserted)
	}
	store = reopenTestStore(t, store, path, options)
	assertReplay(t, store, candidate)

	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
	assertCheckpoint(t, store, false, Checkpoint{})
	store = reopenTestStore(t, store, path, options)
	assertReplay(t, store, candidate)

	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	persisted, found, err := store.IncidentByCandidate(ctx, candidate.ID)
	if err != nil || !found || persisted != incident {
		t.Fatalf("IncidentByCandidate = (%v, %v, %v), want (%v, true, nil)", persisted, found, err, incident)
	}

	if err := store.Ack(ctx, candidate.ID, incident.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	assertCheckpoint(t, store, false, block.Checkpoint)
	assertReplay(t, store)
	result, err = store.Put(ctx, candidate)
	if err != nil || result != PutAlreadyAcknowledged {
		t.Fatalf("duplicate Put after Ack = (%v, %v), want (%v, nil)", result, err, PutAlreadyAcknowledged)
	}
	if err := store.PutIncident(ctx, Incident{ID: incident.ID, Candidate: incident.Candidate, Network: incident.Network, CreatedAt: incident.CreatedAt.Add(time.Hour)}); err != nil {
		t.Fatalf("duplicate PutIncident: %v", err)
	}
	if err := store.Ack(ctx, candidate.ID, incident.ID); err != nil {
		t.Fatalf("duplicate Ack: %v", err)
	}
}

func TestBoltStoreEmptyAndMultiCandidateBlocksAdvanceContiguously(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()

	empty := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 100, BlockHash: testHash(100)},
		ParentHash: testHash(99),
	}
	if err := store.CommitCanonicalBlock(ctx, empty); err != nil {
		t.Fatalf("commit baseline empty block: %v", err)
	}
	assertCheckpoint(t, store, true, empty.Checkpoint)
	assertCheckpoint(t, store, false, empty.Checkpoint)

	first := testLogCandidate(options, 101, testHash(101), 1, 1)
	second := testLogCandidate(options, 101, testHash(101), 2, 2)
	multi := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 101, BlockHash: testHash(101)},
		ParentHash: empty.BlockHash,
		Candidates: []domain.RescueCandidate{second, first},
	}
	if err := store.CommitCanonicalBlock(ctx, multi); err != nil {
		t.Fatalf("commit multi-candidate block: %v", err)
	}
	assertCheckpoint(t, store, true, multi.Checkpoint)
	assertCheckpoint(t, store, false, empty.Checkpoint)

	for index, candidate := range []domain.RescueCandidate{first, second} {
		incident := testIncident(candidate, time.Duration(index+1))
		if err := store.PutIncident(ctx, incident); err != nil {
			t.Fatalf("PutIncident %d: %v", index, err)
		}
		if err := store.Ack(ctx, candidate.ID, incident.ID); err != nil {
			t.Fatalf("Ack %d: %v", index, err)
		}
		if index == 0 {
			assertCheckpoint(t, store, false, empty.Checkpoint)
		}
	}
	assertCheckpoint(t, store, false, multi.Checkpoint)

	emptyAfter := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 102, BlockHash: testHash(102)},
		ParentHash: multi.BlockHash,
	}
	if err := store.CommitCanonicalBlock(ctx, emptyAfter); err != nil {
		t.Fatalf("commit trailing empty block: %v", err)
	}
	assertCheckpoint(t, store, false, emptyAfter.Checkpoint)

	alreadyAcked := testBlockCandidate(options, 103, testHash(103))
	if _, err := store.Put(ctx, alreadyAcked); err != nil {
		t.Fatalf("Put pre-ack candidate: %v", err)
	}
	incident := testIncident(alreadyAcked, 4)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident pre-ack candidate: %v", err)
	}
	if err := store.Ack(ctx, alreadyAcked.ID, incident.ID); err != nil {
		t.Fatalf("Ack pre-commit candidate: %v", err)
	}
	alreadyAckedBlock := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 103, BlockHash: testHash(103)},
		ParentHash: emptyAfter.BlockHash,
		Candidates: []domain.RescueCandidate{alreadyAcked},
	}
	if err := store.CommitCanonicalBlock(ctx, alreadyAckedBlock); err != nil {
		t.Fatalf("commit already-acknowledged block: %v", err)
	}
	assertCheckpoint(t, store, false, alreadyAckedBlock.Checkpoint)
}

func TestBoltStoreCommitFailureDoesNotMoveScanCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	baseline := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 500, BlockHash: testHash(50)},
		ParentHash: testHash(49),
	}
	if err := store.CommitCanonicalBlock(ctx, baseline); err != nil {
		t.Fatalf("commit baseline: %v", err)
	}

	wrongMember := testBlockCandidate(options, 502, testHash(52))
	failed := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 501, BlockHash: testHash(51)},
		ParentHash: baseline.BlockHash,
		Candidates: []domain.RescueCandidate{wrongMember},
	}
	if err := store.CommitCanonicalBlock(ctx, failed); err == nil {
		t.Fatal("commit with FilterLogs-equivalent incomplete membership succeeded")
	}
	assertCheckpoint(t, store, true, baseline.Checkpoint)

	validMember := testBlockCandidate(options, 501, testHash(51))
	if _, err := store.Put(ctx, validMember); err != nil {
		t.Fatalf("Put before incomplete seal: %v", err)
	}
	failed.Candidates = nil
	if err := store.CommitCanonicalBlock(ctx, failed); err == nil {
		t.Fatal("empty seal omitted trusted ready work from the same block")
	}
	assertCheckpoint(t, store, true, baseline.Checkpoint)

	failed.Candidates = []domain.RescueCandidate{validMember}
	failed.ParentHash = testHash(1)
	if err := store.CommitCanonicalBlock(ctx, failed); err == nil {
		t.Fatal("commit with wrong parent succeeded")
	}
	assertCheckpoint(t, store, true, baseline.Checkpoint)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	failed.ParentHash = baseline.BlockHash
	if err := store.CommitCanonicalBlock(canceled, failed); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit error = %v, want context.Canceled", err)
	}
	assertCheckpoint(t, store, true, baseline.Checkpoint)
}

func TestBoltStoreCanonicalSealDeletesSameHashObservedNonMember(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	hash := testHash(0x61)
	fake := testLogCandidate(options, 61, hash, 1, 1)
	if _, err := store.PutObserved(ctx, fake); err != nil {
		t.Fatalf("PutObserved fake log: %v", err)
	}
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 61, BlockHash: hash},
		ParentHash: testHash(0x60),
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
	assertCheckpoint(t, store, false, block.Checkpoint)
	if data := candidateData(t, store, fake.ID); data != nil {
		t.Fatal("same-hash provisional non-member survived canonical seal")
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		record, err := decodeBlock(tx.Bucket(blocksBucket).Get(blockNumberKey(block.BlockNumber)), store.journalCapacity)
		if err != nil || len(record.Members) != 0 {
			t.Fatalf("canonical membership = (%v, %v), want empty", record.Members, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := store.PutObserved(ctx, fake); err != nil || result != PutAlreadyAcknowledged {
		t.Fatalf("late fake coalescing = (%v, %v), want acknowledged", result, err)
	}
}

func TestBoltStoreRejectsCandidateAfterBlockSeal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	sealed := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 10, BlockHash: testHash(10)},
		ParentHash: testHash(9),
	}
	if err := store.CommitCanonicalBlock(ctx, sealed); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	late := testLogCandidate(options, 10, sealed.BlockHash, 1, 1)
	result, err := store.PutObserved(ctx, late)
	if err != nil || result != PutAlreadyAcknowledged {
		t.Fatalf("late PutObserved = (%v, %v), want acknowledged coalescing", result, err)
	}
	assertReplay(t, store)
	assertCheckpoint(t, store, true, sealed.Checkpoint)
	assertCheckpoint(t, store, false, sealed.Checkpoint)
}

func TestBoltStoreDuplicateAfterRestartPreservesFirstPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	original := testLogCandidate(options, 70, testHash(70), 1, 1)
	original.Token.Symbol = "FIRST"
	original.Token.Decimals = 6
	original.Generation = 1

	store := openTestStore(t, path, options)
	if _, err := store.Put(ctx, original); err != nil {
		t.Fatalf("Put: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	duplicate := original
	duplicate.Token.Symbol = "SECOND"
	duplicate.Token.Decimals = 18
	duplicate.Generation = 99
	result, err := store.Put(ctx, duplicate)
	if err != nil || result != PutAlreadyPending {
		t.Fatalf("duplicate Put = (%v, %v), want (%v, nil)", result, err, PutAlreadyPending)
	}
	assertReplay(t, store, original)

	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 70, BlockHash: testHash(70)},
		ParentHash: testHash(69),
		Candidates: []domain.RescueCandidate{duplicate},
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("CommitCanonicalBlock: %v", err)
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("idempotent CommitCanonicalBlock: %v", err)
	}
	conflict := block
	conflict.Candidates = nil
	if err := store.CommitCanonicalBlock(ctx, conflict); err == nil {
		t.Fatal("conflicting duplicate canonical block succeeded")
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
}

func TestBoltStoreDelayedRetryUsesClockWithoutBlockingReady(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	delayed := testBlockCandidate(options, 1, testHash(1))
	ready := testBlockCandidate(options, 2, testHash(2))

	for index, candidate := range []domain.RescueCandidate{delayed, ready} {
		if _, err := store.Put(ctx, candidate); err != nil {
			t.Fatalf("Put %d: %v", index, err)
		}
		incident := testIncident(candidate, time.Duration(index+1))
		if err := store.PutIncident(ctx, incident); err != nil {
			t.Fatalf("PutIncident %d: %v", index, err)
		}
	}
	delayedIncident := testIncident(delayed, 1)
	if err := store.Nack(ctx, delayed.ID, delayedIncident.ID, clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	next, err := store.Next(ctx, options.Network)
	if err != nil || next != ready {
		t.Fatalf("Next with poison delayed item = (%v, %v), want ready candidate", next, err)
	}
	readyIncident := testIncident(ready, 2)
	if err := store.Ack(ctx, ready.ID, readyIncident.ID); err != nil {
		t.Fatalf("Ack ready: %v", err)
	}
	clock.Advance(time.Hour)
	next, err = store.Next(ctx, options.Network)
	if err != nil || next != delayed {
		t.Fatalf("Next after fake clock advance = (%v, %v), want delayed candidate", next, err)
	}
}

func TestBoltStoreObservationSaturationIsImmediateAndCanonicalCommitProgresses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	options.MaxPending = 1
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	provisional := testBlockCandidate(options, 1, testHash(1))
	blocked := testBlockCandidate(options, 2, testHash(2))
	if _, err := store.PutObserved(ctx, provisional); err != nil {
		t.Fatalf("PutObserved: %v", err)
	}

	if result, err := store.PutObserved(ctx, blocked); result != 0 || !errors.Is(err, ErrObservationSaturated) {
		t.Fatalf("saturated PutObserved = (%v, %v), want immediate ErrObservationSaturated", result, err)
	}
	assertReplay(t, store)
	assertCheckpoint(t, store, true, Checkpoint{})

	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 1, BlockHash: provisional.BlockHash},
		ParentHash: testHash(0xff),
		Candidates: []domain.RescueCandidate{provisional},
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("canonical commit after provisional saturation: %v", err)
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
	if data := candidateData(t, store, blocked.ID); data != nil {
		t.Fatal("saturated observation was persisted")
	}
}

func TestBoltStoreCanonicalBlockStagesBeyondReadyCapacityAndAckPromotes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	options.MaxPending = 2
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	hash := testHash(0x81)
	candidates := []domain.RescueCandidate{
		testLogCandidate(options, 81, hash, 1, 1),
		testLogCandidate(options, 81, hash, 2, 2),
		testLogCandidate(options, 81, hash, 3, 3),
	}
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 81, BlockHash: hash},
		ParentHash: testHash(0x80),
		Candidates: candidates,
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("CommitCanonicalBlock(MaxPending+1): %v", err)
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
	assertStatusCounts(t, store, map[CandidateStatus]int{CandidateReady: 2, CandidateStaged: 1})
	assertReplaySet(t, store, candidates)

	ready := candidateWithStatus(t, store, candidates, CandidateReady)
	incident := testIncident(ready, 1)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident: %v", err)
	}
	if err := store.Ack(ctx, ready.ID, incident.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	assertStatusCounts(t, store, map[CandidateStatus]int{CandidateReady: 2, CandidateAcknowledged: 1})
}

func TestBoltStoreFutureDelayedWindowDoesNotStarveStagedAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	options.MaxPending = 2
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	poison := []domain.RescueCandidate{
		domain.NewPeriodicCandidate(options.Network, options.Source, 1, 1),
		domain.NewPeriodicCandidate(options.Network, options.Source, 1, 2),
	}
	for index, candidate := range poison {
		if _, err := store.Put(ctx, candidate); err != nil {
			t.Fatalf("Put poison %d: %v", index, err)
		}
		incident := testIncident(candidate, time.Duration(index+1))
		if err := store.PutIncident(ctx, incident); err != nil {
			t.Fatalf("PutIncident poison %d: %v", index, err)
		}
		if err := store.Nack(ctx, candidate.ID, incident.ID, clock.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Nack poison %d: %v", index, err)
		}
	}
	valid := testBlockCandidate(options, 90, testHash(0x90))
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 90, BlockHash: valid.BlockHash},
		ParentHash: testHash(0x89),
		Candidates: []domain.RescueCandidate{valid},
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("CommitCanonicalBlock valid candidate: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	assertReplaySet(t, store, append(poison, valid))
	assertStatusCounts(t, store, map[CandidateStatus]int{CandidateDelayed: 2, CandidateReady: 1})
	next, err := store.Next(ctx, options.Network)
	if err != nil || next != valid {
		t.Fatalf("Next with delayed poison window = (%v, %v), want staged canonical candidate", next, err)
	}
}

func TestBoltStoreRoundRobinPreventsDueDelayedStarvation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 13, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	options.MaxPending = 1
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	poison := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 1)
	if _, err := store.Put(ctx, poison); err != nil {
		t.Fatal(err)
	}
	poisonIncident := testIncident(poison, 1)
	if err := store.PutIncident(ctx, poisonIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Nack(ctx, poison.ID, poisonIncident.ID, clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	first := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 2)
	second := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 3)
	third := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 4)
	if _, err := store.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, second); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if next, err := store.Next(ctx, options.Network); err != nil || next != first {
		t.Fatalf("first Next = (%v, %v)", next, err)
	}
	firstIncident := testIncident(first, 2)
	if err := store.PutIncident(ctx, firstIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(ctx, first.ID, firstIncident.ID); err != nil {
		t.Fatal(err)
	}

	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	if next, err := store.Next(ctx, options.Network); err != nil || next != second {
		t.Fatalf("second Next = (%v, %v)", next, err)
	}
	if _, err := store.Put(ctx, third); err != nil {
		t.Fatal(err)
	}
	secondIncident := testIncident(second, 3)
	if err := store.PutIncident(ctx, secondIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(ctx, second.ID, secondIncident.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := store.Next(ctx, options.Network); err != nil || next != poison {
		t.Fatalf("due delayed candidate starved by staged backlog: Next = (%v, %v)", next, err)
	}
}

func TestBoltStoreFIFODispatchPreservesFairPromotionWithMultipleSlots(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 14, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	options.MaxPending = 2
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	poison := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 1)
	filler := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 2)
	stagedFirst := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 3)
	for _, candidate := range []domain.RescueCandidate{poison, filler} {
		if _, err := store.Put(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	poisonIncident := testIncident(poison, 1)
	if err := store.PutIncident(ctx, poisonIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Nack(ctx, poison.ID, poisonIncident.ID, clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, stagedFirst); err != nil {
		t.Fatal(err)
	}
	lowerID := domain.RescueCandidate{}
	for observation := uint64(10); observation < 10_000; observation++ {
		candidate := domain.NewPeriodicCandidate(options.Network, options.Source, 1, observation)
		if bytes.Compare(candidate.ID[:], poison.ID[:]) < 0 {
			lowerID = candidate
			break
		}
	}
	if lowerID == (domain.RescueCandidate{}) {
		t.Fatal("не найден deterministic lower-ID candidate")
	}
	if _, err := store.Put(ctx, lowerID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if next, err := store.Next(ctx, options.Network); err != nil || next != filler {
		t.Fatalf("first FIFO Next = (%v, %v), want filler", next, err)
	}
	fillerIncident := testIncident(filler, 2)
	if err := store.PutIncident(ctx, fillerIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(ctx, filler.ID, fillerIncident.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := store.Next(ctx, options.Network); err != nil || next != stagedFirst {
		t.Fatalf("second FIFO Next = (%v, %v), want first staged", next, err)
	}
	stagedIncident := testIncident(stagedFirst, 3)
	if err := store.PutIncident(ctx, stagedIncident); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(ctx, stagedFirst.ID, stagedIncident.ID); err != nil {
		t.Fatal(err)
	}

	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	if next, err := store.Next(ctx, options.Network); err != nil || next != poison {
		t.Fatalf("FIFO lost round-robin order to lower ID: Next = (%v, %v), lower=%s", next, err, lowerID.ID)
	}
}

func TestBoltStoreJournalSaturationCancelsWithoutCursorAndRecoversAfterAck(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	options.MaxPending = 1
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	store.journalCapacity = 2
	firstHash := testHash(0xa1)
	firstCandidates := []domain.RescueCandidate{
		testLogCandidate(options, 100, firstHash, 1, 1),
		testLogCandidate(options, 100, firstHash, 2, 2),
	}
	firstBlock := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 100, BlockHash: firstHash},
		ParentHash: testHash(0xa0),
		Candidates: firstCandidates,
	}
	if err := store.CommitCanonicalBlock(ctx, firstBlock); err != nil {
		t.Fatalf("commit full journal: %v", err)
	}
	secondCandidate := testLogCandidate(options, 101, testHash(0xa2), 3, 3)
	secondBlock := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 101, BlockHash: secondCandidate.BlockHash},
		ParentHash: firstBlock.BlockHash,
		Candidates: []domain.RescueCandidate{secondCandidate},
	}
	canceled, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	cancelResult := make(chan error, 1)
	go func() {
		close(started)
		cancelResult <- store.CommitCanonicalBlock(canceled, secondBlock)
	}()
	<-started
	cancel()
	if err := <-cancelResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("saturated canceled commit error = %v, want context.Canceled", err)
	}
	assertCheckpoint(t, store, true, firstBlock.Checkpoint)

	ready := candidateWithStatus(t, store, firstCandidates, CandidateReady)
	incident := testIncident(ready, 1)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident before capacity release: %v", err)
	}
	commitResult := make(chan error, 1)
	go func() { commitResult <- store.CommitCanonicalBlock(ctx, secondBlock) }()
	ackResult := make(chan error, 1)
	go func() { ackResult <- store.Ack(ctx, ready.ID, incident.ID) }()
	if err := <-ackResult; err != nil {
		t.Fatalf("Ack releasing journal: %v", err)
	}
	if err := <-commitResult; err != nil {
		t.Fatalf("commit after Ack release: %v", err)
	}
	assertCheckpoint(t, store, true, secondBlock.Checkpoint)
}

func TestBoltStoreBoundsUnconfirmedBlockSpanUntilConsumerAck(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	candidate := testBlockCandidate(options, 1, testHash(2))
	first := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 1, BlockHash: candidate.BlockHash},
		ParentHash: testHash(1),
		Candidates: []domain.RescueCandidate{candidate},
	}
	if err := store.CommitCanonicalBlock(ctx, first); err != nil {
		t.Fatalf("commit first blocked block: %v", err)
	}
	previous := first.BlockHash
	var last CanonicalBlock
	for number := uint64(2); number <= unconfirmedBlockLimit; number++ {
		last = CanonicalBlock{
			Checkpoint: Checkpoint{Network: options.Network, BlockNumber: number, BlockHash: testHash(byte(number%250 + 1))},
			ParentHash: previous,
		}
		if err := store.CommitCanonicalBlock(ctx, last); err != nil {
			t.Fatalf("commit unconfirmed block %d: %v", number, err)
		}
		previous = last.BlockHash
	}
	blocked := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: unconfirmedBlockLimit + 1, BlockHash: testHash(9)},
		ParentHash: previous,
	}
	canceled, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- store.CommitCanonicalBlock(canceled, blocked)
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded span cancellation = %v, want context.Canceled", err)
	}
	assertCheckpoint(t, store, true, last.Checkpoint)

	incident := testIncident(candidate, 1)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident: %v", err)
	}
	if err := store.Ack(ctx, candidate.ID, incident.ID); err != nil {
		t.Fatalf("Ack releasing block span: %v", err)
	}
	if err := store.CommitCanonicalBlock(ctx, blocked); err != nil {
		t.Fatalf("commit after block span release: %v", err)
	}
	assertCheckpoint(t, store, true, blocked.Checkpoint)
	assertCheckpoint(t, store, false, blocked.Checkpoint)
}

func TestBoltStoreObservedRemovedAndCanonicalPromotion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()

	removed := testLogCandidate(options, 20, testHash(20), 1, 1)
	if _, err := store.PutObserved(ctx, removed); err != nil {
		t.Fatalf("PutObserved removed candidate: %v", err)
	}
	assertReplay(t, store)
	if err := store.MarkRemoved(ctx, removed.ID); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	removedBlock := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 20, BlockHash: testHash(20)},
		ParentHash: testHash(19),
		Candidates: []domain.RescueCandidate{removed},
	}
	if err := store.CommitCanonicalBlock(ctx, removedBlock); err != nil {
		t.Fatalf("commit block with removed member: %v", err)
	}
	assertCheckpoint(t, store, false, Checkpoint{})
	assertReplay(t, store, removed)
	removedIncident := testIncident(removed, 1)
	if err := store.PutIncident(ctx, removedIncident); err != nil {
		t.Fatalf("PutIncident canonical member after Removed: %v", err)
	}
	if err := store.Ack(ctx, removed.ID, removedIncident.ID); err != nil {
		t.Fatalf("Ack canonical member after Removed: %v", err)
	}
	assertCheckpoint(t, store, false, removedBlock.Checkpoint)

	observed := testLogCandidate(options, 21, testHash(21), 2, 2)
	observed.Token.Symbol = "OBSERVED"
	observed.Token.Decimals = 6
	if _, err := store.PutObserved(ctx, observed); err != nil {
		t.Fatalf("PutObserved promoted candidate: %v", err)
	}
	canonical := observed
	canonical.Token.Symbol = "CANONICAL"
	canonical.Token.Decimals = 18
	canonical.Generation = 7
	promotedBlock := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 21, BlockHash: testHash(21)},
		ParentHash: removedBlock.BlockHash,
		Candidates: []domain.RescueCandidate{canonical},
	}
	if err := store.CommitCanonicalBlock(ctx, promotedBlock); err != nil {
		t.Fatalf("commit observed candidate: %v", err)
	}
	assertReplay(t, store, canonical)
	assertCheckpoint(t, store, false, removedBlock.Checkpoint)
	if err := store.MarkRemoved(ctx, canonical.ID); err == nil {
		t.Fatal("MarkRemoved promoted ready candidate succeeded")
	}
	incident := testIncident(canonical, 3)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatalf("PutIncident promoted candidate: %v", err)
	}
	if err := store.Ack(ctx, canonical.ID, incident.ID); err != nil {
		t.Fatalf("Ack promoted candidate: %v", err)
	}
	assertCheckpoint(t, store, false, promotedBlock.Checkpoint)
}

func TestBoltStoreBindingSchemaLockAndPermissions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "private", "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || parentInfo.Mode().Perm() != 0o700 {
		t.Fatalf("parent permissions = (%v, %v), want 0700", parentInfo, err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = (%v, %v), want 0600", fileInfo, err)
	}

	started := time.Now()
	_, lockErr := Open(path, options)
	if lockErr == nil {
		t.Fatal("second Open acquired locked database")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("second Open exceeded bounded lock timeout: %s", time.Since(started))
	}
	if strings.Contains(lockErr.Error(), path) {
		t.Fatalf("public lock error leaked path: %v", lockErr)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mismatches := []OpenOptions{options, options, options, options, options}
	mismatches[0].Network++
	mismatches[1].Source[0] = 1
	mismatches[2].PolicyFingerprint[0]++
	mismatches[3].MaxPending++
	mismatches[4].MaxDiscoveredTokens++
	for index, mismatch := range mismatches {
		opened, err := Open(path, mismatch)
		if err == nil {
			opened.Close()
			t.Fatalf("binding mismatch %d opened database", index)
		}
		if strings.Contains(err.Error(), path) {
			t.Fatalf("binding mismatch %d leaked path: %v", index, err)
		}
	}

	raw, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if err := raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(schemaKey, encodeUint32(schemaVersion+1))
	}); err != nil {
		t.Fatalf("write future schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("future schema opened")
	}
}

func TestBoltStoreRejectsCorruptionOnReadAndReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	candidate := testBlockCandidate(options, 1, testHash(1))
	if _, err := store.Put(ctx, candidate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(candidatesBucket).Put(candidate.ID[:], []byte{recordVersion, byte(CandidateReady)})
	}); err != nil {
		t.Fatalf("inject corruption: %v", err)
	}
	if _, err := store.Replay(ctx, options.Network); !errors.Is(err, errCorrupt) {
		t.Fatalf("Replay corruption error = %v, want safe corruption error", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("corrupted database reopened")
	} else if strings.Contains(err.Error(), path) {
		t.Fatalf("corruption error leaked path: %v", err)
	}
}

func TestBoltStoreRejectsReinitializingExistingState(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, string){
		"усечённый файл": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
		},
		"удалённая схема": func(t *testing.T, path string) {
			t.Helper()
			raw, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.Update(func(tx *bolt.Tx) error {
				for _, bucket := range [][]byte{metaBucket, candidatesBucket, pendingBucket, readyOrderBucket, blockIndexBucket, incidentsBucket, blocksBucket, discoveredBucket, tombstonesBucket} {
					if err := tx.DeleteBucket(bucket); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "handoff.db")
			options := testOpenOptions(nil)
			store := openTestStore(t, path, options)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			corrupt(t, path)
			if opened, err := Open(path, options); err == nil {
				opened.Close()
				t.Fatal("существующий повреждённый state был переинициализирован")
			}
		})
	}
}

func TestBoltStoreRejectsReadyOrderSequenceRollback(t *testing.T) {
	for name, sequence := range map[string]uint64{"rollback": 0, "исчерпание": ^uint64(0)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "handoff.db")
			options := testOpenOptions(nil)
			store := openTestStore(t, path, options)
			candidate := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 1)
			if _, err := store.Put(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			setBucketSequence(t, path, readyOrderBucket, sequence)
			if opened, err := Open(path, options); err == nil {
				opened.Close()
				t.Fatal("store принял некорректный ready-order sequence")
			}
		})
	}
}

func TestBoltStoreRejectsCorruptTombstoneCoverageAndSequence(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"rollback sequence":  func(t *testing.T, path string) { setBucketSequence(t, path, tombstonesBucket, 0) },
		"exhausted sequence": func(t *testing.T, path string) { setBucketSequence(t, path, tombstonesBucket, ^uint64(0)) },
		"missing tombstone": func(t *testing.T, path string) {
			mutateStore(t, path, func(tx *bolt.Tx) error {
				cursor := tx.Bucket(tombstonesBucket).Cursor()
				key, _ := cursor.First()
				if key == nil {
					return errors.New("tombstone отсутствует")
				}
				return cursor.Delete()
			})
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "handoff.db")
			options := testOpenOptions(nil)
			store := openTestStore(t, path, options)
			candidate := domain.NewPeriodicCandidate(options.Network, options.Source, 1, 1)
			if _, err := store.Put(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
			incident := testIncident(candidate, 1)
			if err := store.PutIncident(context.Background(), incident); err != nil {
				t.Fatal(err)
			}
			if err := store.Ack(context.Background(), candidate.ID, incident.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			corrupt(t, path)
			if opened, err := Open(path, options); err == nil {
				opened.Close()
				t.Fatal("store принял повреждённые tombstones")
			}
		})
	}
}

func setBucketSequence(t *testing.T, path string, bucket []byte, sequence uint64) {
	t.Helper()
	mutateStore(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).SetSequence(sequence)
	})
}

func mutateStore(t *testing.T, path string, mutate func(*bolt.Tx) error) {
	t.Helper()
	raw, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mutateErr := raw.Update(mutate)
	closeErr := raw.Close()
	if mutateErr != nil {
		t.Fatal(mutateErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestBoltStoreDiscoveredTokensRequireCanonicalConfirmation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	options.MaxDiscoveredTokens = 2
	store := openTestStore(t, path, options)
	provisional := testLogCandidate(options, 1, testHash(1), 2, 1)
	if _, err := store.PutObserved(ctx, provisional); err != nil {
		t.Fatalf("PutObserved: %v", err)
	}
	assertDiscoveredTokens(t, store, options.Network, nil)
	if err := store.MarkRemoved(ctx, provisional.ID); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	assertDiscoveredTokens(t, store, options.Network, nil)
	if overflowed, err := store.DiscoveryOverflowed(ctx, options.Network); err != nil || overflowed {
		t.Fatalf("DiscoveryOverflowed = (%v, %v), want false", overflowed, err)
	}
}

func TestBoltStoreDiscoveryOverflowIsDurableAndDoesNotBlockWork(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	options.MaxDiscoveredTokens = 1024
	store := openTestStore(t, path, options)
	hash := testHash(0xb1)
	candidates := make([]domain.RescueCandidate, 0, 1026)
	wantTokens := make([]common.Address, 0, 1024)
	for index := uint64(1); index <= 1025; index++ {
		candidate := testIndexedLogCandidate(options, 200, hash, index)
		candidates = append(candidates, candidate)
		if index <= 1024 {
			wantTokens = append(wantTokens, candidate.Token.Address)
		}
	}
	native := testBlockCandidate(options, 200, hash)
	candidates = append(candidates, native)
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 200, BlockHash: hash},
		ParentHash: testHash(0xb0),
		Candidates: candidates,
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("canonical seal at discovery limit: %v", err)
	}
	assertCheckpoint(t, store, true, block.Checkpoint)
	assertDiscoveredTokens(t, store, options.Network, wantTokens)
	if overflowed, err := store.DiscoveryOverflowed(ctx, options.Network); err != nil || !overflowed {
		t.Fatalf("DiscoveryOverflowed = (%v, %v), want true", overflowed, err)
	}

	configuredToken := testIndexedToken(2000)
	configured := domain.NewTokenReconciliationCandidate(options.Network, options.Source, configuredToken, 1, 1)
	if result, err := store.Put(ctx, configured); err != nil || result != PutInserted {
		t.Fatalf("configured token work after overflow = (%v, %v), want inserted", result, err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	if overflowed, err := store.DiscoveryOverflowed(ctx, options.Network); err != nil || !overflowed {
		t.Fatalf("DiscoveryOverflowed after reopen = (%v, %v), want true", overflowed, err)
	}
	assertDiscoveredTokens(t, store, options.Network, wantTokens)
}

func TestBoltStoreConcurrentDuplicateIsRaceSafe(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	candidate := testBlockCandidate(options, 9, testHash(9))

	const workers = 32
	results := make(chan PutResult, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.Put(ctx, candidate)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent Put: %v", err)
	}
	inserted := 0
	for result := range results {
		switch result {
		case PutInserted:
			inserted++
		case PutAlreadyPending:
		default:
			t.Fatalf("unexpected concurrent Put result: %v", result)
		}
	}
	if inserted != 1 {
		t.Fatalf("inserted count = %d, want 1", inserted)
	}
	assertReplay(t, store, candidate)
}

func TestBoltStoreCanonicalCommitProgressesWithFullObservedWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	options.MaxPending = 2
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	canonicalHash := testHash(0x71)
	orphan := testLogCandidate(options, 70, testHash(0xee), 1, 1)
	canonical := testLogCandidate(options, 70, canonicalHash, 2, 2)
	for _, candidate := range []domain.RescueCandidate{orphan, canonical} {
		if _, err := store.PutObserved(ctx, candidate); err != nil {
			t.Fatalf("PutObserved: %v", err)
		}
	}
	native := testBlockCandidate(options, 70, canonicalHash)
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 70, BlockHash: canonicalHash},
		ParentHash: testHash(0x70),
		Candidates: []domain.RescueCandidate{native, canonical},
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatalf("canonical commit deadlocked or failed at provisional bound: %v", err)
	}
	replay, err := store.Replay(ctx, options.Network)
	if err != nil || len(replay) != 2 {
		t.Fatalf("Replay after canonical commit = (%v, %v)", replay, err)
	}
	seen := map[domain.CandidateID]bool{replay[0].ID: true, replay[1].ID: true}
	if !seen[canonical.ID] || !seen[native.ID] {
		t.Fatalf("Replay omitted canonical candidates: %v", replay)
	}
}

func TestBoltStoreBoundsTombstonesAndCanonicalHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)

	for number := uint64(1); number <= canonicalBlockLimit+44; number++ {
		parent := testHash(byte((number-1)%255 + 1))
		if number == 1 {
			parent = testHash(0xaa)
		}
		block := CanonicalBlock{
			Checkpoint: Checkpoint{Network: options.Network, BlockNumber: number, BlockHash: testHash(byte(number%255 + 1))},
			ParentHash: parent,
		}
		if err := store.CommitCanonicalBlock(ctx, block); err != nil {
			t.Fatalf("CommitCanonicalBlock(%d): %v", number, err)
		}
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket(blocksBucket).Stats().KeyN; got > int(canonicalBlockLimit)+1 {
			t.Fatalf("canonical block records = %d, limit %d", got, canonicalBlockLimit+1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for observation := uint64(1); observation <= tombstoneLimit+4; observation++ {
		candidate := domain.NewPeriodicCandidate(options.Network, options.Source, 1, observation)
		if _, err := store.Put(ctx, candidate); err != nil {
			t.Fatalf("Put tombstone %d: %v", observation, err)
		}
		incident := testIncident(candidate, time.Duration(observation))
		if err := store.PutIncident(ctx, incident); err != nil {
			t.Fatalf("PutIncident tombstone %d: %v", observation, err)
		}
		if err := store.Ack(ctx, candidate.ID, incident.ID); err != nil {
			t.Fatalf("Ack tombstone %d: %v", observation, err)
		}
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket(tombstonesBucket).Stats().KeyN; got != tombstoneLimit {
			t.Fatalf("tombstones = %d, want %d", got, tombstoneLimit)
		}
		if got := tx.Bucket(candidatesBucket).Stats().KeyN; got > tombstoneLimit {
			t.Fatalf("acknowledged candidates = %d, limit %d", got, tombstoneLimit)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
}

func openTestStore(t *testing.T, path string, options OpenOptions) *BoltStore {
	t.Helper()
	store, err := Open(path, options)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func reopenTestStore(t *testing.T, store *BoltStore, path string, options OpenOptions) *BoltStore {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	return openTestStore(t, path, options)
}

func testOpenOptions(clock storeClock) OpenOptions {
	var source common.Address
	source[len(source)-1] = 0xa1
	var policy [32]byte
	policy[len(policy)-1] = 0xb2
	return OpenOptions{
		Network:             31337,
		Source:              source,
		PolicyFingerprint:   policy,
		MaxPending:          32,
		MaxDiscoveredTokens: 16,
		Clock:               clock,
	}
}

func testBlockCandidate(options OpenOptions, number uint64, hash common.Hash) domain.RescueCandidate {
	return domain.NewBlockCandidate(options.Network, domain.CandidateNative, options.Source, hash, number)
}

func testLogCandidate(options OpenOptions, number uint64, hash common.Hash, tokenByte, logIndex byte) domain.RescueCandidate {
	var tokenAddress common.Address
	tokenAddress[len(tokenAddress)-1] = tokenByte
	token := domain.Token{Address: tokenAddress, Symbol: "TOK", Decimals: 18}
	return domain.NewLogCandidate(options.Network, options.Source, token, hash, testHash(tokenByte+100), number, uint(logIndex))
}

func testIndexedToken(index uint64) domain.Token {
	var address common.Address
	binary.BigEndian.PutUint64(address[len(address)-8:], index)
	return domain.Token{Address: address, Symbol: "TOK", Decimals: 18}
}

func testIndexedLogCandidate(options OpenOptions, number uint64, hash common.Hash, index uint64) domain.RescueCandidate {
	var txHash common.Hash
	binary.BigEndian.PutUint64(txHash[len(txHash)-8:], index)
	return domain.NewLogCandidate(options.Network, options.Source, testIndexedToken(index), hash, txHash, number, uint(index))
}

func testIncident(candidate domain.RescueCandidate, offset time.Duration) Incident {
	return Incident{
		ID:        domain.NewIncidentID(candidate.ID),
		Candidate: candidate.ID,
		Network:   candidate.Network,
		CreatedAt: time.Date(2026, time.August, 1, 10, 0, 0, 0, time.UTC).Add(offset),
	}
}

func testHash(value byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = value
	return hash
}

func assertReplay(t *testing.T, store *BoltStore, want ...domain.RescueCandidate) {
	t.Helper()
	got, err := store.Replay(context.Background(), store.network)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Replay length = %d, want %d: %v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("Replay[%d] = %v, want %v", index, got[index], want[index])
		}
	}
}

func assertReplaySet(t *testing.T, store *BoltStore, want []domain.RescueCandidate) {
	t.Helper()
	got, err := store.Replay(context.Background(), store.network)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Replay length = %d, want %d", len(got), len(want))
	}
	wanted := make(map[domain.CandidateID]domain.RescueCandidate, len(want))
	for _, candidate := range want {
		wanted[candidate.ID] = candidate
	}
	for _, candidate := range got {
		if expected, ok := wanted[candidate.ID]; !ok || expected != candidate {
			t.Fatalf("Replay contained unexpected candidate: %v", candidate)
		}
	}
}

func candidateData(t *testing.T, store *BoltStore, candidateID domain.CandidateID) []byte {
	t.Helper()
	var result []byte
	if err := store.db.View(func(tx *bolt.Tx) error {
		result = append([]byte(nil), tx.Bucket(candidatesBucket).Get(candidateID[:])...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func candidateWithStatus(t *testing.T, store *BoltStore, candidates []domain.RescueCandidate, status CandidateStatus) domain.RescueCandidate {
	t.Helper()
	for _, candidate := range candidates {
		data := candidateData(t, store, candidate.ID)
		record, err := decodeCandidateRecord(data)
		if err != nil {
			t.Fatalf("decode candidate %s: %v", candidate.ID, err)
		}
		if record.Status == status {
			return candidate
		}
	}
	t.Fatalf("no candidate with status %v", status)
	return domain.RescueCandidate{}
}

func assertStatusCounts(t *testing.T, store *BoltStore, want map[CandidateStatus]int) {
	t.Helper()
	got := make(map[CandidateStatus]int)
	if err := store.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
			record, err := decodeCandidateRecord(value)
			if err != nil {
				return err
			}
			got[record.Status]++
			return nil
		})
	}); err != nil {
		t.Fatalf("read candidate statuses: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate status counts = %v, want %v", got, want)
	}
	for status, count := range want {
		if got[status] != count {
			t.Fatalf("candidate status counts = %v, want %v", got, want)
		}
	}
}

func assertCheckpoint(t *testing.T, store *BoltStore, scan bool, want Checkpoint) {
	t.Helper()
	var (
		got   Checkpoint
		found bool
		err   error
	)
	if scan {
		got, found, err = store.LoadScanCursor(context.Background(), store.network)
	} else {
		got, found, err = store.LoadCheckpoint(context.Background(), store.network)
	}
	wantFound := want != (Checkpoint{})
	if err != nil || found != wantFound || got != want {
		t.Fatalf("checkpoint(scan=%v) = (%v, %v, %v), want (%v, %v, nil)", scan, got, found, err, want, wantFound)
	}
}

func assertDiscoveredTokens(t *testing.T, store *BoltStore, network domain.NetworkID, want []common.Address) {
	t.Helper()
	got, err := store.DiscoveredTokens(context.Background(), network)
	if err != nil {
		t.Fatalf("DiscoveredTokens: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("DiscoveredTokens length = %d, want %d: %v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("DiscoveredTokens[%d] = %v, want %v", index, got[index], want[index])
		}
	}
}

type fakeStoreClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeStoreClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeStoreClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}
