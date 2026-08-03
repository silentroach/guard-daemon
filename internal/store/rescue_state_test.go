package store

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	bolt "go.etcd.io/bbolt"
)

func TestBoltStoreMigratesV1WithoutStateLoss(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	baseline := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 40, BlockHash: testHash(40)},
		ParentHash: testHash(39),
	}
	if err := store.CommitCanonicalBlock(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	candidate := testBlockCandidate(options, 41, testHash(41))
	block := CanonicalBlock{
		Checkpoint: Checkpoint{Network: options.Network, BlockNumber: 41, BlockHash: candidate.BlockHash},
		ParentHash: baseline.BlockHash,
		Candidates: []domain.RescueCandidate{candidate},
	}
	if err := store.CommitCanonicalBlock(ctx, block); err != nil {
		t.Fatal(err)
	}
	incident := testIncident(candidate, 1)
	if err := store.PutIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	wantCandidateRecord := candidateData(t, store, candidate.ID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	mutateStore(t, path, func(tx *bolt.Tx) error {
		return downgradeStoreToV1(tx)
	})

	store = openTestStore(t, path, options)
	defer store.Close()
	assertReplay(t, store, candidate)
	assertCheckpoint(t, store, true, block.Checkpoint)
	assertCheckpoint(t, store, false, baseline.Checkpoint)
	gotIncident, found, err := store.IncidentByCandidate(ctx, candidate.ID)
	if err != nil || !found || gotIncident != incident {
		t.Fatalf("IncidentByCandidate after migration = (%v, %v, %v), want (%v, true, nil)", gotIncident, found, err, incident)
	}
	if got := candidateData(t, store, candidate.ID); !bytes.Equal(got, wantCandidateRecord) {
		t.Fatal("candidate record changed during migration")
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(tx.Bucket(metaBucket).Get(schemaKey), encodeUint32(schemaVersion)) ||
			tx.Bucket(rescueStateBucket) == nil || tx.Bucket(leasesBucket) == nil ||
			!bytes.Equal(tx.Bucket(metaBucket).Get(sponsorKey), options.Sponsor[:]) ||
			!bytes.Equal(tx.Bucket(metaBucket).Get(destinationKey), options.Destination[:]) ||
			!bytes.Equal(tx.Bucket(metaBucket).Get(rescuerKey), options.Rescuer[:]) ||
			!bytes.Equal(tx.Bucket(metaBucket).Get(nonceFloorKey), encodeUint64(0)) {
			t.Fatal("v1 database was not upgraded to the exact v2 schema")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBoltStoreV1MigrationRejectsCorruptionAtomically(t *testing.T) {
	t.Parallel()

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
	mutateStore(t, path, func(tx *bolt.Tx) error {
		if err := downgradeStoreToV1(tx); err != nil {
			return err
		}
		return tx.Bucket(candidatesBucket).Put(candidate.ID[:], []byte{recordVersion})
	})

	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("corrupt v1 database migrated")
	}
	raw, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(tx.Bucket(metaBucket).Get(schemaKey), encodeUint32(schemaVersionV1)) ||
			tx.Bucket(rescueStateBucket) != nil || tx.Bucket(leasesBucket) != nil || tx.Bucket(metaBucket).Get(sponsorKey) != nil ||
			tx.Bucket(metaBucket).Get(destinationKey) != nil || tx.Bucket(metaBucket).Get(rescuerKey) != nil ||
			tx.Bucket(metaBucket).Get(nonceFloorKey) != nil {
			t.Fatal("failed migration partially modified the v1 database")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoltStoreRescueIncidentSurvivesAmbiguousRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	incident := testRescueIncident(options, 1, true)
	stored, err := store.PutRescueIncident(ctx, incident)
	if err != nil || !sameRescueIncident(stored, incident) {
		t.Fatalf("PutRescueIncident failed: found=%v error=%v", sameRescueIncident(stored, incident), err)
	}

	duplicate := incident
	duplicate.CreatedAt = duplicate.CreatedAt.Add(time.Minute)
	duplicate.UpdatedAt = duplicate.UpdatedAt.Add(time.Minute)
	stored, err = store.PutRescueIncident(ctx, duplicate)
	if err != nil || !sameRescueIncident(stored, incident) {
		t.Fatalf("idempotent PutRescueIncident failed: original=%v error=%v", sameRescueIncident(stored, incident), err)
	}
	conflict := incident
	conflict.Policy.MaxAttempts++
	if _, err := store.PutRescueIncident(ctx, conflict); !errors.Is(err, errConflict) {
		t.Fatalf("policy conflict error = %v, want errConflict", err)
	}

	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	signed := prepared
	signed = signTestRescueIncident(t, signed)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	ambiguous := signed
	ambiguous.Status = RescueAmbiguous
	ambiguous.LastCode = "receipt-timeout"
	ambiguous.UpdatedAt = ambiguous.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}

	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	got, found, err := store.RescueIncident(ctx, ambiguous.ID)
	if err != nil || !found || !sameRescueIncident(got, ambiguous) {
		t.Fatalf("RescueIncident after restart mismatch: found=%v equal=%v error=%v", found, sameRescueIncident(got, ambiguous), err)
	}
	if got.TxHash == (common.Hash{}) {
		t.Fatal("ambiguous transaction lost its hash")
	}
}

func TestBoltStoreRescueIncidentsAreDeterministicallySorted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incidents := []RescueIncident{
		testRescueIncident(options, 3, true),
		testRescueIncident(options, 1, true),
		testRescueIncident(options, 2, false),
	}
	for _, incident := range incidents {
		if _, err := store.PutRescueIncident(ctx, incident); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(incidents, func(i, j int) bool { return bytes.Compare(incidents[i].ID[:], incidents[j].ID[:]) < 0 })
	got, err := store.RescueIncidents(ctx, options.Network)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(incidents) {
		t.Fatalf("RescueIncidents length = %d, want %d", len(got), len(incidents))
	}
	for index := range incidents {
		if !sameRescueIncident(got[index], incidents[index]) {
			t.Fatalf("RescueIncidents[%d] mismatch", index)
		}
	}
}

func TestBoltStoreRescueIncidentRejectsBackwardAndTerminalTransitions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incident := testRescueIncident(options, 1, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateRescueIncident(ctx, incident); !errors.Is(err, errConflict) {
		t.Fatalf("Prepared -> Pending error = %v, want errConflict", err)
	}
	signed := signTestRescueIncident(t, prepared)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	backward := prepared
	backward.UpdatedAt = signed.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, backward); !errors.Is(err, errConflict) {
		t.Fatalf("Signed -> Prepared error = %v, want errConflict", err)
	}
	broadcast := signed
	broadcast.Status = RescueBroadcast
	broadcast.UpdatedAt = broadcast.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, broadcast); err != nil {
		t.Fatal(err)
	}
	success := broadcast
	success.Status = RescueTrustedSuccess
	success.SignedTransaction = nil
	success.ReconcileUntil = time.Time{}
	success.UpdatedAt = success.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, success); err != nil {
		t.Fatal(err)
	}
	afterTerminal := broadcast
	afterTerminal.Status = RescueAmbiguous
	afterTerminal.LastCode = "late-reconciliation"
	afterTerminal.UpdatedAt = success.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, afterTerminal); !errors.Is(err, errConflict) {
		t.Fatalf("terminal transition error = %v, want errConflict", err)
	}
}

