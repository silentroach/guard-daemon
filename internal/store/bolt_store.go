package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
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

	errInvalidOptions = errors.New("store: invalid options")
	errOpenFailed     = errors.New("store: failed to open database")
	errBinding        = errors.New("store: stored binding does not match")
	errCorrupt        = errors.New("store: state data is corrupt")
	errClosed         = errors.New("store: database is closed")
	errInvalidInput   = errors.New("store: invalid input")
	errConflict       = errors.New("store: state transition conflict")
	errJournalFull    = errors.New("store: persistent journal capacity reached")

	// ErrObservationSaturated означает, что предварительное недоверенное наблюдение
	// отброшено. Каноническое сканирование остаётся источником достоверных данных
	// и восстановит кандидата.
	ErrObservationSaturated = errors.New("store: preliminary observation limit reached")
)

type storeClock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// BoltStore хранит состояние наблюдателя и координатора в одной базе bbolt
// с транзакционными гарантиями ACID.
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

// Open открывает либо инициализирует привязанное к конфигурации хранилище,
// сохраняющее согласованность при сбоях.
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
