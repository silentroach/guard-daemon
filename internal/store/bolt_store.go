package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

const (
	schemaVersionV1       = uint32(1)
	schemaVersionV2       = uint32(2)
	schemaVersion         = uint32(3)
	openTimeout           = 250 * time.Millisecond
	maxConfiguredBound    = uint32(1_000_000)
	minJournalCapacity    = uint64(100_000)
	journalCapacityFactor = uint64(128)
	maxJournalCapacity    = uint64(100_000_000)
	maxDelayedWaitSlice   = time.Second
	tombstoneLimit        = 128
	canonicalBlockLimit   = uint64(256)
	unconfirmedBlockLimit = uint64(256)
)

var (
	metaBucket        = []byte("meta")
	candidatesBucket  = []byte("candidates")
	pendingBucket     = []byte("pending")
	readyOrderBucket  = []byte("ready-order")
	blockIndexBucket  = []byte("candidate-blocks")
	incidentsBucket   = []byte("incidents")
	blocksBucket      = []byte("blocks")
	discoveredBucket  = []byte("discovered")
	tombstonesBucket  = []byte("tombstones")
	rescueStateBucket = []byte("rescue-incidents")
	leasesBucket      = []byte("leases")

	schemaKey            = []byte("schema")
	networkKey           = []byte("network")
	sourceKey            = []byte("source")
	sponsorKey           = []byte("sponsor")
	destinationKey       = []byte("destination")
	rescuerKey           = []byte("rescuer")
	policyKey            = []byte("policy")
	maxPendingKey        = []byte("max-pending")
	maxDiscoveredKey     = []byte("max-discovered")
	baselineKey          = []byte("baseline")
	scanCursorKey        = []byte("scan-cursor")
	checkpointKey        = []byte("checkpoint")
	discoveryOverflowKey = []byte("discovery-overflow")
	promotionTurnKey     = []byte("promotion-turn")
	nonceFloorKey        = []byte("nonce-floor")

	errInvalidOptions = errors.New("хранилище: некорректные параметры")
	errOpenFailed     = errors.New("хранилище: не удалось открыть state")
	errBinding        = errors.New("хранилище: сохранённая привязка не совпадает")
	errCorrupt        = errors.New("хранилище: state повреждён")
	errClosed         = errors.New("хранилище: state закрыт")
	errInvalidInput   = errors.New("хранилище: некорректные входные данные")
	errConflict       = errors.New("хранилище: конфликт перехода состояния")
	errJournalFull    = errors.New("хранилище: достигнут предел durable journal")

	// ErrObservationSaturated means an untrusted provisional hint was dropped.
	// Canonical scanning remains authoritative and will recover the candidate.
	ErrObservationSaturated = errors.New("хранилище: достигнут предел provisional observations")
)

type storeClock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// BoltStore persists watcher handoff state in one ACID bbolt database.
type BoltStore struct {
	db                  *bolt.DB
	network             domain.NetworkID
	source              common.Address
	sponsor             common.Address
	destination         common.Address
	rescuer             common.Address
	maxPending          uint32
	journalCapacity     uint32
	maxDiscoveredTokens uint32
	policyFingerprint   [32]byte
	clock               storeClock

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	ops    sync.WaitGroup

	wakeMu sync.Mutex
	wake   chan struct{}

	leaseMu   sync.Mutex
	leaseLoss map[leaseIdentity]chan struct{}
}

var _ HandoffStore = (*BoltStore)(nil)

// Open opens or initializes a fail-closed, configuration-bound handoff store.
func Open(path string, options OpenOptions) (*BoltStore, error) {
	if path == "" || options.Network <= 0 || options.Source == (common.Address{}) || options.Sponsor == (common.Address{}) ||
		options.Destination == (common.Address{}) || options.Rescuer == (common.Address{}) ||
		options.Source == options.Sponsor || options.Source == options.Destination || options.Sponsor == options.Destination ||
		options.MaxPending == 0 || options.MaxPending > maxConfiguredBound ||
		options.MaxDiscoveredTokens == 0 || options.MaxDiscoveredTokens > maxConfiguredBound {
		return nil, errInvalidOptions
	}
	parent := filepath.Dir(path)
	if err := ensureParent(parent); err != nil {
		return nil, errOpenFailed
	}
	created := false
	file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case createErr == nil:
		created = true
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return nil, errOpenFailed
		}
	case errors.Is(createErr, os.ErrExist):
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return nil, errOpenFailed
		}
		if info.Size() == 0 {
			return nil, errCorrupt
		}
	default:
		return nil, errOpenFailed
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openTimeout, NoSync: false})
	if err != nil {
		if created {
			_ = os.Remove(path)
		}
		return nil, errOpenFailed
	}
	fail := func(public error) (*BoltStore, error) {
		_ = db.Close()
		if created {
			_ = os.Remove(path)
		}
		return nil, public
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fail(errOpenFailed)
	}

	clock := storeClock(wallClock{})
	if options.Clock != nil {
		clock = options.Clock
	}
	store := &BoltStore{
		db:                  db,
		network:             options.Network,
		source:              options.Source,
		sponsor:             options.Sponsor,
		destination:         options.Destination,
		rescuer:             options.Rescuer,
		maxPending:          options.MaxPending,
		journalCapacity:     deriveJournalCapacity(options.MaxPending),
		maxDiscoveredTokens: options.MaxDiscoveredTokens,
		policyFingerprint:   options.PolicyFingerprint,
		clock:               clock,
		wake:                make(chan struct{}),
		done:                make(chan struct{}),
		leaseLoss:           make(map[leaseIdentity]chan struct{}),
	}
	initialized := false
	err = db.Update(func(tx *bolt.Tx) error {
		var initializeErr error
		initialized, initializeErr = store.initializeOrMigrate(tx, options, created)
		return initializeErr
	})
	if err != nil {
		if errors.Is(err, errBinding) {
			return fail(errBinding)
		}
		return fail(errCorrupt)
	}
	if initialized != created {
		return fail(errCorrupt)
	}
	if err := db.View(func(tx *bolt.Tx) error { return store.validateDatabase(tx) }); err != nil {
		return fail(errCorrupt)
	}
	return store, nil
}