func TestBoltStoreRescueIncidentAllowsBoundedRetryToPrepared(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incident := testRescueIncident(options, 1, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	retryable := prepared
	retryable.Status = RescueRetryable
	retryable.LastCode = "fee-read"
	retryable.UpdatedAt = retryable.UpdatedAt.Add(time.Second)
	retryable.RetryAt = retryable.UpdatedAt.Add(retryable.Policy.RetryDelay)
	invalidPreparationFailure := retryable
	invalidPreparationFailure.TxHash = testHash(0xee)
	if err := store.UpdateRescueIncident(ctx, invalidPreparationFailure); !errors.Is(err, errInvalidInput) {
		t.Fatalf("Prepared -> Retryable with new TxHash error = %v, want errInvalidInput", err)
	}
	if err := store.UpdateRescueIncident(ctx, retryable); err != nil {
		t.Fatalf("Prepared -> Retryable: %v", err)
	}
	nextAttempt := retryable
	nextAttempt.Status = RescuePrepared
	nextAttempt.Attempts++
	nextAttempt.SponsorNonce = 70
	nextAttempt.SourceNonce = 80
	nextAttempt.SnapshotBlockNumber = 200
	nextAttempt.SnapshotBlockHash = testHash(0xc2)
	nextAttempt.SourceBefore = [32]byte{}
	nextAttempt.SourceBefore[len(nextAttempt.SourceBefore)-1] = 30
	nextAttempt.DestinationBefore = [32]byte{}
	nextAttempt.DestinationBefore[len(nextAttempt.DestinationBefore)-1] = 40
	nextAttempt.RetryAt = time.Time{}
	nextAttempt.LastCode = ""
	nextAttempt.UpdatedAt = retryable.RetryAt
	early := nextAttempt
	early.UpdatedAt = retryable.RetryAt.Add(-time.Nanosecond)
	if err := store.UpdateRescueIncident(ctx, early); !errors.Is(err, errConflict) {
		t.Fatalf("Retryable -> Prepared before RetryAt error = %v, want errConflict", err)
	}
	skippedAttempt := nextAttempt
	skippedAttempt.Attempts++
	if err := store.UpdateRescueIncident(ctx, skippedAttempt); !errors.Is(err, errConflict) {
		t.Fatalf("Retryable -> Prepared attempts jump error = %v, want errConflict", err)
	}
	if err := store.UpdateRescueIncident(ctx, nextAttempt); err != nil {
		t.Fatalf("Retryable -> Prepared: %v", err)
	}
	got, found, err := store.RescueIncident(ctx, nextAttempt.ID)
	if err != nil || !found || !sameRescueIncident(got, nextAttempt) {
		t.Fatalf("fresh retry preparation mismatch: found=%v equal=%v error=%v", found, sameRescueIncident(got, nextAttempt), err)
	}
}

func TestBoltStoreRescueIncidentRejectsReSigningHashedRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incident := testRescueIncident(options, 2, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	signed := signTestRescueIncident(t, prepared)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	retryable := signed
	retryable.Status = RescueRetryable
	retryable.SignedTransaction = nil
	retryable.TxHash = common.Hash{}
	retryable.ReconcileUntil = time.Time{}
	retryable.LastCode = "definitive-send-error"
	retryable.UpdatedAt = retryable.UpdatedAt.Add(time.Second)
	retryable.RetryAt = retryable.UpdatedAt.Add(retryable.Policy.RetryDelay)
	if err := store.UpdateRescueIncident(ctx, retryable); !errors.Is(err, errConflict) {
		t.Fatalf("Signed -> Retryable error = %v, want errConflict", err)
	}
}

func TestBoltStoreSignedTransactionRoundTripOwnsPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	incident := testRescueIncident(options, 4, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	signed := signTestRescueIncident(t, prepared)
	wantPayload := append([]byte(nil), signed.SignedTransaction...)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	signed.SignedTransaction[0] ^= 0xff
	stored, found, err := store.RescueIncident(ctx, signed.ID)
	if err != nil || !found || !bytes.Equal(stored.SignedTransaction, wantPayload) {
		t.Fatalf("stored payload ownership failed: found=%v error=%v", found, err)
	}
	stored.SignedTransaction[0] ^= 0xff
	stored, found, err = store.RescueIncident(ctx, signed.ID)
	if err != nil || !found || !bytes.Equal(stored.SignedTransaction, wantPayload) {
		t.Fatalf("returned payload ownership failed: found=%v error=%v", found, err)
	}
	listed, err := store.RescueIncidents(ctx, options.Network)
	if err != nil || len(listed) != 1 {
		t.Fatalf("RescueIncidents payload read failed: count=%d error=%v", len(listed), err)
	}
	listed[0].SignedTransaction[0] ^= 0xff
	stored, found, err = store.RescueIncident(ctx, signed.ID)
	if err != nil || !found || !bytes.Equal(stored.SignedTransaction, wantPayload) {
		t.Fatalf("listed payload ownership failed: found=%v error=%v", found, err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	stored, found, err = store.RescueIncident(ctx, signed.ID)
	if err != nil || !found || !bytes.Equal(stored.SignedTransaction, wantPayload) || stored.TxHash != storedTransactionHash(t, wantPayload) {
		t.Fatalf("signed payload did not survive reopen: found=%v error=%v", found, err)
	}
}

func TestBoltStoreRejectsInvalidSignedTransactionIdentity(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, *RescueIncident){
		"hash": func(_ *testing.T, incident *RescueIncident) {
			incident.TxHash[0] ^= 0xff
		},
		"sponsor nonce": func(_ *testing.T, incident *RescueIncident) {
			incident.SponsorNonce++
		},
		"source nonce": func(_ *testing.T, incident *RescueIncident) {
			incident.SourceNonce++
		},
		"transaction type": func(t *testing.T, incident *RescueIncident) {
			incident.SignedTransaction, incident.TxHash = testSignedDynamicFeeTransaction(t, incident.Network, incident.SponsorNonce)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			options := testOpenOptions(nil)
			store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
			defer store.Close()
			incident := testRescueIncident(options, 5, true)
			if _, err := store.PutRescueIncident(ctx, incident); err != nil {
				t.Fatal(err)
			}
			prepared := prepareRescueIncident(incident)
			if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
				t.Fatal(err)
			}
			signed := signTestRescueIncident(t, prepared)
			mutate(t, &signed)
			if err := store.UpdateRescueIncident(ctx, signed); !errors.Is(err, errInvalidInput) {
				t.Fatalf("invalid signed transaction error = %v, want errInvalidInput", err)
			}
		})
	}
}

