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

const (
	leaseRecordVersion = byte(1)
	maxLeaseOwnerBytes = 128
)

var (
	ErrLeaseHeld = errors.New("хранилище: lease уже удерживается")
	ErrLeaseLost = errors.New("хранилище: lease потерян")
	closedLease  = func() <-chan struct{} {
		closed := make(chan struct{})
		close(closed)
		return closed
	}()
)

type leaseIdentity struct {
	key         LeaseKey
	owner       string
	expiresUnix int64
	expiresNano int32
}

func (store *BoltStore) Acquire(ctx context.Context, key LeaseKey, owner string, ttl time.Duration) (Lease, error) {
	now := normalizeStoreTime(store.clock.Now())
	expiresAt, ok := leaseExpiration(now, ttl)
	requested := Lease{Key: key, Owner: owner, ExpiresAt: expiresAt}
	encoded, err := encodeLease(requested)
	if !ok || err != nil || key.Network != store.network {
		return Lease{}, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer finish()

	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(leasesBucket)
		keyBytes := encodeLeaseKey(key)
		if persisted := bucket.Get(keyBytes); persisted != nil {
			current, decodeErr := decodeLease(persisted)
			if decodeErr != nil || current.Key != key {
				return errCorrupt
			}
			if current.ExpiresAt.After(now) {
				return ErrLeaseHeld
			}
		}
		return bucket.Put(keyBytes, encoded)
	})
	if err != nil {
		public := store.publicError(err)
		if errors.Is(public, errCorrupt) {
			store.closeLeaseKeySignals(key)
		}
		return Lease{}, public
	}
	store.replaceLeaseSignal(requested)
	return requested, nil
}

func (store *BoltStore) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	lease = normalizeLease(lease)
	now := normalizeStoreTime(store.clock.Now())
	expiresAt, ok := leaseExpiration(now, ttl)
	renewed := lease
	renewed.ExpiresAt = expiresAt
	encoded, err := encodeLease(renewed)
	if !ok || err != nil || lease.Key.Network != store.network {
		return Lease{}, errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer finish()

	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(leasesBucket)
		key := encodeLeaseKey(lease.Key)
		persisted := bucket.Get(key)
		if persisted == nil {
			return ErrLeaseLost
		}
		current, decodeErr := decodeLease(persisted)
		if decodeErr != nil || current.Key != lease.Key {
			return errCorrupt
		}
		if !sameLease(current, lease) || !current.ExpiresAt.After(now) {
			return ErrLeaseLost
		}
		return bucket.Put(key, encoded)
	})
	if err != nil {
		public := store.publicError(err)
		if leaseLossDetected(public) {
			store.closeLeaseSignal(lease)
		}
		return Lease{}, public
	}
	if sameLease(renewed, lease) {
		return lease, nil
	}
	store.replaceLeaseSignal(renewed)
	return renewed, nil
}

func (store *BoltStore) Validate(ctx context.Context, lease Lease) error {
	lease = normalizeLease(lease)
	if err := validateLease(lease); err != nil || lease.Key.Network != store.network {
		return errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	now := normalizeStoreTime(store.clock.Now())
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		data := tx.Bucket(leasesBucket).Get(encodeLeaseKey(lease.Key))
		if data == nil {
			return ErrLeaseLost
		}
		current, decodeErr := decodeLease(data)
		if decodeErr != nil || current.Key != lease.Key {
			return errCorrupt
		}
		if !sameLease(current, lease) || !current.ExpiresAt.After(now) {
			return ErrLeaseLost
		}
		return nil
	})
	public := store.publicError(err)
	if leaseLossDetected(public) {
		store.closeLeaseSignal(lease)
	}
	return public
}

func (store *BoltStore) Lost(lease Lease) <-chan struct{} {
	lease = normalizeLease(lease)
	if validateLease(lease) != nil || lease.Key.Network != store.network {
		return closedLease
	}
	id := leaseSignalIdentity(lease)
	store.leaseMu.Lock()
	defer store.leaseMu.Unlock()
	if signal := store.leaseLoss[id]; signal != nil {
		return signal
	}
	return closedLease
}

func (store *BoltStore) Release(ctx context.Context, lease Lease) error {
	lease = normalizeLease(lease)
	if err := validateLease(lease); err != nil || lease.Key.Network != store.network {
		return errInvalidInput
	}
	db, finish, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	now := normalizeStoreTime(store.clock.Now())
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(leasesBucket)
		key := encodeLeaseKey(lease.Key)
		persisted := bucket.Get(key)
		if persisted == nil {
			return ErrLeaseLost
		}
		current, decodeErr := decodeLease(persisted)
		if decodeErr != nil || current.Key != lease.Key {
			return errCorrupt
		}
		if !sameLease(current, lease) || !current.ExpiresAt.After(now) {
			return ErrLeaseLost
		}
		return bucket.Delete(key)
	})
	public := store.publicError(err)
	if public == nil || leaseLossDetected(public) {
		store.closeLeaseSignal(lease)
	}
	return public
}