func ensureParent(parent string) error {
	info, err := os.Stat(parent)
	switch {
	case err == nil:
		if !info.IsDir() {
			return errInvalidOptions
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	return os.Chmod(parent, 0o700)
}

func deriveJournalCapacity(maxPending uint32) uint32 {
	capacity := uint64(maxPending) * journalCapacityFactor
	if capacity < minJournalCapacity {
		capacity = minJournalCapacity
	}
	minimum := uint64(maxPending) + 1
	if capacity < minimum {
		capacity = minimum
	}
	if capacity > maxJournalCapacity {
		capacity = maxJournalCapacity
	}
	return uint32(capacity)
}

func (store *BoltStore) initializeOrMigrate(tx *bolt.Tx, options OpenOptions, allowInitialize bool) (bool, error) {
	meta := tx.Bucket(metaBucket)
	if meta == nil {
		if !allowInitialize {
			return false, errCorrupt
		}
		key, _ := tx.Cursor().First()
		if key != nil {
			return false, errCorrupt
		}
		var err error
		if meta, err = tx.CreateBucket(metaBucket); err != nil {
			return false, err
		}
		for _, bucket := range [][]byte{candidatesBucket, pendingBucket, readyOrderBucket, blockIndexBucket, incidentsBucket, blocksBucket, discoveredBucket, tombstonesBucket, rescueStateBucket, leasesBucket} {
			if _, err := tx.CreateBucket(bucket); err != nil {
				return false, err
			}
		}
		bindings := []struct {
			key   []byte
			value []byte
		}{
			{schemaKey, encodeUint32(schemaVersion)},
			{networkKey, encodeUint64(uint64(options.Network))},
			{sourceKey, append([]byte(nil), options.Source[:]...)},
			{sponsorKey, append([]byte(nil), options.Sponsor[:]...)},
			{destinationKey, append([]byte(nil), options.Destination[:]...)},
			{rescuerKey, append([]byte(nil), options.Rescuer[:]...)},
			{policyKey, append([]byte(nil), options.PolicyFingerprint[:]...)},
			{maxPendingKey, encodeUint32(options.MaxPending)},
			{maxDiscoveredKey, encodeUint32(options.MaxDiscoveredTokens)},
			{nonceFloorKey, encodeUint64(0)},
		}
		for _, binding := range bindings {
			if err := meta.Put(binding.key, binding.value); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	schema := meta.Get(schemaKey)
	if !bytes.Equal(schema, encodeUint32(schemaVersionV1)) && !bytes.Equal(schema, encodeUint32(schemaVersionV2)) && !bytes.Equal(schema, encodeUint32(schemaVersion)) {
		return false, errBinding
	}
	bindings := []struct {
		key   []byte
		value []byte
	}{
		{networkKey, encodeUint64(uint64(options.Network))},
		{sourceKey, options.Source[:]},
		{policyKey, options.PolicyFingerprint[:]},
		{maxPendingKey, encodeUint32(options.MaxPending)},
		{maxDiscoveredKey, encodeUint32(options.MaxDiscoveredTokens)},
	}
	for _, binding := range bindings {
		if !bytes.Equal(meta.Get(binding.key), binding.value) {
			return false, errBinding
		}
	}
	for _, bucket := range [][]byte{candidatesBucket, pendingBucket, readyOrderBucket, blockIndexBucket, incidentsBucket, blocksBucket, discoveredBucket, tombstonesBucket} {
		if tx.Bucket(bucket) == nil {
			return false, errCorrupt
		}
	}
	if bytes.Equal(schema, encodeUint32(schemaVersionV1)) {
		if err := store.validateDatabaseVersion(tx, schemaVersionV1); err != nil {
			return false, err
		}
		if _, err := tx.CreateBucket(rescueStateBucket); err != nil {
			return false, err
		}
		if _, err := tx.CreateBucket(leasesBucket); err != nil {
			return false, err
		}
		for _, binding := range []struct {
			key   []byte
			value []byte
		}{
			{sponsorKey, options.Sponsor[:]},
			{destinationKey, options.Destination[:]},
			{rescuerKey, options.Rescuer[:]},
			{nonceFloorKey, encodeUint64(0)},
		} {
			if err := meta.Put(binding.key, binding.value); err != nil {
				return false, err
			}
		}
		if err := meta.Put(schemaKey, encodeUint32(schemaVersion)); err != nil {
			return false, err
		}
		return false, nil
	}
	if bytes.Equal(schema, encodeUint32(schemaVersionV2)) {
		if err := store.validateDatabaseVersion(tx, schemaVersionV2); err != nil {
			return false, err
		}
		if err := migrateV2TokenOutcomes(tx); err != nil {
			return false, err
		}
		if err := meta.Put(schemaKey, encodeUint32(schemaVersion)); err != nil {
			return false, err
		}
	}
	for _, bucket := range [][]byte{rescueStateBucket, leasesBucket} {
		if tx.Bucket(bucket) == nil {
			return false, errCorrupt
		}
	}
	for _, binding := range []struct {
		key   []byte
		value []byte
	}{
		{sponsorKey, options.Sponsor[:]},
		{destinationKey, options.Destination[:]},
		{rescuerKey, options.Rescuer[:]},
	} {
		if !bytes.Equal(meta.Get(binding.key), binding.value) {
			return false, errBinding
		}
	}
	return false, nil
}

func (store *BoltStore) Put(ctx context.Context, candidate domain.RescueCandidate) (PutResult, error) {
	if err := store.validateInputCandidate(candidate); err != nil {
		return 0, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()
	for {
		wake := store.wakeSignal()
		var (
			result  PutResult
			changed bool
		)
		err = db.Update(func(tx *bolt.Tx) error {
			if err := contextError(ctx); err != nil {
				return err
			}
			bucket := tx.Bucket(candidatesBucket)
			persisted := bucket.Get(candidate.ID[:])
			if persisted != nil {
				record, decodeErr := decodeCandidateRecord(persisted)
				if decodeErr != nil || !sameCandidateIdentity(record.Candidate, candidate) || !store.matchesCandidate(record.Candidate) {
					return errCorrupt
				}
				if record.Status == CandidateAcknowledged {
					result = PutAlreadyAcknowledged
					return nil
				}
				result = PutAlreadyPending
				if record.Status != CandidateObserved && record.Status != CandidateRemoved {
					return nil
				}
				if err := store.reserveJournal(tx, 1); err != nil {
					return err
				}
				record.Status = CandidateStaged
				record.Candidate = updateCandidateMetadata(record.Candidate, candidate)
				encoded, encodeErr := encodeCandidateRecord(record)
				if encodeErr != nil {
					return errCorrupt
				}
				if err := bucket.Put(candidate.ID[:], encoded); err != nil {
					return err
				}
				changed = true
				return store.promoteCandidates(tx, store.clock.Now())
			}
			if store.candidateAlreadyScanned(tx, candidate) {
				result = PutAlreadyAcknowledged
				return nil
			}
			if err := store.rejectLateCandidate(tx, candidate); err != nil {
				return err
			}
			if err := store.reserveJournal(tx, 1); err != nil {
				return err
			}
			encoded, encodeErr := encodeCandidateRecord(candidateRecord{Status: CandidateStaged, Candidate: candidate})
			if encodeErr != nil {
				return errInvalidInput
			}
			if err := bucket.Put(candidate.ID[:], encoded); err != nil {
				return err
			}
			if err := store.indexCandidateBlock(tx, candidate); err != nil {
				return err
			}
			result = PutInserted
			changed = true
			return store.promoteCandidates(tx, store.clock.Now())
		})
		if errors.Is(err, errJournalFull) {
			if err := store.wait(ctx, wake, 0); err != nil {
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, store.publicError(err)
		}
		if changed {
			store.notify()
		}
		return result, nil
	}
}

func (store *BoltStore) PutObserved(ctx context.Context, candidate domain.RescueCandidate) (PutResult, error) {
	if err := store.validateInputCandidate(candidate); err != nil {
		return 0, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()
	var (
		result  PutResult
		changed bool
	)
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(candidatesBucket)
		persisted := bucket.Get(candidate.ID[:])
		if persisted != nil {
			record, decodeErr := decodeCandidateRecord(persisted)
			if decodeErr != nil || !sameCandidateIdentity(record.Candidate, candidate) || !store.matchesCandidate(record.Candidate) {
				return errCorrupt
			}
			if record.Status == CandidateAcknowledged {
				result = PutAlreadyAcknowledged
				return nil
			}
			result = PutAlreadyPending
			return nil
		}
		if store.candidateAlreadyScanned(tx, candidate) {
			result = PutAlreadyAcknowledged
			return nil
		}
		if err := store.rejectLateCandidate(tx, candidate); err != nil {
			return err
		}
		count, countErr := store.statusCount(tx, CandidateObserved)
		if countErr != nil {
			return countErr
		}
		if count >= store.maxPending {
			return ErrObservationSaturated
		}
		encoded, encodeErr := encodeCandidateRecord(candidateRecord{Status: CandidateObserved, Candidate: candidate})
		if encodeErr != nil {
			return errInvalidInput
		}
		if err := bucket.Put(candidate.ID[:], encoded); err != nil {
			return err
		}
		if err := store.indexCandidateBlock(tx, candidate); err != nil {
			return err
		}
		result = PutInserted
		changed = true
		return nil
	})
	if err != nil {
		return 0, store.publicError(err)
	}
	if changed {
		store.notify()
	}
	return result, nil
}

func (store *BoltStore) MarkRemoved(ctx context.Context, candidateID domain.CandidateID) error {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	changed := false
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(candidatesBucket)
		data := bucket.Get(candidateID[:])
		if data == nil {
			return errConflict
		}
		record, decodeErr := decodeCandidateRecord(data)
		if decodeErr != nil || record.Candidate.ID != candidateID || !store.matchesCandidate(record.Candidate) {
			return errCorrupt
		}
		switch record.Status {
		case CandidateRemoved:
			return nil
		case CandidateObserved:
			record.Status = CandidateRemoved
		case CandidateStaged, CandidateReady, CandidateDelayed, CandidateAcknowledged:
			return errConflict
		default:
			return errCorrupt
		}
		blockIndexKey := candidateBlockIndexKey(record.Candidate)
		if err := bucket.Delete(candidateID[:]); err != nil {
			return err
		}
		if blockIndexKey != nil {
			if err := tx.Bucket(blockIndexBucket).Delete(blockIndexKey); err != nil {
				return err
			}
		}
		if err := tx.Bucket(pendingBucket).Delete(candidateID[:]); err != nil {
			return err
		}
		changed = true
		return store.advanceCheckpoint(tx)
	})
	if err != nil {
		return store.publicError(err)
	}
	if changed {
		store.notify()
	}
	return nil
}

func (store *BoltStore) CommitCanonicalBlock(ctx context.Context, block CanonicalBlock) error {
	if err := store.validateCanonicalBlock(block); err != nil {
		return errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	members, err := sortedMemberIDs(block.Candidates)
	if err != nil {
		return errInvalidInput
	}
	requested := blockRecord{Checkpoint: block.Checkpoint, ParentHash: block.ParentHash, Members: members}
	encodedBlock, err := encodeBlock(requested, store.journalCapacity)
	if err != nil {
		return errInvalidInput
	}
	for {
		wake := store.wakeSignal()
		changed := false
		err = db.Update(func(tx *bolt.Tx) error {
			if err := contextError(ctx); err != nil {
				return err
			}
			blocks := tx.Bucket(blocksBucket)
			key := blockNumberKey(block.BlockNumber)
			if persisted := blocks.Get(key); persisted != nil {
				if !bytes.Equal(persisted, encodedBlock) {
					return errConflict
				}
				return store.validateCommittedCandidates(tx, block.Candidates)
			}
			meta := tx.Bucket(metaBucket)
			cursorData := meta.Get(scanCursorKey)
			if cursorData == nil {
				if meta.Get(baselineKey) != nil {
					return errCorrupt
				}
			} else {
				cursor, decodeErr := decodeCheckpoint(cursorData)
				if decodeErr != nil || cursor.Network != store.network || cursor.BlockNumber == ^uint64(0) {
					return errCorrupt
				}
				if block.BlockNumber != cursor.BlockNumber+1 || block.ParentHash != cursor.BlockHash {
					return errConflict
				}
			}
			wanted := make(map[domain.CandidateID]struct{}, len(members))
			for _, member := range members {
				wanted[member] = struct{}{}
			}
			if err := store.reconcileBlockObservations(tx, block, wanted); err != nil {
				return err
			}
			if err := store.reserveUnconfirmedBlock(tx, block.BlockNumber); err != nil {
				return err
			}

			additional, countErr := store.additionalJournalEntries(tx, block.Candidates)
			if countErr != nil {
				return countErr
			}
			if err := store.reserveJournal(tx, additional); err != nil {
				return err
			}
			if err := store.confirmDiscoveredTokens(tx, block.Candidates); err != nil {
				return err
			}
			candidates := tx.Bucket(candidatesBucket)
			for _, candidate := range block.Candidates {
				data := candidates.Get(candidate.ID[:])
				if data == nil {
					encoded, encodeErr := encodeCandidateRecord(candidateRecord{Status: CandidateStaged, Candidate: candidate})
					if encodeErr != nil {
						return errInvalidInput
					}
					if err := candidates.Put(candidate.ID[:], encoded); err != nil {
						return err
					}
					if err := store.indexCandidateBlock(tx, candidate); err != nil {
						return err
					}
					continue
				}
				record, decodeErr := decodeCandidateRecord(data)
				if decodeErr != nil || !store.matchesCandidate(record.Candidate) || !sameCandidateIdentity(record.Candidate, candidate) {
					return errCorrupt
				}
				if record.Status == CandidateObserved || record.Status == CandidateRemoved {
					record.Status = CandidateStaged
					record.Candidate = updateCandidateMetadata(record.Candidate, candidate)
					encoded, encodeErr := encodeCandidateRecord(record)
					if encodeErr != nil {
						return errInvalidInput
					}
					if err := candidates.Put(candidate.ID[:], encoded); err != nil {
						return err
					}
				}
			}
			if err := blocks.Put(key, encodedBlock); err != nil {
				return err
			}
			checkpointData, encodeErr := encodeCheckpoint(block.Checkpoint)
			if encodeErr != nil {
				return errInvalidInput
			}
			if cursorData == nil {
				if err := meta.Put(baselineKey, key); err != nil {
					return err
				}
			}
			if err := meta.Put(scanCursorKey, checkpointData); err != nil {
				return err
			}
			if err := store.promoteCandidates(tx, store.clock.Now()); err != nil {
				return err
			}
			changed = true
			return store.advanceCheckpoint(tx)
		})
		if errors.Is(err, errJournalFull) {
			if err := store.wait(ctx, wake, 0); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return store.publicError(err)
		}
		if changed {
			store.notify()
		}
		return nil
	}
}

func (store *BoltStore) reconcileBlockObservations(tx *bolt.Tx, block CanonicalBlock, wanted map[domain.CandidateID]struct{}) error {
	prefix := encodeUint64(block.BlockNumber)
	index := tx.Bucket(blockIndexBucket)
	candidates := tx.Bucket(candidatesBucket)
	pending := tx.Bucket(pendingBucket)
	cursor := index.Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		if len(key) != 8+common.HashLength+len(domain.CandidateID{}) || !bytes.Equal(value, []byte{recordVersion}) {
			return errCorrupt
		}
		var candidateID domain.CandidateID
		copy(candidateID[:], key[8+common.HashLength:])
		record, err := decodeCandidateRecord(candidates.Get(candidateID[:]))
		if err != nil || record.Candidate.ID != candidateID || record.Candidate.BlockNumber != block.BlockNumber {
			return errCorrupt
		}
		if _, member := wanted[candidateID]; member {
			continue
		}
		switch record.Status {
		case CandidateObserved, CandidateRemoved:
			if err := candidates.Delete(candidateID[:]); err != nil {
				return err
			}
			if err := pending.Delete(candidateID[:]); err != nil {
				return err
			}
			if err := cursor.Delete(); err != nil {
				return err
			}
		case CandidateStaged, CandidateReady, CandidateDelayed, CandidateAcknowledged:
			return errConflict
		default:
			return errCorrupt
		}
	}
	return nil
}

func (store *BoltStore) LoadScanCursor(ctx context.Context, network domain.NetworkID) (Checkpoint, bool, error) {
	return store.loadCheckpoint(ctx, network, scanCursorKey)
}

func (store *BoltStore) LoadCheckpoint(ctx context.Context, network domain.NetworkID) (Checkpoint, bool, error) {
	return store.loadCheckpoint(ctx, network, checkpointKey)
}

func (store *BoltStore) loadCheckpoint(ctx context.Context, network domain.NetworkID, key []byte) (Checkpoint, bool, error) {
	if network != store.network {
		return Checkpoint{}, false, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	defer finish()
	var (
		checkpoint Checkpoint
		found      bool
	)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		data := tx.Bucket(metaBucket).Get(key)
		if data == nil {
			return nil
		}
		var decodeErr error
		checkpoint, decodeErr = decodeCheckpoint(data)
		if decodeErr != nil || checkpoint.Network != store.network {
			return errCorrupt
		}
		found = true
		return nil
	})
	if err != nil {
		return Checkpoint{}, false, store.publicError(err)
	}
	return checkpoint, found, nil
}

func (store *BoltStore) DiscoveredTokens(ctx context.Context, network domain.NetworkID) ([]common.Address, error) {
	if network != store.network {
		return nil, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	tokens := make([]common.Address, 0)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(discoveredBucket)
		return bucket.ForEach(func(key, value []byte) error {
			if len(key) != common.AddressLength || !bytes.Equal(value, []byte{recordVersion}) || uint32(len(tokens)) == store.maxDiscoveredTokens {
				return errCorrupt
			}
			address := common.BytesToAddress(key)
			if address == (common.Address{}) {
				return errCorrupt
			}
			tokens = append(tokens, address)
			return nil
		})
	})
	if err != nil {
		return nil, store.publicError(err)
	}
	sort.Slice(tokens, func(i, j int) bool { return bytes.Compare(tokens[i][:], tokens[j][:]) < 0 })
	return tokens, nil
}

func (store *BoltStore) DiscoveryOverflowed(ctx context.Context, network domain.NetworkID) (bool, error) {
	if network != store.network {
		return false, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	overflowed := false
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		value := tx.Bucket(metaBucket).Get(discoveryOverflowKey)
		if value == nil {
			return nil
		}
		if !bytes.Equal(value, []byte{recordVersion}) {
			return errCorrupt
		}
		overflowed = true
		return nil
	})
	if err != nil {
		return false, store.publicError(err)
	}
	return overflowed, nil
}

