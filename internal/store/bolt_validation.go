package store

import (
	"bytes"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

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