func (store *BoltStore) validateLeasesBucket(tx *bolt.Tx) error {
	bucket := tx.Bucket(leasesBucket)
	if bucket.Sequence() != 0 {
		return errCorrupt
	}
	return bucket.ForEach(func(key, value []byte) error {
		lease, err := decodeLease(value)
		if err != nil || lease.Key.Network != store.network || !bytes.Equal(key, encodeLeaseKey(lease.Key)) {
			return errCorrupt
		}
		return nil
	})
}

func encodeLease(lease Lease) ([]byte, error) {
	lease = normalizeLease(lease)
	if err := validateLease(lease); err != nil {
		return nil, err
	}
	owner := []byte(lease.Owner)
	result := make([]byte, 0, 1+8+common.AddressLength+2+len(owner)+12)
	result = append(result, leaseRecordVersion)
	result = appendUint64(result, uint64(lease.Key.Network))
	result = append(result, lease.Key.Sponsor[:]...)
	result = appendUint16(result, uint16(len(owner)))
	result = append(result, owner...)
	result = appendTime(result, lease.ExpiresAt)
	return result, nil
}

func decodeLease(data []byte) (Lease, error) {
	const fixedSize = 1 + 8 + common.AddressLength + 2 + 12
	if len(data) < fixedSize || data[0] != leaseRecordVersion {
		return Lease{}, errInvalidRecord
	}
	offset := 1
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return Lease{}, errInvalidRecord
	}
	lease := Lease{Key: LeaseKey{Network: domain.NetworkID(network)}}
	if !readFixed(data, &offset, lease.Key.Sponsor[:]) {
		return Lease{}, errInvalidRecord
	}
	ownerLength, ok := readUint16(data, &offset)
	if !ok || ownerLength == 0 || ownerLength > maxLeaseOwnerBytes || int(ownerLength) > len(data)-offset {
		return Lease{}, errInvalidRecord
	}
	lease.Owner = string(data[offset : offset+int(ownerLength)])
	offset += int(ownerLength)
	if lease.ExpiresAt, ok = readTime(data, &offset); !ok || offset != len(data) {
		return Lease{}, errInvalidRecord
	}
	if err := validateLease(lease); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func validateLease(lease Lease) error {
	if lease.Key.Network <= 0 || lease.Key.Sponsor == (common.Address{}) || len(lease.Owner) == 0 || len(lease.Owner) > maxLeaseOwnerBytes ||
		!validRescueTime(lease.ExpiresAt) {
		return errInvalidRecord
	}
	return nil
}

func leaseExpiration(now time.Time, ttl time.Duration) (time.Time, bool) {
	if ttl <= 0 || !validRescueTime(now) {
		return time.Time{}, false
	}
	expiresAt := normalizeStoreTime(now.Add(ttl))
	return expiresAt, validRescueTime(expiresAt) && expiresAt.After(now)
}

func encodeLeaseKey(key LeaseKey) []byte {
	result := make([]byte, 0, 8+common.AddressLength)
	result = appendUint64(result, uint64(key.Network))
	return append(result, key.Sponsor[:]...)
}

func normalizeLease(lease Lease) Lease {
	lease.ExpiresAt = normalizeStoreTime(lease.ExpiresAt)
	return lease
}

func sameLease(left, right Lease) bool {
	return left.Key == right.Key && left.Owner == right.Owner && left.ExpiresAt.Equal(right.ExpiresAt)
}

func leaseSignalIdentity(lease Lease) leaseIdentity {
	return leaseIdentity{
		key:         lease.Key,
		owner:       lease.Owner,
		expiresUnix: lease.ExpiresAt.Unix(),
		expiresNano: int32(lease.ExpiresAt.Nanosecond()),
	}
}

func (store *BoltStore) replaceLeaseSignal(lease Lease) {
	id := leaseSignalIdentity(lease)
	store.leaseMu.Lock()
	defer store.leaseMu.Unlock()
	for existing, signal := range store.leaseLoss {
		if existing.key == lease.Key {
			close(signal)
			delete(store.leaseLoss, existing)
		}
	}
	store.leaseLoss[id] = make(chan struct{})
}

func (store *BoltStore) closeLeaseSignal(lease Lease) {
	id := leaseSignalIdentity(lease)
	store.leaseMu.Lock()
	defer store.leaseMu.Unlock()
	if signal := store.leaseLoss[id]; signal != nil {
		close(signal)
		delete(store.leaseLoss, id)
	}
}

func (store *BoltStore) closeLeaseKeySignals(key LeaseKey) {
	store.leaseMu.Lock()
	defer store.leaseMu.Unlock()
	for id, signal := range store.leaseLoss {
		if id.key == key {
			close(signal)
			delete(store.leaseLoss, id)
		}
	}
}

func leaseLossDetected(err error) bool {
	return errors.Is(err, ErrLeaseLost) || errors.Is(err, errCorrupt)
}

func (store *BoltStore) closeAllLeaseSignals() {
	store.leaseMu.Lock()
	defer store.leaseMu.Unlock()
	for id, signal := range store.leaseLoss {
		close(signal)
		delete(store.leaseLoss, id)
	}
}