func TestBoltStoreSignedTransactionCorruptionFailsClosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	incident := testRescueIncident(options, 6, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	signed := signTestRescueIncident(t, prepared)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(rescueStateBucket)
		record := append([]byte(nil), bucket.Get(signed.ID[:])...)
		offset := bytes.Index(record, signed.SignedTransaction)
		if offset < 0 {
			return errors.New("signed payload not found in local record")
		}
		record[offset] ^= 0xff
		return bucket.Put(signed.ID[:], record)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RescueIncident(ctx, signed.ID); !errors.Is(err, errCorrupt) {
		t.Fatalf("corrupt signed payload error = %v, want errCorrupt", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("database with corrupt signed payload reopened")
	}
}

func TestBoltStoreAtomicPreparationFailureAndRetryFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incident := testRescueIncident(options, 7, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	failure := prepareRescueIncident(incident)
	failure.Status = RescueRetryable
	failure.LastCode = "snapshot-read"
	failure.RetryAt = failure.UpdatedAt.Add(failure.Policy.RetryDelay)
	if err := store.UpdateRescueIncident(ctx, failure); err != nil {
		t.Fatalf("atomic Pending -> Retryable: %v", err)
	}
	arbitraryMutation := failure
	arbitraryMutation.LastCode = "different"
	arbitraryMutation.UpdatedAt = arbitraryMutation.UpdatedAt.Add(time.Second)
	arbitraryMutation.RetryAt = arbitraryMutation.RetryAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, arbitraryMutation); !errors.Is(err, errConflict) {
		t.Fatalf("arbitrary Retryable mutation error = %v, want errConflict", err)
	}
	nextFailure := failure
	nextFailure.Attempts++
	nextFailure.SponsorNonce++
	nextFailure.SourceNonce++
	nextFailure.SnapshotBlockNumber++
	nextFailure.SnapshotBlockHash = testHash(0xd2)
	nextFailure.LastCode = "fee-read"
	nextFailure.UpdatedAt = failure.RetryAt
	nextFailure.RetryAt = nextFailure.UpdatedAt.Add(nextFailure.Policy.RetryDelay)
	if err := store.UpdateRescueIncident(ctx, nextFailure); err != nil {
		t.Fatalf("bounded Retryable -> Retryable: %v", err)
	}
}