func (store *BoltStore) Replay(ctx context.Context, network domain.NetworkID) ([]domain.RescueCandidate, error) {
	if network != store.network {
		return nil, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	result := make([]domain.RescueCandidate, 0)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		return tx.Bucket(candidatesBucket).ForEach(func(key, value []byte) error {
			record, decodeErr := decodeCandidateRecord(value)
			if decodeErr != nil || !bytes.Equal(key, record.Candidate.ID[:]) || !store.matchesCandidate(record.Candidate) {
				return errCorrupt
			}
			if isUnfinished(record.Status) {
				result = append(result, record.Candidate)
			}
			return nil
		})
	})
	if err != nil {
		return nil, store.publicError(err)
	}
	return result, nil
}

func (store *BoltStore) Next(ctx context.Context, network domain.NetworkID) (domain.RescueCandidate, error) {
	if network != store.network {
		return domain.RescueCandidate{}, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return domain.RescueCandidate{}, err
	}
	defer finish()
	for {
		wake := store.wakeSignal()
		var (
			candidate domain.RescueCandidate
			found     bool
			nextRetry time.Time
		)
		now := store.clock.Now()
		promoted := false
		err = db.Update(func(tx *bolt.Tx) error {
			if err := contextError(ctx); err != nil {
				return err
			}
			before, countErr := store.readyCount(tx)
			if countErr != nil {
				return countErr
			}
			if err := store.promoteCandidates(tx, now); err != nil {
				return err
			}
			after, countErr := store.readyCount(tx)
			if countErr != nil {
				return countErr
			}
			promoted = after > before
			if err := tx.Bucket(readyOrderBucket).ForEach(func(key, value []byte) error {
				if len(key) != 8 || len(value) != len(domain.CandidateID{}) || !bytes.Equal(tx.Bucket(pendingBucket).Get(value), key) {
					return errCorrupt
				}
				data := tx.Bucket(candidatesBucket).Get(value)
				record, decodeErr := decodeCandidateRecord(data)
				if decodeErr != nil || !bytes.Equal(value, record.Candidate.ID[:]) || !store.matchesCandidate(record.Candidate) || record.Status != CandidateReady {
					return errCorrupt
				}
				if !found {
					candidate, found = record.Candidate, true
				}
				return nil
			}); err != nil {
				return err
			}
			return tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
				record, decodeErr := decodeCandidateRecord(value)
				if decodeErr != nil {
					return errCorrupt
				}
				if record.Status == CandidateDelayed && record.RetryAt.After(now) && (nextRetry.IsZero() || record.RetryAt.Before(nextRetry)) {
					nextRetry = record.RetryAt
				}
				return nil
			})
		})
		if err != nil {
			return domain.RescueCandidate{}, store.publicError(err)
		}
		if found {
			if promoted {
				store.notify()
			}
			return candidate, nil
		}
		if promoted {
			store.notify()
		}
		wait := time.Duration(0)
		if !nextRetry.IsZero() {
			wait = nextRetry.Sub(now)
			if wait > maxDelayedWaitSlice {
				wait = maxDelayedWaitSlice
			}
		}
		if err := store.wait(ctx, wake, wait); err != nil {
			return domain.RescueCandidate{}, err
		}
	}
}

