package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

const (
	recordVersion  = byte(1)
	maxSymbolBytes = 64
)

var errInvalidRecord = errors.New("некорректная запись хранилища")

type candidateRecord struct {
	Status    CandidateStatus
	RetryAt   time.Time
	Candidate domain.RescueCandidate
}

type blockRecord struct {
	Checkpoint Checkpoint
	ParentHash common.Hash
	Members    []domain.CandidateID
}

func encodeCandidateRecord(record candidateRecord) ([]byte, error) {
	if err := validateCandidateRecord(record); err != nil {
		return nil, err
	}
	if !record.RetryAt.IsZero() && !validPersistentTime(record.RetryAt) {
		return nil, errInvalidRecord
	}
	candidate := record.Candidate
	symbol := []byte(candidate.Token.Symbol)
	result := make([]byte, 0, 2+12+32+8+1+20+20+1+2+len(symbol)+32+8+32+8+8+8)
	result = append(result, recordVersion, byte(record.Status))
	result = appendTime(result, record.RetryAt)
	result = append(result, candidate.ID[:]...)
	result = appendUint64(result, uint64(candidate.Network))
	result = append(result, byte(candidate.Kind))
	result = append(result, candidate.Source[:]...)
	result = append(result, candidate.Token.Address[:]...)
	result = append(result, candidate.Token.Decimals)
	result = appendUint16(result, uint16(len(symbol)))
	result = append(result, symbol...)
	result = append(result, candidate.BlockHash[:]...)
	result = appendUint64(result, candidate.BlockNumber)
	result = append(result, candidate.TxHash[:]...)
	result = appendUint64(result, uint64(candidate.LogIndex))
	result = appendUint64(result, candidate.Generation)
	result = appendUint64(result, candidate.Observation)
	return result, nil
}

func decodeCandidateRecord(data []byte) (candidateRecord, error) {
	const fixedSize = 2 + 12 + 32 + 8 + 1 + 20 + 20 + 1 + 2 + 32 + 8 + 32 + 8 + 8 + 8
	if len(data) < fixedSize || data[0] != recordVersion {
		return candidateRecord{}, errInvalidRecord
	}
	offset := 1
	status := CandidateStatus(data[offset])
	offset++
	retryAt, ok := readTime(data, &offset)
	if !ok {
		return candidateRecord{}, errInvalidRecord
	}
	var candidate domain.RescueCandidate
	if !readFixed(data, &offset, candidate.ID[:]) {
		return candidateRecord{}, errInvalidRecord
	}
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return candidateRecord{}, errInvalidRecord
	}
	candidate.Network = domain.NetworkID(int64(network))
	if offset >= len(data) {
		return candidateRecord{}, errInvalidRecord
	}
	candidate.Kind = domain.CandidateKind(data[offset])
	offset++
	if !readFixed(data, &offset, candidate.Source[:]) || !readFixed(data, &offset, candidate.Token.Address[:]) || offset >= len(data) {
		return candidateRecord{}, errInvalidRecord
	}
	candidate.Token.Decimals = data[offset]
	offset++
	symbolLength, ok := readUint16(data, &offset)
	if !ok || symbolLength > maxSymbolBytes || int(symbolLength) > len(data)-offset {
		return candidateRecord{}, errInvalidRecord
	}
	candidate.Token.Symbol = string(data[offset : offset+int(symbolLength)])
	offset += int(symbolLength)
	if !readFixed(data, &offset, candidate.BlockHash[:]) {
		return candidateRecord{}, errInvalidRecord
	}
	if candidate.BlockNumber, ok = readUint64(data, &offset); !ok {
		return candidateRecord{}, errInvalidRecord
	}
	if !readFixed(data, &offset, candidate.TxHash[:]) {
		return candidateRecord{}, errInvalidRecord
	}
	logIndex, ok := readUint64(data, &offset)
	if !ok || uint64(uint(logIndex)) != logIndex {
		return candidateRecord{}, errInvalidRecord
	}
	candidate.LogIndex = uint(logIndex)
	if candidate.Generation, ok = readUint64(data, &offset); !ok {
		return candidateRecord{}, errInvalidRecord
	}
	if candidate.Observation, ok = readUint64(data, &offset); !ok || offset != len(data) {
		return candidateRecord{}, errInvalidRecord
	}
	record := candidateRecord{Status: status, RetryAt: retryAt, Candidate: candidate}
	if err := validateCandidateRecord(record); err != nil {
		return candidateRecord{}, err
	}
	return record, nil
}