func TestBoltStoreAmbiguousReconciliationCannotExhaust(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	options := testOpenOptions(nil)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	incident := testRescueIncident(options, 8, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	prepared := prepareRescueIncident(incident)
	if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	signed := signTestRescueIncident(t, prepared)
	if err := store.UpdateRescueIncident(ctx, signed); err != nil {
		t.Fatal(err)
	}
	ambiguous := signed
	ambiguous.Status = RescueAmbiguous
	ambiguous.LastCode = "receipt-quorum"
	ambiguous.UpdatedAt = ambiguous.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	exhausted := ambiguous
	exhausted.Status = RescueExhausted
	exhausted.TxHash = common.Hash{}
	exhausted.SignedTransaction = nil
	exhausted.ReconcileUntil = time.Time{}
	exhausted.LastCode = "quorum-timeout"
	exhausted.UpdatedAt = exhausted.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, exhausted); !errors.Is(err, errConflict) {
		t.Fatalf("Ambiguous -> Exhausted error = %v, want errConflict", err)
	}
	extended := ambiguous
	extended.LastCode = "receipt-still-unknown"
	extended.UpdatedAt = extended.UpdatedAt.Add(time.Minute)
	extended.ReconcileUntil = extended.ReconcileUntil.Add(time.Minute)
	extended.RetryAt = extended.UpdatedAt.Add(30 * time.Second)
	if err := store.UpdateRescueIncident(ctx, extended); err != nil {
		t.Fatalf("bounded Ambiguous -> Ambiguous: %v", err)
	}
	rebroadcast := extended
	rebroadcast.Status = RescueBroadcast
	rebroadcast.LastCode = ""
	rebroadcast.RetryAt = time.Time{}
	rebroadcast.UpdatedAt = rebroadcast.UpdatedAt.Add(time.Second)
	if err := store.UpdateRescueIncident(ctx, rebroadcast); err != nil {
		t.Fatalf("exact Ambiguous -> Broadcast rebroadcast: %v", err)
	}
}