func (store *BoltStore) PutIncident(ctx context.Context, incident Incident) error {
	encoded, err := encodeIncident(incident)
	if err != nil || incident.Network != store.network {
		return errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		candidateData := tx.Bucket(candidatesBucket).Get(incident.Candidate[:])
		if candidateData == nil {
			return errConflict
		}
		record, decodeErr := decodeCandidateRecord(candidateData)
		if decodeErr != nil || !store.matchesCandidate(record.Candidate) || record.Candidate.ID != incident.Candidate {
			return errCorrupt
		}
		if record.Status != CandidateReady && record.Status != CandidateDelayed && record.Status != CandidateAcknowledged {
			return errConflict
		}
		bucket := tx.Bucket(incidentsBucket)
		persisted := bucket.Get(incident.Candidate[:])
		if persisted == nil {
			return bucket.Put(incident.Candidate[:], encoded)
		}
		stored, decodeErr := decodeIncident(persisted)
		if decodeErr != nil || stored.Candidate != incident.Candidate || stored.Network != store.network {
			return errCorrupt
		}
		if stored.ID != incident.ID {
			return errConflict
		}
		return nil
	})
	return store.publicError(err)
}

func (store *BoltStore) IncidentByCandidate(ctx context.Context, candidateID domain.CandidateID) (Incident, bool, error) {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return Incident{}, false, err
	}
	defer finish()
	var (
		incident Incident
		found    bool
	)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		data := tx.Bucket(incidentsBucket).Get(candidateID[:])
		if data == nil {
			return nil
		}
		var decodeErr error
		incident, decodeErr = decodeIncident(data)
		if decodeErr != nil || incident.Candidate != candidateID || incident.Network != store.network {
			return errCorrupt
		}
		found = true
		return nil
	})
	if err != nil {
		return Incident{}, false, store.publicError(err)
	}
	return incident, found, nil
}

func (store *BoltStore) Nack(ctx context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID, retryAt time.Time) error {
	if retryAt.IsZero() {
		return errInvalidInput
	}
	return store.resolve(ctx, candidateID, incidentID, CandidateDelayed, retryAt)
}

func (store *BoltStore) Ack(ctx context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID) error {
	return store.resolve(ctx, candidateID, incidentID, CandidateAcknowledged, time.Time{})
}

func (store *BoltStore) resolve(ctx context.Context, candidateID domain.CandidateID, incidentID domain.IncidentID, status CandidateStatus, retryAt time.Time) error {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	changed := false
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		incidentData := tx.Bucket(incidentsBucket).Get(candidateID[:])
		if incidentData == nil {
			return errConflict
		}
		incident, decodeErr := decodeIncident(incidentData)
		if decodeErr != nil || incident.Candidate != candidateID || incident.Network != store.network {
			return errCorrupt
		}
		if incident.ID != incidentID {
			return errConflict
		}
		bucket := tx.Bucket(candidatesBucket)
		data := bucket.Get(candidateID[:])
		if data == nil {
			return errCorrupt
		}
		record, decodeErr := decodeCandidateRecord(data)
		if decodeErr != nil || record.Candidate.ID != candidateID || !store.matchesCandidate(record.Candidate) {
			return errCorrupt
		}
		if record.Status == CandidateAcknowledged {
			if status == CandidateAcknowledged {
				return nil
			}
			return errConflict
		}
		if record.Status != CandidateReady {
			return errConflict
		}
		record.Status = status
		record.RetryAt = retryAt.Round(0).UTC()
		encoded, encodeErr := encodeCandidateRecord(record)
		if encodeErr != nil {
			return errInvalidInput
		}
		if err := bucket.Put(candidateID[:], encoded); err != nil {
			return err
		}
		pending := tx.Bucket(pendingBucket)
		orderKey := append([]byte(nil), pending.Get(candidateID[:])...)
		if len(orderKey) != 8 || !bytes.Equal(tx.Bucket(readyOrderBucket).Get(orderKey), candidateID[:]) {
			return errCorrupt
		}
		if err := tx.Bucket(readyOrderBucket).Delete(orderKey); err != nil {
			return err
		}
		if err := pending.Delete(candidateID[:]); err != nil {
			return err
		}
		changed = true
		if status == CandidateAcknowledged {
			if err := store.addTombstone(tx, candidateID); err != nil {
				return err
			}
			if err := store.advanceCheckpoint(tx); err != nil {
				return err
			}
		}
		return store.promoteCandidates(tx, store.clock.Now())
	})
	if err != nil {
		return store.publicError(err)
	}
	if changed {
		store.notify()
	}
	return nil
}

func (store *BoltStore) Close() error {
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return nil
	}
	store.closed = true
	close(store.done)
	store.mu.Unlock()
	store.ops.Wait()
	store.closeAllLeaseSignals()
	if err := store.db.Close(); err != nil {
		return errClosed
	}
	return nil
}

func (store *BoltStore) begin(ctx context.Context) (*bolt.DB, func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, nil, errClosed
	}
	store.ops.Add(1)
	return store.db, store.ops.Done, nil
}

func (store *BoltStore) wait(ctx context.Context, wake <-chan struct{}, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-store.done:
			return errClosed
		case <-wake:
			return nil
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.done:
		return errClosed
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (store *BoltStore) notify() {
	store.wakeMu.Lock()
	close(store.wake)
	store.wake = make(chan struct{})
	store.wakeMu.Unlock()
}

func (store *BoltStore) wakeSignal() <-chan struct{} {
	store.wakeMu.Lock()
	defer store.wakeMu.Unlock()
	return store.wake
}

func (store *BoltStore) publicError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	for _, public := range []error{errClosed, errInvalidInput, errConflict, ErrObservationSaturated, ErrLeaseHeld, ErrLeaseLost, errCorrupt} {
		if errors.Is(err, public) {
			return public
		}
	}
	return errCorrupt
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errInvalidInput
	}
	return ctx.Err()
}

