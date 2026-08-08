package store

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

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