func TestBoltStoreNonceFloorIsMonotonicAndDurable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	if floor, err := store.NonceFloor(ctx); err != nil || floor != 0 {
		t.Fatalf("initial NonceFloor = (%d, %v), want (0, nil)", floor, err)
	}
	if err := store.RaiseNonceFloor(ctx, 12); err != nil {
		t.Fatal(err)
	}
	if err := store.RaiseNonceFloor(ctx, 5); err != nil {
		t.Fatal(err)
	}
	store = reopenTestStore(t, store, path, options)
	if floor, err := store.NonceFloor(ctx); err != nil || floor != 12 {
		t.Fatalf("durable NonceFloor = (%d, %v), want (12, nil)", floor, err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(nonceFloorKey, []byte{1})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NonceFloor(ctx); !errors.Is(err, errCorrupt) {
		t.Fatalf("corrupt NonceFloor error = %v, want errCorrupt", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("database with corrupt nonce floor reopened")
	}
}

func TestBoltStorePrunesOnlyOldTerminalIncidents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	terminalUpdatedAt := time.Date(2026, time.August, 2, 17, 0, 0, 0, time.UTC)
	terminals := make([]RescueIncident, 0, 4)
	for generation := byte(10); generation < 14; generation++ {
		incident := testRescueIncident(options, generation, true)
		if _, err := store.PutRescueIncident(ctx, incident); err != nil {
			t.Fatal(err)
		}
		prepared := prepareRescueIncident(incident)
		prepared.UpdatedAt = terminalUpdatedAt
		if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
			t.Fatal(err)
		}
		terminal := prepared
		terminal.Status = RescueFailed
		terminal.LastCode = "pre-sign-terminal"
		terminal.UpdatedAt = terminalUpdatedAt.Add(time.Second)
		if err := store.UpdateRescueIncident(ctx, terminal); err != nil {
			t.Fatal(err)
		}
		terminals = append(terminals, terminal)
	}
	unresolved := []RescueIncident{testRescueIncident(options, 14, true), testRescueIncident(options, 15, true)}
	for index, incident := range unresolved {
		if _, err := store.PutRescueIncident(ctx, incident); err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			retryable := prepareRescueIncident(incident)
			retryable.Status = RescueRetryable
			retryable.LastCode = "preparation"
			retryable.RetryAt = retryable.UpdatedAt.Add(retryable.Policy.RetryDelay)
			if err := store.UpdateRescueIncident(ctx, retryable); err != nil {
				t.Fatal(err)
			}
			unresolved[index] = retryable
		}
	}
	preparedIncident := testRescueIncident(options, 16, true)
	if _, err := store.PutRescueIncident(ctx, preparedIncident); err != nil {
		t.Fatal(err)
	}
	preparedIncident = prepareRescueIncident(preparedIncident)
	if err := store.UpdateRescueIncident(ctx, preparedIncident); err != nil {
		t.Fatal(err)
	}
	unresolved = append(unresolved, preparedIncident)
	for generation, status := range map[byte]RescueStatus{17: RescueSigned, 18: RescueBroadcast, 19: RescueAmbiguous} {
		incident := testRescueIncident(options, generation, true)
		if _, err := store.PutRescueIncident(ctx, incident); err != nil {
			t.Fatal(err)
		}
		prepared := prepareRescueIncident(incident)
		if err := store.UpdateRescueIncident(ctx, prepared); err != nil {
			t.Fatal(err)
		}
		signed := signTestRescueIncident(t, prepared)
		if err := store.UpdateRescueIncident(ctx, signed); err != nil {
			t.Fatal(err)
		}
		current := signed
		if status == RescueBroadcast || status == RescueAmbiguous {
			current.Status = RescueBroadcast
			current.UpdatedAt = current.UpdatedAt.Add(time.Second)
			if err := store.UpdateRescueIncident(ctx, current); err != nil {
				t.Fatal(err)
			}
		}
		if status == RescueAmbiguous {
			current.Status = RescueAmbiguous
			current.LastCode = "receipt-unknown"
			current.UpdatedAt = current.UpdatedAt.Add(time.Second)
			if err := store.UpdateRescueIncident(ctx, current); err != nil {
				t.Fatal(err)
			}
		}
		unresolved = append(unresolved, current)
	}
	if err := store.RaiseNonceFloor(ctx, 44); err != nil {
		t.Fatal(err)
	}
	sort.Slice(terminals, func(i, j int) bool { return bytes.Compare(terminals[i].ID[:], terminals[j].ID[:]) < 0 })
	if err := store.PruneRescueIncidents(ctx, 2); err != nil {
		t.Fatal(err)
	}
	assertRetainedRescueIncidents(t, store, append(terminals[2:], unresolved...))
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	assertRetainedRescueIncidents(t, store, append(terminals[2:], unresolved...))
	if floor, err := store.NonceFloor(ctx); err != nil || floor != 44 {
		t.Fatalf("NonceFloor after prune/reopen = (%d, %v), want (44, nil)", floor, err)
	}
}

