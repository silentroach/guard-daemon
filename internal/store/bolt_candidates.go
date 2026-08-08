package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

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

func (store *BoltStore) validateInputCandidate(candidate domain.RescueCandidate) error {
	if err := validateCandidate(candidate); err != nil || !store.matchesCandidate(candidate) {
		return errInvalidInput
	}
	return nil
}

func (store *BoltStore) matchesCandidate(candidate domain.RescueCandidate) bool {
	return candidate.Network == store.network && candidate.Source == store.source
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
