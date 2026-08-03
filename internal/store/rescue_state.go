package store

import (
	"bytes"
	"context"
	"math"
	"sort"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	bolt "go.etcd.io/bbolt"
)

const (
	rescueRecordVersion     = byte(2)
	maxErrorCodeBytes       = 128
	maxSignedTransactionLen = 128 << 10
)

func (store *BoltStore) PutRescueIncident(ctx context.Context, incident RescueIncident) (RescueIncident, error) {
	incident = cloneRescueIncident(normalizeRescueIncident(incident))
	encoded, err := encodeRescueIncident(incident)
	if err != nil || incident.Network != store.network || incident.Status != RescuePending {
		return RescueIncident{}, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return RescueIncident{}, err
	}
	defer finish()

	stored := incident
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(rescueStateBucket)
		persisted := bucket.Get(incident.ID[:])
		if persisted == nil {
			return bucket.Put(incident.ID[:], encoded)
		}
		var decodeErr error
		stored, decodeErr = decodeRescueIncident(persisted)
		if decodeErr != nil || stored.Network != store.network || stored.ID != incident.ID {
			return errCorrupt
		}
		if !sameRescueIdentity(stored, incident) {
			return errConflict
		}
		return nil
	})
	if err != nil {
		return RescueIncident{}, store.publicError(err)
	}
	return cloneRescueIncident(stored), nil
}

func (store *BoltStore) UpdateRescueIncident(ctx context.Context, incident RescueIncident) error {
	incident = cloneRescueIncident(normalizeRescueIncident(incident))
	encoded, err := encodeRescueIncident(incident)
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
		bucket := tx.Bucket(rescueStateBucket)
		persisted := bucket.Get(incident.ID[:])
		if persisted == nil {
			return errConflict
		}
		current, decodeErr := decodeRescueIncident(persisted)
		if decodeErr != nil || current.Network != store.network || current.ID != incident.ID {
			return errCorrupt
		}
		if sameRescueIncident(current, incident) {
			return nil
		}
		if !validRescueTransition(current, incident) {
			return errConflict
		}
		return bucket.Put(incident.ID[:], encoded)
	})
	return store.publicError(err)
}

func (store *BoltStore) RescueIncident(ctx context.Context, id domain.IncidentID) (RescueIncident, bool, error) {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return RescueIncident{}, false, err
	}
	defer finish()

	var (
		incident RescueIncident
		found    bool
	)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		data := tx.Bucket(rescueStateBucket).Get(id[:])
		if data == nil {
			return nil
		}
		var decodeErr error
		incident, decodeErr = decodeRescueIncident(data)
		if decodeErr != nil || incident.ID != id || incident.Network != store.network {
			return errCorrupt
		}
		found = true
		return nil
	})
	if err != nil {
		return RescueIncident{}, false, store.publicError(err)
	}
	return cloneRescueIncident(incident), found, nil
}

func (store *BoltStore) RescueIncidents(ctx context.Context, network domain.NetworkID) ([]RescueIncident, error) {
	if network != store.network {
		return nil, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()

	incidents := make([]RescueIncident, 0)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		return tx.Bucket(rescueStateBucket).ForEach(func(key, value []byte) error {
			incident, decodeErr := decodeRescueIncident(value)
			if decodeErr != nil || len(key) != len(incident.ID) || !bytes.Equal(key, incident.ID[:]) || incident.Network != store.network {
				return errCorrupt
			}
			incidents = append(incidents, cloneRescueIncident(incident))
			return nil
		})
	})
	if err != nil {
		return nil, store.publicError(err)
	}
	sort.Slice(incidents, func(i, j int) bool {
		return bytes.Compare(incidents[i].ID[:], incidents[j].ID[:]) < 0
	})
	return incidents, nil
}

func (store *BoltStore) NonceFloor(ctx context.Context) (uint64, error) {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()

	var floor uint64
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		var ok bool
		floor, ok = decodeUint64(tx.Bucket(metaBucket).Get(nonceFloorKey))
		if !ok {
			return errCorrupt
		}
		return nil
	})
	if err != nil {
		return 0, store.publicError(err)
	}
	return floor, nil
}