func TestBoltStoreRescueIncidentCorruptionFailsClosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := testOpenOptions(nil)
	store := openTestStore(t, path, options)
	incident := testRescueIncident(options, 1, true)
	if _, err := store.PutRescueIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(rescueStateBucket).Put(incident.ID[:], []byte{rescueRecordVersion, byte(RescuePending)})
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RescueIncident(ctx, incident.ID); !errors.Is(err, errCorrupt) {
		t.Fatalf("corrupt RescueIncident error = %v, want errCorrupt", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("database with corrupt rescue record reopened")
	}
}

func testRescueIncident(options OpenOptions, generation byte, trusted bool) RescueIncident {
	var asset common.Address
	asset[len(asset)-1] = generation
	candidate := domain.NewTokenReconciliationCandidate(options.Network, options.Source, domain.Token{Address: asset}, uint64(generation), uint64(generation))
	createdAt := time.Date(2026, time.August, 2, 15, int(generation), 0, 0, time.UTC)
	return RescueIncident{
		ID:         domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, asset),
		Parent:     domain.NewIncidentID(candidate.ID),
		Candidate:  candidate.ID,
		Network:    options.Network,
		Kind:       domain.CandidateToken,
		Asset:      asset,
		Generation: uint64(generation),
		Trusted:    trusted,
		Policy: RescuePolicySnapshot{
			MaxAttempts:     3,
			RetryDelay:      time.Minute,
			FinalityTimeout: 5 * time.Minute,
		},
		Status:    RescuePending,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func downgradeStoreToV1(tx *bolt.Tx) error {
	if err := tx.DeleteBucket(rescueStateBucket); err != nil {
		return err
	}
	if err := tx.DeleteBucket(leasesBucket); err != nil {
		return err
	}
	meta := tx.Bucket(metaBucket)
	for _, key := range [][]byte{sponsorKey, destinationKey, rescuerKey, nonceFloorKey} {
		if err := meta.Delete(key); err != nil {
			return err
		}
	}
	return meta.Put(schemaKey, encodeUint32(schemaVersionV1))
}

func prepareRescueIncident(incident RescueIncident) RescueIncident {
	incident.Status = RescuePrepared
	incident.Attempts = 1
	incident.SponsorNonce = 7
	incident.SourceNonce = 8
	incident.SnapshotBlockNumber = 100
	incident.SnapshotBlockHash = testHash(0xc1)
	incident.SourceBefore[len(incident.SourceBefore)-1] = 10
	incident.DestinationBefore[len(incident.DestinationBefore)-1] = 20
	incident.UpdatedAt = incident.UpdatedAt.Add(time.Second)
	return incident
}

func signTestRescueIncident(t *testing.T, incident RescueIncident) RescueIncident {
	t.Helper()
	incident.Status = RescueSigned
	incident.UpdatedAt = incident.UpdatedAt.Add(time.Second)
	incident.SignedTransaction, incident.TxHash = testSignedSetCodeTransaction(t, incident.Network, incident.SponsorNonce, incident.SourceNonce)
	incident.ReconcileUntil = incident.UpdatedAt.Add(incident.Policy.FinalityTimeout)
	return incident
}

func testSignedSetCodeTransaction(t *testing.T, network domain.NetworkID, sponsorNonce, sourceNonce uint64) ([]byte, common.Hash) {
	t.Helper()
	chainID := big.NewInt(int64(network))
	chainU256, overflow := uint256.FromBig(chainID)
	if overflow {
		t.Fatal("local chain ID does not fit uint256")
	}
	sourceKey := testPrivateKey(t, 1)
	sponsorKey := testPrivateKey(t, 2)
	var rescuer common.Address
	rescuer[len(rescuer)-1] = 0x31
	authorization, err := types.SignSetCode(sourceKey, types.SetCodeAuthorization{
		ChainID: *chainU256,
		Address: rescuer,
		Nonce:   sourceNonce,
	})
	if err != nil {
		t.Fatalf("sign local authorization: %v", err)
	}
	var target common.Address
	target[len(target)-1] = 0x32
	transaction := types.NewTx(&types.SetCodeTx{
		ChainID:   chainU256,
		Nonce:     sponsorNonce,
		GasTipCap: uint256.NewInt(1),
		GasFeeCap: uint256.NewInt(2),
		Gas:       100_000,
		To:        target,
		Data:      []byte{0xca, 0xfe},
		AuthList:  []types.SetCodeAuthorization{authorization},
	})
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(chainID), sponsorKey)
	if err != nil {
		t.Fatalf("sign local transaction: %v", err)
	}
	payload, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal local transaction: %v", err)
	}
	return payload, signed.Hash()
}