func validateCandidateRecord(record candidateRecord) error {
	if err := validateCandidate(record.Candidate); err != nil {
		return err
	}
	switch record.Status {
	case CandidateObserved, CandidateStaged, CandidateReady, CandidateAcknowledged, CandidateRemoved:
		if !record.RetryAt.IsZero() {
			return errInvalidRecord
		}
	case CandidateDelayed:
		if record.RetryAt.IsZero() {
			return errInvalidRecord
		}
	default:
		return errInvalidRecord
	}
	return nil
}

func validateCandidate(candidate domain.RescueCandidate) error {
	if len(candidate.Token.Symbol) > maxSymbolBytes {
		return errInvalidRecord
	}
	if err := domain.ValidateCandidate(candidate); err != nil {
		return errInvalidRecord
	}
	return nil
}

func sameCandidateIdentity(left, right domain.RescueCandidate) bool {
	left.Token.Symbol = ""
	left.Token.Decimals = 0
	left.Generation = 0
	right.Token.Symbol = ""
	right.Token.Decimals = 0
	right.Generation = 0
	return left == right
}

func updateCandidateMetadata(persisted, canonical domain.RescueCandidate) domain.RescueCandidate {
	persisted.Token.Symbol = canonical.Token.Symbol
	persisted.Token.Decimals = canonical.Token.Decimals
	persisted.Generation = canonical.Generation
	return persisted
}

func encodeIncident(incident Incident) ([]byte, error) {
	if incident.Network <= 0 || incident.ID != domain.NewIncidentID(incident.Candidate) || !validPersistentTime(incident.CreatedAt) {
		return nil, errInvalidRecord
	}
	result := make([]byte, 0, 1+32+32+8+12)
	result = append(result, recordVersion)
	result = append(result, incident.ID[:]...)
	result = append(result, incident.Candidate[:]...)
	result = appendUint64(result, uint64(incident.Network))
	result = appendTime(result, incident.CreatedAt)
	return result, nil
}

func decodeIncident(data []byte) (Incident, error) {
	const size = 1 + 32 + 32 + 8 + 12
	if len(data) != size || data[0] != recordVersion {
		return Incident{}, errInvalidRecord
	}
	offset := 1
	var incident Incident
	if !readFixed(data, &offset, incident.ID[:]) || !readFixed(data, &offset, incident.Candidate[:]) {
		return Incident{}, errInvalidRecord
	}
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return Incident{}, errInvalidRecord
	}
	incident.Network = domain.NetworkID(int64(network))
	if incident.CreatedAt, ok = readTime(data, &offset); !ok || offset != len(data) {
		return Incident{}, errInvalidRecord
	}
	if incident.Network <= 0 || incident.ID != domain.NewIncidentID(incident.Candidate) || incident.CreatedAt.IsZero() {
		return Incident{}, errInvalidRecord
	}
	return incident, nil
}

func encodeBlock(record blockRecord, maxMembers uint32) ([]byte, error) {
	if err := validateBlockRecord(record, maxMembers); err != nil {
		return nil, err
	}
	result := make([]byte, 0, 1+8+8+32+32+4+len(record.Members)*32)
	result = append(result, recordVersion)
	result = appendUint64(result, uint64(record.Checkpoint.Network))
	result = appendUint64(result, record.Checkpoint.BlockNumber)
	result = append(result, record.Checkpoint.BlockHash[:]...)
	result = append(result, record.ParentHash[:]...)
	result = appendUint32(result, uint32(len(record.Members)))
	for _, member := range record.Members {
		result = append(result, member[:]...)
	}
	return result, nil
}

func decodeBlock(data []byte, maxMembers uint32) (blockRecord, error) {
	const fixedSize = 1 + 8 + 8 + 32 + 32 + 4
	if len(data) < fixedSize || data[0] != recordVersion {
		return blockRecord{}, errInvalidRecord
	}
	offset := 1
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return blockRecord{}, errInvalidRecord
	}
	number, ok := readUint64(data, &offset)
	if !ok {
		return blockRecord{}, errInvalidRecord
	}
	record := blockRecord{Checkpoint: Checkpoint{Network: domain.NetworkID(int64(network)), BlockNumber: number}}
	if !readFixed(data, &offset, record.Checkpoint.BlockHash[:]) || !readFixed(data, &offset, record.ParentHash[:]) {
		return blockRecord{}, errInvalidRecord
	}
	count, ok := readUint32(data, &offset)
	if !ok || count > maxMembers || uint64(count)*32 != uint64(len(data)-offset) {
		return blockRecord{}, errInvalidRecord
	}
	record.Members = make([]domain.CandidateID, int(count))
	for index := range record.Members {
		if !readFixed(data, &offset, record.Members[index][:]) {
			return blockRecord{}, errInvalidRecord
		}
	}
	if offset != len(data) {
		return blockRecord{}, errInvalidRecord
	}
	if err := validateBlockRecord(record, maxMembers); err != nil {
		return blockRecord{}, err
	}
	return record, nil
}