func (store *BoltStore) validateInputCandidate(candidate domain.RescueCandidate) error {
	if err := validateCandidate(candidate); err != nil || !store.matchesCandidate(candidate) {
		return errInvalidInput
	}
	return nil
}

func (store *BoltStore) matchesCandidate(candidate domain.RescueCandidate) bool {
	return candidate.Network == store.network && candidate.Source == store.source
}

func (store *BoltStore) validateCanonicalBlock(block CanonicalBlock) error {
	if block.Network != store.network || block.BlockHash == (common.Hash{}) || uint64(len(block.Candidates)) > uint64(store.journalCapacity) {
		return errInvalidInput
	}
	for _, candidate := range block.Candidates {
		if err := store.validateInputCandidate(candidate); err != nil || candidate.BlockNumber != block.BlockNumber || candidate.BlockHash != block.BlockHash {
			return errInvalidInput
		}
	}
	return nil
}

func (store *BoltStore) readyCount(tx *bolt.Tx) (uint32, error) {
	var count uint32
	err := tx.Bucket(pendingBucket).ForEach(func(key, value []byte) error {
		if len(key) != len(domain.CandidateID{}) || len(value) != 8 || !bytes.Equal(tx.Bucket(readyOrderBucket).Get(value), key) || count == store.maxPending {
			return errCorrupt
		}
		count++
		return nil
	})
	return count, err
}

func (store *BoltStore) statusCount(tx *bolt.Tx, _ CandidateStatus) (uint32, error) {
	var count uint32
	err := tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
		record, err := decodeCandidateRecord(value)
		if err != nil {
			return errCorrupt
		}
		if record.Status == CandidateObserved {
			if count == store.maxPending {
				return ErrObservationSaturated
			}
			count++
		}
		return nil
	})
	return count, err
}

func (store *BoltStore) journalCount(tx *bolt.Tx) (uint32, error) {
	var count uint32
	err := tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
		record, decodeErr := decodeCandidateRecord(value)
		if decodeErr != nil {
			return errCorrupt
		}
		if !isUnfinished(record.Status) {
			return nil
		}
		if count == store.journalCapacity {
			return errJournalFull
		}
		count++
		return nil
	})
	return count, err
}

func (store *BoltStore) reserveJournal(tx *bolt.Tx, additional uint32) error {
	count, err := store.journalCount(tx)
	if err != nil {
		return err
	}
	if uint64(count)+uint64(additional) > uint64(store.journalCapacity) {
		return errJournalFull
	}
	return nil
}

func (store *BoltStore) additionalJournalEntries(tx *bolt.Tx, candidates []domain.RescueCandidate) (uint32, error) {
	var additional uint32
	bucket := tx.Bucket(candidatesBucket)
	for _, candidate := range candidates {
		data := bucket.Get(candidate.ID[:])
		if data == nil {
			additional++
			continue
		}
		record, err := decodeCandidateRecord(data)
		if err != nil || !store.matchesCandidate(record.Candidate) || !sameCandidateIdentity(record.Candidate, candidate) {
			return 0, errCorrupt
		}
		if record.Status == CandidateObserved || record.Status == CandidateRemoved {
			additional++
		}
	}
	return additional, nil
}

func (store *BoltStore) reserveUnconfirmedBlock(tx *bolt.Tx, number uint64) error {
	meta := tx.Bucket(metaBucket)
	cursorData := meta.Get(scanCursorKey)
	if cursorData == nil {
		return nil
	}
	var confirmed uint64
	if checkpointData := meta.Get(checkpointKey); checkpointData != nil {
		checkpoint, err := decodeCheckpoint(checkpointData)
		if err != nil || checkpoint.Network != store.network || number <= checkpoint.BlockNumber {
			return errCorrupt
		}
		confirmed = checkpoint.BlockNumber
	} else {
		baseline, ok := decodeUint64(meta.Get(baselineKey))
		if !ok || number < baseline {
			return errCorrupt
		}
		if number-baseline+1 > unconfirmedBlockLimit {
			return errJournalFull
		}
		return nil
	}
	if number-confirmed > unconfirmedBlockLimit {
		return errJournalFull
	}
	return nil
}

func (store *BoltStore) promoteCandidates(tx *bolt.Tx, now time.Time) error {
	ready, err := store.readyCount(tx)
	if err != nil {
		return err
	}
	available := store.maxPending - ready
	if available == 0 {
		return nil
	}
	type promotion struct {
		key    []byte
		record candidateRecord
	}
	collect := func(status CandidateStatus) ([]promotion, error) {
		items := make([]promotion, 0)
		err := tx.Bucket(candidatesBucket).ForEach(func(key, value []byte) error {
			record, decodeErr := decodeCandidateRecord(value)
			if decodeErr != nil {
				return errCorrupt
			}
			if record.Status != status || status == CandidateDelayed && record.RetryAt.After(now) {
				return nil
			}
			if tx.Bucket(pendingBucket).Get(key) != nil {
				return errCorrupt
			}
			items = append(items, promotion{key: append([]byte(nil), key...), record: record})
			return nil
		})
		return items, err
	}
	staged, err := collect(CandidateStaged)
	if err != nil {
		return err
	}
	delayed, err := collect(CandidateDelayed)
	if err != nil {
		return err
	}
	if len(staged) == 0 && len(delayed) == 0 {
		return nil
	}
	preferDelayed := false
	if value := tx.Bucket(metaBucket).Get(promotionTurnKey); value != nil {
		if len(value) != 2 || value[0] != recordVersion || value[1] > 1 {
			return errCorrupt
		}
		preferDelayed = value[1] == 1
	}
	promotions := make([]promotion, 0, available)
	for stagedIndex, delayedIndex := 0, 0; uint32(len(promotions)) < available && (stagedIndex < len(staged) || delayedIndex < len(delayed)); {
		if (preferDelayed && delayedIndex < len(delayed)) || stagedIndex >= len(staged) {
			promotions = append(promotions, delayed[delayedIndex])
			delayedIndex++
		} else {
			promotions = append(promotions, staged[stagedIndex])
			stagedIndex++
		}
		preferDelayed = !preferDelayed
	}
	for _, item := range promotions {
		item.record.Status = CandidateReady
		item.record.RetryAt = time.Time{}
		encoded, encodeErr := encodeCandidateRecord(item.record)
		if encodeErr != nil {
			return errCorrupt
		}
		if err := tx.Bucket(candidatesBucket).Put(item.key, encoded); err != nil {
			return err
		}
		readyOrder := tx.Bucket(readyOrderBucket)
		sequence, err := readyOrder.NextSequence()
		if err != nil || sequence == 0 {
			return errCorrupt
		}
		orderKey := encodeUint64(sequence)
		if readyOrder.Get(orderKey) != nil {
			return errCorrupt
		}
		if err := readyOrder.Put(orderKey, item.key); err != nil {
			return err
		}
		if err := tx.Bucket(pendingBucket).Put(item.key, orderKey); err != nil {
			return err
		}
	}
	turn := byte(0)
	if preferDelayed {
		turn = 1
	}
	return tx.Bucket(metaBucket).Put(promotionTurnKey, []byte{recordVersion, turn})
}

func (store *BoltStore) validateCommittedCandidates(tx *bolt.Tx, candidates []domain.RescueCandidate) error {
	bucket := tx.Bucket(candidatesBucket)
	for _, candidate := range candidates {
		data := bucket.Get(candidate.ID[:])
		if data == nil {
			return errCorrupt
		}
		record, err := decodeCandidateRecord(data)
		if err != nil || !store.matchesCandidate(record.Candidate) || !sameCandidateIdentity(record.Candidate, candidate) ||
			record.Status == CandidateObserved || record.Status == CandidateRemoved {
			return errCorrupt
		}
	}
	return nil
}