func testSignedDynamicFeeTransaction(t *testing.T, network domain.NetworkID, nonce uint64) ([]byte, common.Hash) {
	t.Helper()
	chainID := big.NewInt(int64(network))
	var target common.Address
	target[len(target)-1] = 0x41
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       100_000,
		To:        &target,
		Data:      []byte{0xca, 0xfe},
	})
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(chainID), testPrivateKey(t, 2))
	if err != nil {
		t.Fatalf("sign local dynamic fee transaction: %v", err)
	}
	payload, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal local dynamic fee transaction: %v", err)
	}
	return payload, signed.Hash()
}

func storedTransactionHash(t *testing.T, payload []byte) common.Hash {
	t.Helper()
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(payload); err != nil {
		t.Fatalf("decode stored local transaction: %v", err)
	}
	return transaction.Hash()
}

func assertRetainedRescueIncidents(t *testing.T, store *BoltStore, want []RescueIncident) {
	t.Helper()
	got, err := store.RescueIncidents(context.Background(), store.network)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("retained incident count = %d, want %d", len(got), len(want))
	}
	wanted := make(map[domain.IncidentID]RescueIncident, len(want))
	for _, incident := range want {
		wanted[incident.ID] = incident
	}
	for _, incident := range got {
		expected, ok := wanted[incident.ID]
		if !ok || !sameRescueIncident(incident, expected) {
			t.Fatalf("unexpected retained incident %s", incident.ID)
		}
	}
}

func testPrivateKey(t *testing.T, marker byte) *ecdsa.PrivateKey {
	t.Helper()
	encoded := make([]byte, 32)
	encoded[len(encoded)-1] = marker
	key, err := crypto.ToECDSA(encoded)
	if err != nil {
		t.Fatalf("construct deterministic local key: %v", err)
	}
	return key
}