func validateBlockRecord(record blockRecord, maxMembers uint32) error {
	if record.Checkpoint.Network <= 0 || record.Checkpoint.BlockHash == (common.Hash{}) || uint64(len(record.Members)) > uint64(maxMembers) {
		return errInvalidRecord
	}
	if (record.Checkpoint.BlockNumber == 0) != (record.ParentHash == (common.Hash{})) {
		return errInvalidRecord
	}
	for index, member := range record.Members {
		if index > 0 && bytes.Compare(record.Members[index-1][:], member[:]) >= 0 {
			return errInvalidRecord
		}
	}
	return nil
}

func sortedMemberIDs(candidates []domain.RescueCandidate) ([]domain.CandidateID, error) {
	members := make([]domain.CandidateID, len(candidates))
	for index, candidate := range candidates {
		members[index] = candidate.ID
	}
	sort.Slice(members, func(i, j int) bool {
		return bytes.Compare(members[i][:], members[j][:]) < 0
	})
	for index := 1; index < len(members); index++ {
		if members[index-1] == members[index] {
			return nil, errInvalidRecord
		}
	}
	return members, nil
}

func encodeCheckpoint(checkpoint Checkpoint) ([]byte, error) {
	if checkpoint.Network <= 0 || checkpoint.BlockHash == (common.Hash{}) {
		return nil, errInvalidRecord
	}
	result := make([]byte, 0, 8+8+32)
	result = appendUint64(result, uint64(checkpoint.Network))
	result = appendUint64(result, checkpoint.BlockNumber)
	result = append(result, checkpoint.BlockHash[:]...)
	return result, nil
}

func decodeCheckpoint(data []byte) (Checkpoint, error) {
	if len(data) != 8+8+32 {
		return Checkpoint{}, errInvalidRecord
	}
	offset := 0
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return Checkpoint{}, errInvalidRecord
	}
	number, ok := readUint64(data, &offset)
	if !ok {
		return Checkpoint{}, errInvalidRecord
	}
	checkpoint := Checkpoint{Network: domain.NetworkID(int64(network)), BlockNumber: number}
	if !readFixed(data, &offset, checkpoint.BlockHash[:]) || offset != len(data) || checkpoint.Network <= 0 || checkpoint.BlockHash == (common.Hash{}) {
		return Checkpoint{}, errInvalidRecord
	}
	return checkpoint, nil
}

func appendTime(target []byte, value time.Time) []byte {
	if value.IsZero() {
		return append(target, make([]byte, 12)...)
	}
	value = value.Round(0).UTC()
	target = appendUint64(target, uint64(value.Unix()))
	return appendUint32(target, uint32(value.Nanosecond()))
}

func validPersistentTime(value time.Time) bool {
	return !value.IsZero() && (value.Unix() != 0 || value.Nanosecond() != 0)
}

func readTime(data []byte, offset *int) (time.Time, bool) {
	seconds, ok := readUint64(data, offset)
	if !ok {
		return time.Time{}, false
	}
	nanoseconds, ok := readUint32(data, offset)
	if !ok || nanoseconds >= uint32(time.Second) {
		return time.Time{}, false
	}
	if seconds == 0 && nanoseconds == 0 {
		return time.Time{}, true
	}
	return time.Unix(int64(seconds), int64(nanoseconds)).UTC(), true
}

func appendUint16(target []byte, value uint16) []byte {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	return append(target, encoded[:]...)
}

func appendUint32(target []byte, value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return append(target, encoded[:]...)
}

func appendUint64(target []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(target, encoded[:]...)
}

func readUint16(data []byte, offset *int) (uint16, bool) {
	if *offset < 0 || len(data)-*offset < 2 {
		return 0, false
	}
	value := binary.BigEndian.Uint16(data[*offset : *offset+2])
	*offset += 2
	return value, true
}

func readUint32(data []byte, offset *int) (uint32, bool) {
	if *offset < 0 || len(data)-*offset < 4 {
		return 0, false
	}
	value := binary.BigEndian.Uint32(data[*offset : *offset+4])
	*offset += 4
	return value, true
}

func readUint64(data []byte, offset *int) (uint64, bool) {
	if *offset < 0 || len(data)-*offset < 8 {
		return 0, false
	}
	value := binary.BigEndian.Uint64(data[*offset : *offset+8])
	*offset += 8
	return value, true
}

func readFixed(data []byte, offset *int, target []byte) bool {
	if *offset < 0 || len(target) > len(data)-*offset {
		return false
	}
	copy(target, data[*offset:*offset+len(target)])
	*offset += len(target)
	return true
}