func (store *BoltStore) validateFullMembership(tx *bolt.Tx, block CanonicalBlock, members []domain.CandidateID) error {
	prefix := encodeUint64(block.BlockNumber)
	wanted := make(map[domain.CandidateID]struct{}, len(members))
	for _, member := range members {
		wanted[member] = struct{}{}
	}
	cursor := tx.Bucket(blockIndexBucket).Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		if len(key) != 8+common.HashLength+len(domain.CandidateID{}) || !bytes.Equal(value, []byte{recordVersion}) {
			return errCorrupt
		}
		var candidateID domain.CandidateID
		copy(candidateID[:], key[8+common.HashLength:])
		if !bytes.Equal(key[8:8+common.HashLength], block.BlockHash[:]) {
			return errConflict
		}
		if _, ok := wanted[candidateID]; !ok {
			return errConflict
		}
	}
	return nil
}

func (store *BoltStore) rejectLateCandidate(tx *bolt.Tx, candidate domain.RescueCandidate) error {
	if candidate.BlockHash == (common.Hash{}) {
		return nil
	}
	cursorData := tx.Bucket(metaBucket).Get(scanCursorKey)
	if cursorData == nil {
		return nil
	}
	cursor, err := decodeCheckpoint(cursorData)
	if err != nil || cursor.Network != store.network {
		return errCorrupt
	}
	if candidate.BlockNumber <= cursor.BlockNumber {
		return errConflict
	}
	return nil
}

func (store *BoltStore) candidateAlreadyScanned(tx *bolt.Tx, candidate domain.RescueCandidate) bool {
	if candidate.BlockHash == (common.Hash{}) {
		return false
	}
	cursorData := tx.Bucket(metaBucket).Get(scanCursorKey)
	if cursorData == nil {
		return false
	}
	cursor, err := decodeCheckpoint(cursorData)
	return err == nil && cursor.Network == store.network && candidate.BlockNumber <= cursor.BlockNumber
}

func (store *BoltStore) indexCandidateBlock(tx *bolt.Tx, candidate domain.RescueCandidate) error {
	key := candidateBlockIndexKey(candidate)
	if key == nil {
		return nil
	}
	return tx.Bucket(blockIndexBucket).Put(key, []byte{recordVersion})
}

func candidateBlockIndexKey(candidate domain.RescueCandidate) []byte {
	if candidate.BlockHash == (common.Hash{}) {
		return nil
	}
	key := make([]byte, 0, 8+common.HashLength+len(candidate.ID))
	key = appendUint64(key, candidate.BlockNumber)
	key = append(key, candidate.BlockHash[:]...)
	return append(key, candidate.ID[:]...)
}

func isUnfinished(status CandidateStatus) bool {
	return status == CandidateStaged || status == CandidateReady || status == CandidateDelayed
}