func (store *BoltStore) RaiseNonceFloor(ctx context.Context, floor uint64) error {
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		current, ok := decodeUint64(meta.Get(nonceFloorKey))
		if !ok {
			return errCorrupt
		}
		if floor <= current {
			return nil
		}
		return meta.Put(nonceFloorKey, encodeUint64(floor))
	})
	return store.publicError(err)
}

func (store *BoltStore) PruneRescueIncidents(ctx context.Context, retain int) error {
	if retain < 0 || uint64(retain) > uint64(maxConfiguredBound) {
		return errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	type terminalIncident struct {
		id        domain.IncidentID
		updatedAt time.Time
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(rescueStateBucket)
		terminals := make([]terminalIncident, 0)
		if err := bucket.ForEach(func(key, value []byte) error {
			if err := contextError(ctx); err != nil {
				return err
			}
			incident, decodeErr := decodeRescueIncident(value)
			if decodeErr != nil || len(key) != len(incident.ID) || !bytes.Equal(key, incident.ID[:]) || incident.Network != store.network {
				return errCorrupt
			}
			if isTerminalRescueStatus(incident.Status) {
				terminals = append(terminals, terminalIncident{id: incident.ID, updatedAt: incident.UpdatedAt})
			}
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(terminals, func(i, j int) bool {
			if terminals[i].updatedAt.Equal(terminals[j].updatedAt) {
				return bytes.Compare(terminals[i].id[:], terminals[j].id[:]) < 0
			}
			return terminals[i].updatedAt.Before(terminals[j].updatedAt)
		})
		for _, terminal := range terminals[:max(0, len(terminals)-retain)] {
			if err := contextError(ctx); err != nil {
				return err
			}
			if err := bucket.Delete(terminal.id[:]); err != nil {
				return err
			}
		}
		return nil
	})
	return store.publicError(err)
}

func (store *BoltStore) validateRescueStateBucket(tx *bolt.Tx) error {
	bucket := tx.Bucket(rescueStateBucket)
	if bucket.Sequence() != 0 {
		return errCorrupt
	}
	return bucket.ForEach(func(key, value []byte) error {
		incident, err := decodeRescueIncident(value)
		if err != nil || len(key) != len(incident.ID) || !bytes.Equal(key, incident.ID[:]) || incident.Network != store.network {
			return errCorrupt
		}
		return nil
	})
}

func encodeRescueIncident(incident RescueIncident) ([]byte, error) {
	incident = normalizeRescueIncident(incident)
	if err := validateRescueIncident(incident); err != nil {
		return nil, err
	}
	code := []byte(incident.LastCode)
	result := make([]byte, 0, 400+len(code)+len(incident.SignedTransaction))
	result = append(result, rescueRecordVersion)
	result = append(result, incident.ID[:]...)
	result = append(result, incident.Parent[:]...)
	result = append(result, incident.Candidate[:]...)
	result = appendUint64(result, uint64(incident.Network))
	result = append(result, byte(incident.Kind))
	result = append(result, incident.Asset[:]...)
	result = appendUint64(result, incident.Generation)
	if incident.Trusted {
		result = append(result, 1)
	} else {
		result = append(result, 0)
	}
	result = appendUint32(result, incident.Policy.MaxAttempts)
	result = appendUint64(result, uint64(incident.Policy.RetryDelay))
	result = appendUint64(result, uint64(incident.Policy.FinalityTimeout))
	result = append(result, byte(incident.Status))
	result = appendUint32(result, incident.Attempts)
	result = appendUint64(result, incident.SponsorNonce)
	result = appendUint64(result, incident.SourceNonce)
	result = append(result, incident.TxHash[:]...)
	result = appendUint32(result, uint32(len(incident.SignedTransaction)))
	result = append(result, incident.SignedTransaction...)
	result = appendUint64(result, incident.SnapshotBlockNumber)
	result = append(result, incident.SnapshotBlockHash[:]...)
	result = append(result, incident.SourceBefore[:]...)
	result = append(result, incident.DestinationBefore[:]...)
	result = appendTime(result, incident.RetryAt)
	result = appendTime(result, incident.ReconcileUntil)
	result = appendUint16(result, uint16(len(code)))
	result = append(result, code...)
	result = appendTime(result, incident.CreatedAt)
	result = appendTime(result, incident.UpdatedAt)
	return result, nil
}

func decodeRescueIncident(data []byte) (RescueIncident, error) {
	const fixedSize = 1 + 32 + 32 + 32 + 8 + 1 + 20 + 8 + 1 + 4 + 8 + 8 + 1 + 4 + 8 + 8 + 32 + 4 + 8 + 32 + 32 + 32 + 12 + 12 + 2 + 12 + 12
	if len(data) < fixedSize || data[0] != rescueRecordVersion {
		return RescueIncident{}, errInvalidRecord
	}
	offset := 1
	var incident RescueIncident
	if !readFixed(data, &offset, incident.ID[:]) || !readFixed(data, &offset, incident.Parent[:]) || !readFixed(data, &offset, incident.Candidate[:]) {
		return RescueIncident{}, errInvalidRecord
	}
	network, ok := readUint64(data, &offset)
	if !ok || network > math.MaxInt64 {
		return RescueIncident{}, errInvalidRecord
	}
	incident.Network = domain.NetworkID(network)
	if offset >= len(data) {
		return RescueIncident{}, errInvalidRecord
	}
	incident.Kind = domain.CandidateKind(data[offset])
	offset++
	if !readFixed(data, &offset, incident.Asset[:]) {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.Generation, ok = readUint64(data, &offset); !ok || offset >= len(data) {
		return RescueIncident{}, errInvalidRecord
	}
	if data[offset] > 1 {
		return RescueIncident{}, errInvalidRecord
	}
	incident.Trusted = data[offset] == 1
	offset++
	if incident.Policy.MaxAttempts, ok = readUint32(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	retryDelay, ok := readUint64(data, &offset)
	if !ok || retryDelay > math.MaxInt64 {
		return RescueIncident{}, errInvalidRecord
	}
	incident.Policy.RetryDelay = time.Duration(retryDelay)
	finalityTimeout, ok := readUint64(data, &offset)
	if !ok || finalityTimeout > math.MaxInt64 || offset >= len(data) {
		return RescueIncident{}, errInvalidRecord
	}
	incident.Policy.FinalityTimeout = time.Duration(finalityTimeout)
	incident.Status = RescueStatus(data[offset])
	offset++
	if incident.Attempts, ok = readUint32(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.SponsorNonce, ok = readUint64(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.SourceNonce, ok = readUint64(data, &offset); !ok || !readFixed(data, &offset, incident.TxHash[:]) {
		return RescueIncident{}, errInvalidRecord
	}
	signedLength, ok := readUint32(data, &offset)
	if !ok || signedLength > maxSignedTransactionLen || uint64(signedLength) > uint64(len(data)-offset) {
		return RescueIncident{}, errInvalidRecord
	}
	incident.SignedTransaction = append([]byte(nil), data[offset:offset+int(signedLength)]...)
	offset += int(signedLength)
	if incident.SnapshotBlockNumber, ok = readUint64(data, &offset); !ok || !readFixed(data, &offset, incident.SnapshotBlockHash[:]) ||
		!readFixed(data, &offset, incident.SourceBefore[:]) || !readFixed(data, &offset, incident.DestinationBefore[:]) {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.RetryAt, ok = readTime(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.ReconcileUntil, ok = readTime(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	codeLength, ok := readUint16(data, &offset)
	if !ok || codeLength > maxErrorCodeBytes || int(codeLength) > len(data)-offset {
		return RescueIncident{}, errInvalidRecord
	}
	incident.LastCode = domain.ErrorCode(string(data[offset : offset+int(codeLength)]))
	offset += int(codeLength)
	if incident.CreatedAt, ok = readTime(data, &offset); !ok {
		return RescueIncident{}, errInvalidRecord
	}
	if incident.UpdatedAt, ok = readTime(data, &offset); !ok || offset != len(data) {
		return RescueIncident{}, errInvalidRecord
	}
	if err := validateRescueIncident(incident); err != nil {
		return RescueIncident{}, err
	}
	return incident, nil
}

func validateRescueIncident(incident RescueIncident) error {
	if incident.Network <= 0 || incident.Candidate == (domain.CandidateID{}) || incident.Generation == 0 ||
		incident.ID != domain.NewAssetIncidentID(incident.Candidate, incident.Kind, incident.Asset) ||
		incident.Parent != domain.NewIncidentID(incident.Candidate) {
		return errInvalidRecord
	}
	switch incident.Kind {
	case domain.CandidateNative:
		if incident.Asset != (common.Address{}) {
			return errInvalidRecord
		}
	case domain.CandidateToken:
		if incident.Asset == (common.Address{}) {
			return errInvalidRecord
		}
	default:
		return errInvalidRecord
	}
	if incident.Policy.MaxAttempts == 0 || incident.Policy.MaxAttempts > maxConfiguredBound || incident.Policy.RetryDelay <= 0 ||
		incident.Policy.FinalityTimeout <= 0 || incident.Attempts > incident.Policy.MaxAttempts || len(incident.LastCode) > maxErrorCodeBytes ||
		len(incident.SignedTransaction) > maxSignedTransactionLen {
		return errInvalidRecord
	}
	if !validRescueTime(incident.CreatedAt) || !validRescueTime(incident.UpdatedAt) || incident.UpdatedAt.Before(incident.CreatedAt) {
		return errInvalidRecord
	}
	if (!incident.RetryAt.IsZero() && !validRescueTime(incident.RetryAt)) ||
		(!incident.ReconcileUntil.IsZero() && !validRescueTime(incident.ReconcileUntil)) || !validRescueStatus(incident.Status) {
		return errInvalidRecord
	}
	if incident.Status == RescuePending {
		if incident.Attempts != 0 || incident.SponsorNonce != 0 || incident.SourceNonce != 0 || incident.TxHash != (common.Hash{}) ||
			len(incident.SignedTransaction) != 0 || incident.SnapshotBlockNumber != 0 || incident.SnapshotBlockHash != (common.Hash{}) ||
			incident.SourceBefore != ([32]byte{}) || incident.DestinationBefore != ([32]byte{}) || !incident.RetryAt.IsZero() ||
			!incident.ReconcileUntil.IsZero() || incident.LastCode != "" {
			return errInvalidRecord
		}
		return nil
	}
	if incident.Attempts == 0 || incident.SnapshotBlockHash == (common.Hash{}) {
		return errInvalidRecord
	}
	switch incident.Status {
	case RescuePrepared, RescueExhausted:
		if !hasNoSignedTransaction(incident) || !incident.RetryAt.IsZero() {
			return errInvalidRecord
		}
	case RescueRetryable:
		if !hasNoSignedTransaction(incident) || incident.LastCode == "" || incident.RetryAt.IsZero() ||
			incident.RetryAt.Before(incident.UpdatedAt) || incident.RetryAt.Sub(incident.UpdatedAt) < incident.Policy.RetryDelay {
			return errInvalidRecord
		}
	case RescueSigned, RescueBroadcast:
		if validateSignedRescueTransaction(incident) != nil || !incident.RetryAt.IsZero() ||
			incident.ReconcileUntil.IsZero() || !incident.ReconcileUntil.After(incident.UpdatedAt) {
			return errInvalidRecord
		}
	case RescueAmbiguous:
		if validateSignedRescueTransaction(incident) != nil || incident.LastCode == "" || incident.ReconcileUntil.IsZero() ||
			!incident.ReconcileUntil.After(incident.UpdatedAt) ||
			(!incident.RetryAt.IsZero() && (incident.RetryAt.Before(incident.UpdatedAt) || incident.RetryAt.After(incident.ReconcileUntil))) {
			return errInvalidRecord
		}
	case RescueFailed:
		if !incident.RetryAt.IsZero() || !validTerminalSignedState(incident) {
			return errInvalidRecord
		}
	case RescueTrustedSuccess, RescueTokenReported, RescueLostRace:
		if incident.TxHash == (common.Hash{}) || !incident.RetryAt.IsZero() || !validTerminalSignedState(incident) {
			return errInvalidRecord
		}
	default:
		return errInvalidRecord
	}
	if incident.Status == RescueTrustedSuccess && !incident.Trusted {
		return errInvalidRecord
	}
	if incident.Status == RescueTokenReported && (incident.Trusted || incident.Kind != domain.CandidateToken) {
		return errInvalidRecord
	}
	return nil
}

func validateSignedRescueTransaction(incident RescueIncident) error {
	if len(incident.SignedTransaction) == 0 || len(incident.SignedTransaction) > maxSignedTransactionLen || incident.TxHash == (common.Hash{}) {
		return errInvalidRecord
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(incident.SignedTransaction); err != nil || transaction.Type() != types.SetCodeTxType ||
		transaction.Hash() != incident.TxHash || transaction.Nonce() != incident.SponsorNonce || len(transaction.Data()) == 0 {
		return errInvalidRecord
	}
	authorizations := transaction.SetCodeAuthorizations()
	if len(authorizations) != 1 || authorizations[0].Nonce != incident.SourceNonce {
		return errInvalidRecord
	}
	return nil
}

func validTerminalSignedState(incident RescueIncident) bool {
	if incident.TxHash == (common.Hash{}) {
		return len(incident.SignedTransaction) == 0 && incident.ReconcileUntil.IsZero()
	}
	if len(incident.SignedTransaction) == 0 {
		return incident.ReconcileUntil.IsZero()
	}
	return validateSignedRescueTransaction(incident) == nil
}

func hasNoSignedTransaction(incident RescueIncident) bool {
	return incident.TxHash == (common.Hash{}) && len(incident.SignedTransaction) == 0 && incident.ReconcileUntil.IsZero()
}

func validRescueTransition(current, next RescueIncident) bool {
	if !sameRescueIdentity(current, next) || !current.CreatedAt.Equal(next.CreatedAt) || next.UpdatedAt.Before(current.UpdatedAt) {
		return false
	}
	switch current.Status {
	case RescuePending:
		return (next.Status == RescuePrepared || next.Status == RescueRetryable) && next.Attempts == 1
	case RescuePrepared:
		if next.Attempts != current.Attempts || !sameRescuePreparation(current, next) {
			return false
		}
		return (next.Status == RescueSigned && next.ReconcileUntil.Sub(next.UpdatedAt) >= next.Policy.FinalityTimeout) ||
			next.Status == RescueRetryable || next.Status == RescueExhausted ||
			(next.Status == RescueFailed && next.TxHash == (common.Hash{}))
	case RescueSigned:
		return (next.Status == RescueBroadcast || next.Status == RescueAmbiguous) && sameSignedRescueOperation(current, next) &&
			next.ReconcileUntil.Equal(current.ReconcileUntil)
	case RescueBroadcast:
		if next.Status == RescueAmbiguous {
			return sameSignedRescueOperation(current, next) && next.ReconcileUntil.Equal(current.ReconcileUntil)
		}
		return isReceiptTerminalStatus(next.Status) && validReceiptTerminalTransition(current, next)
	case RescueRetryable:
		if current.RetryAt.IsZero() || next.UpdatedAt.Before(current.RetryAt) {
			return false
		}
		switch next.Status {
		case RescuePrepared, RescueRetryable:
			return current.Attempts < math.MaxUint32 && next.Attempts == current.Attempts+1
		case RescueExhausted:
			return next.Attempts == current.Attempts && sameRescuePreparation(current, next)
		case RescueFailed:
			return next.Attempts == current.Attempts && next.TxHash == (common.Hash{}) && sameRescuePreparation(current, next)
		default:
			return false
		}
	case RescueAmbiguous:
		switch next.Status {
		case RescueBroadcast:
			return sameSignedRescueOperation(current, next) && next.ReconcileUntil.Equal(current.ReconcileUntil)
		case RescueAmbiguous:
			return sameSignedRescueOperation(current, next) && !next.ReconcileUntil.Before(current.ReconcileUntil) &&
				next.ReconcileUntil.Sub(current.ReconcileUntil) <= current.Policy.FinalityTimeout
		default:
			return isReceiptTerminalStatus(next.Status) && validReceiptTerminalTransition(current, next)
		}
	default:
		return false
	}
}

func validReceiptTerminalTransition(current, next RescueIncident) bool {
	if next.Attempts != current.Attempts || !sameRescuePreparation(current, next) || next.TxHash != current.TxHash {
		return false
	}
	if len(next.SignedTransaction) == 0 {
		return next.ReconcileUntil.IsZero()
	}
	return bytes.Equal(next.SignedTransaction, current.SignedTransaction) &&
		(next.ReconcileUntil.IsZero() || next.ReconcileUntil.Equal(current.ReconcileUntil))
}

func sameSignedRescueOperation(left, right RescueIncident) bool {
	return left.Attempts == right.Attempts && sameRescuePreparation(left, right) && left.TxHash == right.TxHash &&
		bytes.Equal(left.SignedTransaction, right.SignedTransaction)
}

func sameRescuePreparation(left, right RescueIncident) bool {
	return left.SponsorNonce == right.SponsorNonce && left.SourceNonce == right.SourceNonce &&
		left.SnapshotBlockNumber == right.SnapshotBlockNumber && left.SnapshotBlockHash == right.SnapshotBlockHash &&
		left.SourceBefore == right.SourceBefore && left.DestinationBefore == right.DestinationBefore
}

func validRescueStatus(status RescueStatus) bool {
	return status >= RescuePending && status <= RescueLostRace
}

func isTerminalRescueStatus(status RescueStatus) bool {
	switch status {
	case RescueExhausted, RescueFailed, RescueTrustedSuccess, RescueTokenReported, RescueLostRace:
		return true
	default:
		return false
	}
}

func isReceiptTerminalStatus(status RescueStatus) bool {
	switch status {
	case RescueFailed, RescueTrustedSuccess, RescueTokenReported, RescueLostRace:
		return true
	default:
		return false
	}
}

func sameRescueIdentity(left, right RescueIncident) bool {
	return left.ID == right.ID && left.Parent == right.Parent && left.Candidate == right.Candidate && left.Network == right.Network &&
		left.Kind == right.Kind && left.Asset == right.Asset && left.Generation == right.Generation && left.Trusted == right.Trusted &&
		left.Policy == right.Policy
}

func sameRescueIncident(left, right RescueIncident) bool {
	left = normalizeRescueIncident(left)
	right = normalizeRescueIncident(right)
	return sameRescueIdentity(left, right) && left.Status == right.Status && left.Attempts == right.Attempts &&
		left.SponsorNonce == right.SponsorNonce && left.SourceNonce == right.SourceNonce && left.TxHash == right.TxHash &&
		bytes.Equal(left.SignedTransaction, right.SignedTransaction) && left.SnapshotBlockNumber == right.SnapshotBlockNumber &&
		left.SnapshotBlockHash == right.SnapshotBlockHash && left.SourceBefore == right.SourceBefore &&
		left.DestinationBefore == right.DestinationBefore && left.RetryAt.Equal(right.RetryAt) &&
		left.ReconcileUntil.Equal(right.ReconcileUntil) && left.LastCode == right.LastCode &&
		left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt)
}

func cloneRescueIncident(incident RescueIncident) RescueIncident {
	incident.SignedTransaction = append([]byte(nil), incident.SignedTransaction...)
	return incident
}

func normalizeRescueIncident(incident RescueIncident) RescueIncident {
	incident.CreatedAt = normalizeStoreTime(incident.CreatedAt)
	incident.UpdatedAt = normalizeStoreTime(incident.UpdatedAt)
	incident.RetryAt = normalizeStoreTime(incident.RetryAt)
	incident.ReconcileUntil = normalizeStoreTime(incident.ReconcileUntil)
	return incident
}

func normalizeStoreTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.Round(0).UTC()
}

func validRescueTime(value time.Time) bool {
	return !value.IsZero() && value.Unix() > 0 && value.Nanosecond() >= 0 && value.Nanosecond() < int(time.Second)
}