func (store *BoltStore) confirmDiscoveredTokens(tx *bolt.Tx, candidates []domain.RescueCandidate) error {
	bucket := tx.Bucket(discoveredBucket)
	var count uint32
	if err := bucket.ForEach(func(key, value []byte) error {
		if len(key) != common.AddressLength || !bytes.Equal(value, []byte{recordVersion}) || count == store.maxDiscoveredTokens {
			return errCorrupt
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	newTokens := make(map[common.Address]struct{})
	for _, candidate := range candidates {
		if candidate.Kind == domain.CandidateToken && bucket.Get(candidate.Token.Address[:]) == nil {
			newTokens[candidate.Token.Address] = struct{}{}
		}
	}
	addresses := make([]common.Address, 0, len(newTokens))
	for address := range newTokens {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
	for _, address := range addresses {
		if bucket.Get(address[:]) != nil {
			continue
		}
		if count == store.maxDiscoveredTokens {
			if err := tx.Bucket(metaBucket).Put(discoveryOverflowKey, []byte{recordVersion}); err != nil {
				return err
			}
			continue
		}
		if err := bucket.Put(address[:], []byte{recordVersion}); err != nil {
			return err
		}
		count++
	}
	return nil
}

func (store *BoltStore) advanceCheckpoint(tx *bolt.Tx) error {
	meta := tx.Bucket(metaBucket)
	baselineData := meta.Get(baselineKey)
	if baselineData == nil {
		return nil
	}
	baseline, ok := decodeUint64(baselineData)
	if !ok {
		return errCorrupt
	}
	next := baseline
	if checkpointData := meta.Get(checkpointKey); checkpointData != nil {
		checkpoint, err := decodeCheckpoint(checkpointData)
		if err != nil || checkpoint.Network != store.network || checkpoint.BlockNumber == ^uint64(0) {
			return errCorrupt
		}
		next = checkpoint.BlockNumber + 1
	}
	blocks := tx.Bucket(blocksBucket)
	for {
		data := blocks.Get(blockNumberKey(next))
		if data == nil {
			return nil
		}
		block, err := decodeBlock(data, store.journalCapacity)
		if err != nil || block.Checkpoint.Network != store.network || block.Checkpoint.BlockNumber != next {
			return errCorrupt
		}
		complete, err := store.blockComplete(tx, block)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		checkpointData, err := encodeCheckpoint(block.Checkpoint)
		if err != nil {
			return errCorrupt
		}
		if err := meta.Put(checkpointKey, checkpointData); err != nil {
			return err
		}
		if err := store.trimCanonicalBlocks(tx, next); err != nil {
			return err
		}
		if next == ^uint64(0) {
			return nil
		}
		next++
	}
}

func (store *BoltStore) trimCanonicalBlocks(tx *bolt.Tx, checkpoint uint64) error {
	meta := tx.Bucket(metaBucket)
	baseline, ok := decodeUint64(meta.Get(baselineKey))
	if !ok || checkpoint < baseline {
		return errCorrupt
	}
	if checkpoint-baseline+1 <= canonicalBlockLimit {
		return nil
	}
	retainedFrom := checkpoint - canonicalBlockLimit + 1
	blocks := tx.Bucket(blocksBucket)
	for number := baseline; number < retainedFrom; number++ {
		data := blocks.Get(blockNumberKey(number))
		block, err := decodeBlock(data, store.journalCapacity)
		if err != nil || block.Checkpoint.BlockNumber != number || block.Checkpoint.Network != store.network {
			return errCorrupt
		}
		complete, err := store.blockComplete(tx, block)
		if err != nil || !complete {
			return errCorrupt
		}
		if err := store.deleteCompletedBlockCandidates(tx, block); err != nil {
			return err
		}
		if err := blocks.Delete(blockNumberKey(number)); err != nil {
			return err
		}
	}
	return meta.Put(baselineKey, encodeUint64(retainedFrom))
}

func (store *BoltStore) addTombstone(tx *bolt.Tx, candidateID domain.CandidateID) error {
	bucket := tx.Bucket(tombstonesBucket)
	if bucket.Sequence() == ^uint64(0) {
		return errCorrupt
	}
	sequence, err := bucket.NextSequence()
	if err != nil || sequence == 0 {
		return errCorrupt
	}
	key := encodeUint64(sequence)
	if bucket.Get(key) != nil {
		return errCorrupt
	}
	if err := bucket.Put(key, candidateID[:]); err != nil {
		return err
	}
	count := 0
	if err := bucket.ForEach(func(_, _ []byte) error {
		count++
		return nil
	}); err != nil {
		return err
	}
	for count > tombstoneLimit {
		cursor := bucket.Cursor()
		key, value := cursor.First()
		if len(key) != 8 || len(value) != len(domain.CandidateID{}) {
			return errCorrupt
		}
		var expired domain.CandidateID
		copy(expired[:], value)
		if err := store.deleteAcknowledgedCandidate(tx, expired); err != nil {
			return err
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
		count--
	}
	return nil
}

func (store *BoltStore) deleteAcknowledgedCandidate(tx *bolt.Tx, candidateID domain.CandidateID) error {
	candidates := tx.Bucket(candidatesBucket)
	record, err := decodeCandidateRecord(candidates.Get(candidateID[:]))
	if err != nil || record.Status != CandidateAcknowledged || record.Candidate.ID != candidateID {
		return errCorrupt
	}
	if key := candidateBlockIndexKey(record.Candidate); key != nil {
		blockData := tx.Bucket(blocksBucket).Get(blockNumberKey(record.Candidate.BlockNumber))
		if blockData != nil {
			block, decodeErr := decodeBlock(blockData, store.journalCapacity)
			if decodeErr != nil || block.Checkpoint.BlockHash != record.Candidate.BlockHash {
				return errCorrupt
			}
			found := false
			for _, member := range block.Members {
				if member == candidateID {
					found = true
					break
				}
			}
			if !found {
				return errCorrupt
			}
			return nil
		}
		if err := tx.Bucket(blockIndexBucket).Delete(key); err != nil {
			return err
		}
	}
	if err := tx.Bucket(incidentsBucket).Delete(candidateID[:]); err != nil {
		return err
	}
	return candidates.Delete(candidateID[:])
}

func (store *BoltStore) deleteCompletedBlockCandidates(tx *bolt.Tx, block blockRecord) error {
	candidates := tx.Bucket(candidatesBucket)
	for _, candidateID := range block.Members {
		record, err := decodeCandidateRecord(candidates.Get(candidateID[:]))
		if err != nil || record.Candidate.ID != candidateID || record.Status != CandidateAcknowledged {
			return errCorrupt
		}
		if err := tx.Bucket(blockIndexBucket).Delete(candidateBlockIndexKey(record.Candidate)); err != nil {
			return err
		}
		if err := tx.Bucket(incidentsBucket).Delete(candidateID[:]); err != nil {
			return err
		}
		if err := candidates.Delete(candidateID[:]); err != nil {
			return err
		}
		if err := store.removeCandidateTombstones(tx, candidateID); err != nil {
			return err
		}
	}
	return nil
}

func (store *BoltStore) removeCandidateTombstones(tx *bolt.Tx, candidateID domain.CandidateID) error {
	cursor := tx.Bucket(tombstonesBucket).Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		if len(key) != 8 || len(value) != len(candidateID) {
			return errCorrupt
		}
		if bytes.Equal(value, candidateID[:]) {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (store *BoltStore) blockComplete(tx *bolt.Tx, block blockRecord) (bool, error) {
	bucket := tx.Bucket(candidatesBucket)
	for _, member := range block.Members {
		data := bucket.Get(member[:])
		if data == nil {
			return false, errCorrupt
		}
		record, err := decodeCandidateRecord(data)
		if err != nil || record.Candidate.ID != member || !store.matchesCandidate(record.Candidate) ||
			record.Candidate.BlockNumber != block.Checkpoint.BlockNumber || record.Candidate.BlockHash != block.Checkpoint.BlockHash {
			return false, errCorrupt
		}
		if record.Status == CandidateObserved || record.Status == CandidateRemoved {
			return false, errCorrupt
		}
		if record.Status != CandidateAcknowledged {
			return false, nil
		}
	}
	return true, nil
}

func (store *BoltStore) validateDatabase(tx *bolt.Tx) error {
	return store.validateDatabaseVersion(tx, schemaVersion)
}

func (store *BoltStore) validateDatabaseVersion(tx *bolt.Tx, version uint32) error {
	if version != schemaVersionV1 && version != schemaVersionV2 && version != schemaVersion {
		return errCorrupt
	}
	meta := tx.Bucket(metaBucket)
	if meta == nil {
		return errCorrupt
	}
	expectedBuckets := map[string]struct{}{
		string(metaBucket): {}, string(candidatesBucket): {}, string(pendingBucket): {}, string(readyOrderBucket): {}, string(blockIndexBucket): {},
		string(incidentsBucket): {}, string(blocksBucket): {}, string(discoveredBucket): {}, string(tombstonesBucket): {},
	}
	if version >= schemaVersionV2 {
		expectedBuckets[string(rescueStateBucket)] = struct{}{}
		expectedBuckets[string(leasesBucket)] = struct{}{}
	}
	seenBuckets := 0
	if err := tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		if _, ok := expectedBuckets[string(name)]; !ok {
			return errCorrupt
		}
		seenBuckets++
		return nil
	}); err != nil || seenBuckets != len(expectedBuckets) {
		return errCorrupt
	}
	bindings := []struct {
		key   []byte
		value []byte
	}{
		{schemaKey, encodeUint32(version)},
		{networkKey, encodeUint64(uint64(store.network))},
		{sourceKey, store.source[:]},
		{policyKey, store.policyFingerprint[:]},
		{maxPendingKey, encodeUint32(store.maxPending)},
		{maxDiscoveredKey, encodeUint32(store.maxDiscoveredTokens)},
	}
	for _, binding := range bindings {
		if !bytes.Equal(meta.Get(binding.key), binding.value) {
			return errCorrupt
		}
	}
	if version >= schemaVersionV2 {
		for _, binding := range []struct {
			key   []byte
			value []byte
		}{
			{sponsorKey, store.sponsor[:]},
			{destinationKey, store.destination[:]},
			{rescuerKey, store.rescuer[:]},
		} {
			if !bytes.Equal(meta.Get(binding.key), binding.value) {
				return errCorrupt
			}
		}
	}
	allowedMeta := map[string]struct{}{
		string(schemaKey): {}, string(networkKey): {}, string(sourceKey): {}, string(policyKey): {},
		string(maxPendingKey): {}, string(maxDiscoveredKey): {}, string(baselineKey): {},
		string(scanCursorKey): {}, string(checkpointKey): {}, string(discoveryOverflowKey): {}, string(promotionTurnKey): {},
	}
	if version >= schemaVersionV2 {
		allowedMeta[string(sponsorKey)] = struct{}{}
		allowedMeta[string(destinationKey)] = struct{}{}
		allowedMeta[string(rescuerKey)] = struct{}{}
		allowedMeta[string(nonceFloorKey)] = struct{}{}
		if _, ok := decodeUint64(meta.Get(nonceFloorKey)); !ok {
			return errCorrupt
		}
	}
	if err := meta.ForEach(func(key, value []byte) error {
		if value == nil {
			return errCorrupt
		}
		if _, ok := allowedMeta[string(key)]; !ok {
			return errCorrupt
		}
		if bytes.Equal(key, discoveryOverflowKey) && !bytes.Equal(value, []byte{recordVersion}) {
			return errCorrupt
		}
		if bytes.Equal(key, promotionTurnKey) && (len(value) != 2 || value[0] != recordVersion || value[1] > 1) {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	if err := store.validateCandidateIndexes(tx); err != nil {
		return errCorrupt
	}

	incidents := tx.Bucket(incidentsBucket)
	if err := incidents.ForEach(func(key, value []byte) error {
		incident, decodeErr := decodeIncident(value)
		if decodeErr != nil || len(key) != len(incident.Candidate) || !bytes.Equal(key, incident.Candidate[:]) || incident.Network != store.network {
			return errCorrupt
		}
		candidateData := tx.Bucket(candidatesBucket).Get(incident.Candidate[:])
		if candidateData == nil {
			return errCorrupt
		}
		record, decodeErr := decodeCandidateRecord(candidateData)
		if decodeErr != nil || record.Candidate.ID != incident.Candidate {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}

	var discoveredCount uint32
	if err := tx.Bucket(discoveredBucket).ForEach(func(key, value []byte) error {
		if len(key) != common.AddressLength || !bytes.Equal(value, []byte{recordVersion}) ||
			common.BytesToAddress(key) == (common.Address{}) || discoveredCount == store.maxDiscoveredTokens {
			return errCorrupt
		}
		discoveredCount++
		return nil
	}); err != nil {
		return err
	}
	if meta.Get(discoveryOverflowKey) != nil && discoveredCount != store.maxDiscoveredTokens {
		return errCorrupt
	}
	if version >= schemaVersionV2 {
		if err := store.validateRescueStateBucket(tx, version); err != nil {
			return errCorrupt
		}
		if err := store.validateLeasesBucket(tx); err != nil {
			return errCorrupt
		}
	}

	baselineData := meta.Get(baselineKey)
	cursorData := meta.Get(scanCursorKey)
	checkpointData := meta.Get(checkpointKey)
	if baselineData == nil || cursorData == nil {
		if baselineData != nil || cursorData != nil || checkpointData != nil || tx.Bucket(blocksBucket).Stats().KeyN != 0 {
			return errCorrupt
		}
		return store.validateCandidateIncidentStates(tx)
	}
	baseline, ok := decodeUint64(baselineData)
	if !ok {
		return errCorrupt
	}
	cursor, err := decodeCheckpoint(cursorData)
	if err != nil || cursor.Network != store.network || baseline > cursor.BlockNumber {
		return errCorrupt
	}
	var (
		previous           common.Hash
		expectedCheckpoint *Checkpoint
		checkpointBlocked  bool
	)
	for number := baseline; ; number++ {
		data := tx.Bucket(blocksBucket).Get(blockNumberKey(number))
		if data == nil {
			return errCorrupt
		}
		block, decodeErr := decodeBlock(data, store.journalCapacity)
		if decodeErr != nil || block.Checkpoint.Network != store.network || block.Checkpoint.BlockNumber != number {
			return errCorrupt
		}
		if number != baseline && block.ParentHash != previous {
			return errCorrupt
		}
		if err := store.validateFullMembership(tx, CanonicalBlock{Checkpoint: block.Checkpoint}, block.Members); err != nil {
			return errCorrupt
		}
		complete, completeErr := store.blockComplete(tx, block)
		if completeErr != nil {
			return completeErr
		}
		if !checkpointBlocked && complete {
			checkpoint := block.Checkpoint
			expectedCheckpoint = &checkpoint
		} else if !complete {
			checkpointBlocked = true
		}
		previous = block.Checkpoint.BlockHash
		if number == cursor.BlockNumber {
			if previous != cursor.BlockHash {
				return errCorrupt
			}
			break
		}
		if number == ^uint64(0) {
			return errCorrupt
		}
	}
	span := cursor.BlockNumber - baseline
	if span == ^uint64(0) || uint64(tx.Bucket(blocksBucket).Stats().KeyN) != span+1 {
		return errCorrupt
	}
	if expectedCheckpoint == nil {
		if checkpointData != nil {
			return errCorrupt
		}
	} else {
		if checkpointData == nil {
			return errCorrupt
		}
		checkpoint, decodeErr := decodeCheckpoint(checkpointData)
		if decodeErr != nil || checkpoint != *expectedCheckpoint {
			return errCorrupt
		}
		if checkpoint.BlockNumber < baseline || checkpoint.BlockNumber-baseline+1 > canonicalBlockLimit {
			return errCorrupt
		}
		if cursor.BlockNumber-checkpoint.BlockNumber > unconfirmedBlockLimit {
			return errCorrupt
		}
	}
	if expectedCheckpoint == nil && cursor.BlockNumber-baseline+1 > unconfirmedBlockLimit {
		return errCorrupt
	}
	return store.validateCandidateIncidentStates(tx)
}

func (store *BoltStore) validateCandidateIndexes(tx *bolt.Tx) error {
	if _, err := store.readyCount(tx); err != nil {
		return err
	}
	if _, err := store.journalCount(tx); err != nil {
		return err
	}
	observed, err := store.statusCount(tx, CandidateObserved)
	if err != nil || observed > store.maxPending {
		return err
	}
	pending := tx.Bucket(pendingBucket)
	readyOrder := tx.Bucket(readyOrderBucket)
	blockIndex := tx.Bucket(blockIndexBucket)
	if err := tx.Bucket(candidatesBucket).ForEach(func(key, value []byte) error {
		record, decodeErr := decodeCandidateRecord(value)
		if decodeErr != nil || len(key) != len(record.Candidate.ID) || !bytes.Equal(key, record.Candidate.ID[:]) || !store.matchesCandidate(record.Candidate) {
			return errCorrupt
		}
		pendingValue := pending.Get(key)
		if record.Status == CandidateReady {
			if len(pendingValue) != 8 || !bytes.Equal(readyOrder.Get(pendingValue), key) {
				return errCorrupt
			}
		} else if pendingValue != nil {
			return errCorrupt
		}
		blockKey := candidateBlockIndexKey(record.Candidate)
		if blockKey != nil && !bytes.Equal(blockIndex.Get(blockKey), []byte{recordVersion}) {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pending.ForEach(func(key, value []byte) error {
		if len(key) != len(domain.CandidateID{}) || len(value) != 8 || !bytes.Equal(readyOrder.Get(value), key) {
			return errCorrupt
		}
		record, decodeErr := decodeCandidateRecord(tx.Bucket(candidatesBucket).Get(key))
		if decodeErr != nil || record.Status != CandidateReady || !bytes.Equal(key, record.Candidate.ID[:]) {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	var maximumOrder uint64
	if err := readyOrder.ForEach(func(key, value []byte) error {
		if len(key) != 8 || len(value) != len(domain.CandidateID{}) || !bytes.Equal(pending.Get(value), key) {
			return errCorrupt
		}
		sequence, ok := decodeUint64(key)
		if !ok || sequence == 0 || sequence <= maximumOrder {
			return errCorrupt
		}
		maximumOrder = sequence
		record, decodeErr := decodeCandidateRecord(tx.Bucket(candidatesBucket).Get(value))
		if decodeErr != nil || record.Status != CandidateReady || !bytes.Equal(value, record.Candidate.ID[:]) {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	if readyOrder.Sequence() < maximumOrder || readyOrder.Sequence() == ^uint64(0) {
		return errCorrupt
	}
	if err := blockIndex.ForEach(func(key, value []byte) error {
		const keySize = 8 + common.HashLength + 32
		if len(key) != keySize || !bytes.Equal(value, []byte{recordVersion}) {
			return errCorrupt
		}
		var candidateID domain.CandidateID
		copy(candidateID[:], key[8+common.HashLength:])
		record, decodeErr := decodeCandidateRecord(tx.Bucket(candidatesBucket).Get(candidateID[:]))
		if decodeErr != nil || !bytes.Equal(key, candidateBlockIndexKey(record.Candidate)) {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	if err := store.validateTombstones(tx); err != nil {
		return err
	}
	return nil
}

func (store *BoltStore) validateTombstones(tx *bolt.Tx) error {
	bucket := tx.Bucket(tombstonesBucket)
	if bucket.Stats().KeyN > tombstoneLimit {
		return errCorrupt
	}
	seen := make(map[domain.CandidateID]struct{}, bucket.Stats().KeyN)
	var maximumSequence uint64
	if err := bucket.ForEach(func(key, value []byte) error {
		if len(key) != 8 || len(value) != len(domain.CandidateID{}) {
			return errCorrupt
		}
		sequence, ok := decodeUint64(key)
		if !ok || sequence == 0 || sequence <= maximumSequence {
			return errCorrupt
		}
		maximumSequence = sequence
		var candidateID domain.CandidateID
		copy(candidateID[:], value)
		if _, duplicate := seen[candidateID]; duplicate {
			return errCorrupt
		}
		seen[candidateID] = struct{}{}
		record, err := decodeCandidateRecord(tx.Bucket(candidatesBucket).Get(candidateID[:]))
		if err != nil || record.Status != CandidateAcknowledged || record.Candidate.ID != candidateID {
			return errCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	if bucket.Sequence() < maximumSequence || bucket.Sequence() == ^uint64(0) {
		return errCorrupt
	}
	return tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
		record, err := decodeCandidateRecord(value)
		if err != nil {
			return err
		}
		if record.Status != CandidateAcknowledged {
			return nil
		}
		if _, ok := seen[record.Candidate.ID]; ok {
			return nil
		}
		retained, err := store.retainedCanonicalMember(tx, record.Candidate)
		if err != nil || !retained {
			return errCorrupt
		}
		return nil
	})
}

func (store *BoltStore) retainedCanonicalMember(tx *bolt.Tx, candidate domain.RescueCandidate) (bool, error) {
	if candidateBlockIndexKey(candidate) == nil {
		return false, nil
	}
	data := tx.Bucket(blocksBucket).Get(blockNumberKey(candidate.BlockNumber))
	if data == nil {
		return false, nil
	}
	block, err := decodeBlock(data, store.journalCapacity)
	if err != nil || block.Checkpoint.Network != store.network || block.Checkpoint.BlockNumber != candidate.BlockNumber ||
		block.Checkpoint.BlockHash != candidate.BlockHash {
		return false, errCorrupt
	}
	for _, member := range block.Members {
		if member == candidate.ID {
			return true, nil
		}
	}
	return false, nil
}

func (store *BoltStore) validateCandidateIncidentStates(tx *bolt.Tx) error {
	return tx.Bucket(candidatesBucket).ForEach(func(_, value []byte) error {
		record, err := decodeCandidateRecord(value)
		if err != nil {
			return errCorrupt
		}
		if record.Status == CandidateAcknowledged || record.Status == CandidateDelayed {
			incidentData := tx.Bucket(incidentsBucket).Get(record.Candidate.ID[:])
			incident, decodeErr := decodeIncident(incidentData)
			if decodeErr != nil || incident.Candidate != record.Candidate.ID || incident.Network != store.network {
				return errCorrupt
			}
		}
		return nil
	})
}

func blockNumberKey(number uint64) []byte { return encodeUint64(number) }

func encodeUint32(value uint32) []byte {
	var result [4]byte
	binary.BigEndian.PutUint32(result[:], value)
	return result[:]
}

func encodeUint64(value uint64) []byte {
	var result [8]byte
	binary.BigEndian.PutUint64(result[:], value)
	return result[:]
}

func decodeUint64(value []byte) (uint64, bool) {
	if len(value) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(value), true
}
