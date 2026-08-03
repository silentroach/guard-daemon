package budget

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	bolt "go.etcd.io/bbolt"
)

const (
	ledgerSchemaVersion = uint32(1)
	ledgerOpenTimeout   = 250 * time.Millisecond
	defaultMaxRecords   = uint32(100_000)
	maximumObservedSkew = 2 * time.Minute
)

var (
	ledgerMetaBucket         = []byte("meta")
	ledgerReservationsBucket = []byte("reservations")
	ledgerAttemptsBucket     = []byte("attempts")

	ledgerSchemaKey      = []byte("schema")
	ledgerPolicyKey      = []byte("policy")
	ledgerFingerprintKey = []byte("policy-fingerprint")
	ledgerLastSeenKey    = []byte("last-seen")
	ledgerMaxRecordsKey  = []byte("max-records")
)

type sponsorKey struct {
	network domain.NetworkID
	sponsor common.Address
}

type scopeUsage struct {
	reserved  uint256.Int
	hourSpent uint256.Int
	daySpent  uint256.Int
	allSpent  uint256.Int
}

type aggregateUsage struct {
	global   scopeUsage
	networks map[domain.NetworkID]scopeUsage
	sponsors map[sponsorKey]uint256.Int
}

// BudgetLedger хранит все сети в отдельном ACID bbolt-файле.
type BudgetLedger struct {
	db         *bolt.DB
	policy     Policy
	networks   map[domain.NetworkID]NetworkPolicy
	now        func() time.Time
	maxRecords uint32

	mu     sync.Mutex
	closed bool
	ops    sync.WaitGroup
}

var _ Ledger = (*BudgetLedger)(nil)

// Open открывает либо атомарно инициализирует ledger, привязанный к policy и
// внешнему fingerprint. Существующий непустой файл никогда не переинициализируется.
func Open(path string, options OpenOptions) (*BudgetLedger, error) {
	policy, networks, err := validateAndNormalizePolicy(options.Policy)
	if path == "" || err != nil || options.Now == nil || options.PolicyFingerprint == ([32]byte{}) {
		return nil, ErrInvalidOptions
	}
	now := normalizeTime(options.Now())
	if !validPersistentTime(now) {
		return nil, ErrInvalidOptions
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = defaultMaxRecords
	}
	if err := ensureLedgerParent(filepath.Dir(path)); err != nil {
		return nil, ErrOpenFailed
	}

	created := false
	file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case createErr == nil:
		created = true
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return nil, ErrOpenFailed
		}
	case errors.Is(createErr, os.ErrExist):
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return nil, ErrOpenFailed
		}
		if info.Size() == 0 {
			return nil, ErrCorrupt
		}
	default:
		return nil, ErrOpenFailed
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: ledgerOpenTimeout, NoSync: false})
	if err != nil {
		if created {
			_ = os.Remove(path)
		}
		return nil, ErrOpenFailed
	}
	fail := func(public error) (*BudgetLedger, error) {
		_ = db.Close()
		if created {
			_ = os.Remove(path)
		}
		return nil, public
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fail(ErrOpenFailed)
	}

	ledger := &BudgetLedger{db: db, policy: policy, networks: networks, now: options.Now, maxRecords: options.MaxRecords}
	initialized := false
	err = db.Update(func(tx *bolt.Tx) error {
		var initializeErr error
		initialized, initializeErr = ledger.initialize(tx, options.PolicyFingerprint, now, created)
		if initializeErr != nil {
			return initializeErr
		}
		lastSeen, timeErr := decodeStoredTime(tx.Bucket(ledgerMetaBucket).Get(ledgerLastSeenKey))
		if timeErr != nil {
			return timeErr
		}
		_, validateErr := ledger.collectUsage(tx, lastSeen)
		return validateErr
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyMismatch):
			return fail(ErrPolicyMismatch)
		default:
			return fail(ErrCorrupt)
		}
	}
	if initialized != created {
		return fail(ErrCorrupt)
	}
	return ledger, nil
}

func ensureLedgerParent(parent string) error {
	info, err := os.Lstat(parent)
	switch {
	case err == nil:
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidOptions
		}
		return os.Chmod(parent, 0o700)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	return os.Chmod(parent, 0o700)
}

func validateAndNormalizePolicy(policy Policy) (Policy, map[domain.NetworkID]NetworkPolicy, error) {
	if err := validateLimits(policy.Global); err != nil || len(policy.Networks) == 0 || uint64(len(policy.Networks)) > uint64(^uint32(0)) {
		return Policy{}, nil, ErrInvalidOptions
	}
	result := Policy{Global: policy.Global, Networks: append([]NetworkPolicy(nil), policy.Networks...)}
	sort.Slice(result.Networks, func(i, j int) bool { return result.Networks[i].Network < result.Networks[j].Network })
	networks := make(map[domain.NetworkID]NetworkPolicy, len(result.Networks))
	for _, network := range result.Networks {
		if network.Network <= 0 || network.Sponsor == (common.Address{}) || network.EmergencySponsorReserve.IsZero() || validateLimits(network.Limits) != nil {
			return Policy{}, nil, ErrInvalidOptions
		}
		if _, duplicate := networks[network.Network]; duplicate {
			return Policy{}, nil, ErrInvalidOptions
		}
		networks[network.Network] = network
	}
	return result, networks, nil
}

func validateLimits(limits Limits) error {
	if limits.PerTransaction.IsZero() || limits.PerHour.IsZero() || limits.PerDay.IsZero() || limits.Cumulative.IsZero() ||
		limits.PerTransaction.Gt(&limits.PerHour) || limits.PerHour.Gt(&limits.PerDay) || limits.PerDay.Gt(&limits.Cumulative) {
		return ErrInvalidOptions
	}
	return nil
}

func (ledger *BudgetLedger) initialize(tx *bolt.Tx, fingerprint [32]byte, now time.Time, allowInitialize bool) (bool, error) {
	meta := tx.Bucket(ledgerMetaBucket)
	if meta == nil {
		if !allowInitialize {
			return false, ErrCorrupt
		}
		key, _ := tx.Cursor().First()
		if key != nil {
			return false, ErrCorrupt
		}
		var err error
		if meta, err = tx.CreateBucket(ledgerMetaBucket); err != nil {
			return false, err
		}
		if _, err := tx.CreateBucket(ledgerReservationsBucket); err != nil {
			return false, err
		}
		if _, err := tx.CreateBucket(ledgerAttemptsBucket); err != nil {
			return false, err
		}
		bindings := []struct {
			key   []byte
			value []byte
		}{
			{ledgerSchemaKey, encodeUint32(ledgerSchemaVersion)},
			{ledgerPolicyKey, encodePolicy(ledger.policy)},
			{ledgerFingerprintKey, fingerprint[:]},
			{ledgerLastSeenKey, appendTime(nil, time.Unix(1, 0).UTC())},
			{ledgerMaxRecordsKey, encodeUint32(ledger.maxRecords)},
		}
		for _, binding := range bindings {
			if err := meta.Put(binding.key, binding.value); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	if tx.Bucket(ledgerReservationsBucket) == nil || tx.Bucket(ledgerAttemptsBucket) == nil {
		return false, ErrCorrupt
	}
	schema := meta.Get(ledgerSchemaKey)
	if len(schema) != 4 {
		return false, ErrCorrupt
	}
	if !bytes.Equal(schema, encodeUint32(ledgerSchemaVersion)) {
		return false, ErrPolicyMismatch
	}
	storedFingerprint := meta.Get(ledgerFingerprintKey)
	storedPolicy := meta.Get(ledgerPolicyKey)
	if len(storedFingerprint) != len(fingerprint) || storedPolicy == nil {
		return false, ErrCorrupt
	}
	if !bytes.Equal(storedFingerprint, fingerprint[:]) || !bytes.Equal(storedPolicy, encodePolicy(ledger.policy)) {
		return false, ErrPolicyMismatch
	}
	if !bytes.Equal(meta.Get(ledgerMaxRecordsKey), encodeUint32(ledger.maxRecords)) {
		return false, ErrPolicyMismatch
	}
	if _, err := decodeStoredTime(meta.Get(ledgerLastSeenKey)); err != nil {
		return false, ErrCorrupt
	}
	return false, nil
}

func (ledger *BudgetLedger) Reserve(ctx context.Context, request ReservationRequest) (Reservation, error) {
	if err := ledger.validateRequest(request); err != nil {
		return Reservation{}, err
	}
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer finish()
	observed, err := ledger.observedNow()
	if err != nil {
		return Reservation{}, err
	}
	if !request.ObservedAt.IsZero() {
		chainObserved := normalizeTime(request.ObservedAt)
		if !validPersistentTime(chainObserved) || !withinObservedSkew(chainObserved, observed) {
			return Reservation{}, ErrInvalidRequest
		}
	}

	var reservation Reservation
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		now, err := ledger.effectiveNow(tx, observed)
		if err != nil {
			return err
		}
		usage, err := ledger.collectUsage(tx, now)
		if err != nil {
			return err
		}
		attempts := tx.Bucket(ledgerAttemptsBucket)
		key := attemptKey(request.Attempt)
		if existingID := attempts.Get(key); existingID != nil {
			if len(existingID) != len(ReservationID{}) {
				return ErrCorrupt
			}
			var id ReservationID
			copy(id[:], existingID)
			existing, err := ledger.reservationByID(tx, id)
			if err != nil || existing.Request.Attempt != request.Attempt {
				return ErrCorrupt
			}
			if !sameReservationRequest(existing.Request, request) {
				return ErrStateConflict
			}
			if existing.State == ReservationReleased {
				network := ledger.networks[request.Network]
				maximum, err := request.Quote.Maximum(network.TransactionOverhead)
				if err != nil {
					return err
				}
				if err := checkScopeBudget(ledger.policy.Global, usage.global, maximum); err != nil {
					return err
				}
				if err := checkScopeBudget(network.Limits, usage.networks[request.Network], maximum); err != nil {
					return err
				}
				reserved := usage.sponsors[sponsorKey{network: request.Network, sponsor: request.Sponsor}]
				if !sponsorHasCapacity(request.SponsorBalance, network.EmergencySponsorReserve, reserved, maximum) {
					return ErrSponsorReserve
				}
				existing.Request = request
				existing.State = ReservationHeld
				existing.TxHash = common.Hash{}
				existing.Actual.Clear()
				existing.CreatedAt = now
				existing.ExposedAt = time.Time{}
				existing.UpdatedAt = now
				encoded, err := encodeReservation(existing)
				if err != nil {
					return ErrCorrupt
				}
				if err := tx.Bucket(ledgerReservationsBucket).Put(existing.ID[:], encoded); err != nil {
					return err
				}
			}
			reservation = existing
			return nil
		}
		if uint64(tx.Bucket(ledgerReservationsBucket).Stats().KeyN) >= uint64(ledger.maxRecords) {
			removed, err := ledger.purgeOldestReleased(tx)
			if err != nil {
				return err
			}
			if !removed {
				return ErrCapacity
			}
		}

		network := ledger.networks[request.Network]
		maximum, err := request.Quote.Maximum(network.TransactionOverhead)
		if err != nil {
			return err
		}
		if err := checkScopeBudget(ledger.policy.Global, usage.global, maximum); err != nil {
			return err
		}
		if err := checkScopeBudget(network.Limits, usage.networks[request.Network], maximum); err != nil {
			return err
		}
		reserved := usage.sponsors[sponsorKey{network: request.Network, sponsor: request.Sponsor}]
		if !sponsorHasCapacity(request.SponsorBalance, network.EmergencySponsorReserve, reserved, maximum) {
			return ErrSponsorReserve
		}

		reservation = Reservation{
			ID:        NewReservationID(request.Attempt),
			Request:   request,
			Maximum:   maximum,
			State:     ReservationHeld,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if tx.Bucket(ledgerReservationsBucket).Get(reservation.ID[:]) != nil {
			return ErrCorrupt
		}
		encoded, err := encodeReservation(reservation)
		if err != nil {
			return ErrCorrupt
		}
		if err := tx.Bucket(ledgerReservationsBucket).Put(reservation.ID[:], encoded); err != nil {
			return err
		}
		return attempts.Put(key, reservation.ID[:])
	})
	if err != nil {
		return Reservation{}, ledger.publicError(err)
	}
	return reservation, nil
}

func (ledger *BudgetLedger) MarkExposed(ctx context.Context, id ReservationID, txHash common.Hash) (Reservation, error) {
	if id == (ReservationID{}) || txHash == (common.Hash{}) {
		return Reservation{}, ErrInvalidRequest
	}
	return ledger.transition(ctx, id, time.Time{}, func(reservation *Reservation, now time.Time) error {
		switch reservation.State {
		case ReservationHeld:
			reservation.State = ReservationExposed
			reservation.TxHash = txHash
			reservation.ExposedAt = now
			reservation.UpdatedAt = now
			return nil
		case ReservationExposed, ReservationCommitted:
			if reservation.TxHash != txHash {
				return ErrStateConflict
			}
			return nil
		case ReservationReleased:
			return ErrStateConflict
		default:
			return ErrCorrupt
		}
	})
}

func (ledger *BudgetLedger) CommitFinalized(ctx context.Context, charge FinalizedCharge) (Reservation, error) {
	if charge.ReservationID == (ReservationID{}) || charge.TxHash == (common.Hash{}) || charge.Actual.IsZero() {
		return Reservation{}, ErrInvalidRequest
	}
	return ledger.transition(ctx, charge.ReservationID, charge.ObservedAt, func(reservation *Reservation, now time.Time) error {
		switch reservation.State {
		case ReservationExposed:
			if reservation.TxHash != charge.TxHash || charge.Actual.Gt(&reservation.Maximum) {
				return ErrStateConflict
			}
			reservation.State = ReservationCommitted
			reservation.Actual = charge.Actual
			reservation.UpdatedAt = now
			return nil
		case ReservationCommitted:
			if reservation.TxHash != charge.TxHash || !reservation.Actual.Eq(&charge.Actual) {
				return ErrStateConflict
			}
			return nil
		case ReservationHeld, ReservationReleased:
			return ErrStateConflict
		default:
			return ErrCorrupt
		}
	})
}

func (ledger *BudgetLedger) ReleaseProvenUnused(ctx context.Context, id ReservationID) (Reservation, error) {
	if id == (ReservationID{}) {
		return Reservation{}, ErrInvalidRequest
	}
	return ledger.transition(ctx, id, time.Time{}, func(reservation *Reservation, now time.Time) error {
		switch reservation.State {
		case ReservationHeld:
			reservation.State = ReservationReleased
			reservation.UpdatedAt = now
			return nil
		case ReservationReleased:
			return nil
		case ReservationExposed, ReservationCommitted:
			return ErrStateConflict
		default:
			return ErrCorrupt
		}
	})
}

// CheckSponsorCapacity revalidates the latest quorum balance against every
// open reservation for the network+sponsor immediately before a paid action.
func (ledger *BudgetLedger) CheckSponsorCapacity(ctx context.Context, id ReservationID, balance uint256.Int) error {
	if id == (ReservationID{}) || balance.IsZero() {
		return ErrInvalidRequest
	}
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		reservation, err := ledger.reservationByID(tx, id)
		if err != nil {
			return err
		}
		if reservation.State != ReservationHeld && reservation.State != ReservationExposed {
			return ErrStateConflict
		}
		lastSeen, err := decodeStoredTime(tx.Bucket(ledgerMetaBucket).Get(ledgerLastSeenKey))
		if err != nil {
			return ErrCorrupt
		}
		usage, err := ledger.collectUsage(tx, lastSeen)
		if err != nil {
			return err
		}
		policy := ledger.networks[reservation.Request.Network]
		reserved := usage.sponsors[sponsorKey{network: reservation.Request.Network, sponsor: reservation.Request.Sponsor}]
		if !sponsorHasCapacity(balance, policy.EmergencySponsorReserve, reserved, uint256.Int{}) {
			return ErrSponsorReserve
		}
		if reservation.State == ReservationHeld {
			reservation.Request.SponsorBalance = balance
			encoded, err := encodeReservation(reservation)
			if err != nil {
				return ErrCorrupt
			}
			return tx.Bucket(ledgerReservationsBucket).Put(id[:], encoded)
		}
		return nil
	})
	if err != nil {
		return ledger.publicError(err)
	}
	return nil
}

func (ledger *BudgetLedger) transition(ctx context.Context, id ReservationID, observed time.Time, update func(*Reservation, time.Time) error) (Reservation, error) {
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer finish()
	var reservation Reservation
	err = db.Update(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		var now time.Time
		if observed.IsZero() {
			now, err = decodeStoredTime(tx.Bucket(ledgerMetaBucket).Get(ledgerLastSeenKey))
		} else {
			chainObserved := normalizeTime(observed)
			localNow, nowErr := ledger.observedNow()
			if nowErr != nil || !validPersistentTime(chainObserved) || !withinObservedSkew(chainObserved, localNow) {
				return ErrInvalidRequest
			}
			now, err = ledger.effectiveNow(tx, localNow)
		}
		if err != nil {
			return err
		}
		if _, err := ledger.collectUsage(tx, now); err != nil {
			return err
		}
		reservation, err = ledger.reservationByID(tx, id)
		if err != nil {
			return err
		}
		before := reservation
		if err := update(&reservation, now); err != nil {
			return err
		}
		if reservation == before {
			return nil
		}
		if err := ledger.validateReservation(reservation); err != nil {
			return ErrCorrupt
		}
		encoded, err := encodeReservation(reservation)
		if err != nil {
			return ErrCorrupt
		}
		return tx.Bucket(ledgerReservationsBucket).Put(id[:], encoded)
	})
	if err != nil {
		return Reservation{}, ledger.publicError(err)
	}
	return reservation, nil
}

func (ledger *BudgetLedger) ReservationByAttempt(ctx context.Context, attempt Attempt) (Reservation, bool, error) {
	if !validAttempt(attempt) {
		return Reservation{}, false, ErrInvalidRequest
	}
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return Reservation{}, false, err
	}
	defer finish()
	var (
		reservation Reservation
		found       bool
	)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		value := tx.Bucket(ledgerAttemptsBucket).Get(attemptKey(attempt))
		if value == nil {
			return nil
		}
		if len(value) != len(ReservationID{}) {
			return ErrCorrupt
		}
		var id ReservationID
		copy(id[:], value)
		var err error
		reservation, err = ledger.reservationByID(tx, id)
		if err != nil || reservation.Request.Attempt != attempt {
			return ErrCorrupt
		}
		found = true
		return nil
	})
	if err != nil {
		return Reservation{}, false, ledger.publicError(err)
	}
	return reservation, found, nil
}

func (ledger *BudgetLedger) OpenReservations(ctx context.Context) ([]Reservation, error) {
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	reservations := make([]Reservation, 0)
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		lastSeen, err := decodeStoredTime(tx.Bucket(ledgerMetaBucket).Get(ledgerLastSeenKey))
		if err != nil {
			return ErrCorrupt
		}
		if _, err := ledger.collectUsage(tx, lastSeen); err != nil {
			return err
		}
		return tx.Bucket(ledgerReservationsBucket).ForEach(func(_, value []byte) error {
			reservation, err := decodeReservation(value)
			if err != nil {
				return ErrCorrupt
			}
			if reservation.State == ReservationHeld || reservation.State == ReservationExposed {
				reservations = append(reservations, reservation)
			}
			return nil
		})
	})
	if err != nil {
		return nil, ledger.publicError(err)
	}
	sortedReservations(reservations)
	return reservations, nil
}

func (ledger *BudgetLedger) Snapshot(ctx context.Context) (Snapshot, error) {
	db, finish, err := ledger.begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer finish()
	observed, err := ledger.observedNow()
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	err = db.View(func(tx *bolt.Tx) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		now, err := decodeStoredTime(tx.Bucket(ledgerMetaBucket).Get(ledgerLastSeenKey))
		if err != nil {
			return err
		}
		if observed.After(now) {
			now = observed
		}
		usage, err := ledger.collectUsage(tx, now)
		if err != nil {
			return err
		}
		global, err := makeScopeSnapshot(ledger.policy.Global, usage.global)
		if err != nil {
			return err
		}
		snapshot = Snapshot{At: now, Global: global, Networks: make(map[domain.NetworkID]ScopeSnapshot, len(ledger.networks))}
		for networkID, policy := range ledger.networks {
			network, err := makeScopeSnapshot(policy.Limits, usage.networks[networkID])
			if err != nil {
				return err
			}
			snapshot.Networks[networkID] = network
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, ledger.publicError(err)
	}
	return snapshot, nil
}

func (ledger *BudgetLedger) Close() error {
	ledger.mu.Lock()
	if ledger.closed {
		ledger.mu.Unlock()
		return nil
	}
	ledger.closed = true
	ledger.mu.Unlock()
	ledger.ops.Wait()
	if err := ledger.db.Close(); err != nil {
		return ErrClosed
	}
	return nil
}

func (ledger *BudgetLedger) begin(ctx context.Context) (*bolt.DB, func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, nil, err
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.closed {
		return nil, nil, ErrClosed
	}
	ledger.ops.Add(1)
	return ledger.db, ledger.ops.Done, nil
}

func (ledger *BudgetLedger) observedNow() (time.Time, error) {
	now := normalizeTime(ledger.now())
	if !validPersistentTime(now) {
		return time.Time{}, ErrInvalidRequest
	}
	return now, nil
}

func (ledger *BudgetLedger) effectiveNow(tx *bolt.Tx, observed time.Time) (time.Time, error) {
	meta := tx.Bucket(ledgerMetaBucket)
	lastSeen, err := decodeStoredTime(meta.Get(ledgerLastSeenKey))
	if err != nil {
		return time.Time{}, ErrCorrupt
	}
	if !observed.After(lastSeen) {
		return lastSeen, nil
	}
	if err := meta.Put(ledgerLastSeenKey, appendTime(nil, observed)); err != nil {
		return time.Time{}, err
	}
	return observed, nil
}

func decodeStoredTime(data []byte) (time.Time, error) {
	if len(data) != 12 {
		return time.Time{}, ErrCorrupt
	}
	offset := 0
	value, ok := readTime(data, &offset)
	if !ok || offset != len(data) || !validPersistentTime(value) {
		return time.Time{}, ErrCorrupt
	}
	return value, nil
}

func normalizeTime(value time.Time) time.Time { return value.Round(0).UTC() }

func withinObservedSkew(observed, local time.Time) bool {
	delta := observed.Sub(local)
	return delta >= -maximumObservedSkew && delta <= maximumObservedSkew
}

func (ledger *BudgetLedger) validateRequest(request ReservationRequest) error {
	network, ok := ledger.networks[request.Network]
	if !ok || request.Sponsor == (common.Address{}) || request.Sponsor != network.Sponsor || request.Candidate == (domain.CandidateID{}) ||
		!validAttempt(request.Attempt) || request.Quote.GasLimit == 0 || request.Quote.MaxFeePerGas.IsZero() || request.SponsorBalance.IsZero() {
		return ErrInvalidRequest
	}
	if _, err := request.Quote.Maximum(network.TransactionOverhead); err != nil {
		return err
	}
	return nil
}

func validAttempt(attempt Attempt) bool {
	return attempt.Incident != (domain.IncidentID{}) && attempt.Number > 0
}

func (ledger *BudgetLedger) validateReservation(reservation Reservation) error {
	if reservation.ID == (ReservationID{}) || reservation.ID != NewReservationID(reservation.Request.Attempt) ||
		ledger.validateRequest(reservation.Request) != nil || !validPersistentTime(reservation.CreatedAt) ||
		!validPersistentTime(reservation.UpdatedAt) || reservation.UpdatedAt.Before(reservation.CreatedAt) {
		return ErrCorrupt
	}
	network := ledger.networks[reservation.Request.Network]
	maximum, err := reservation.Request.Quote.Maximum(network.TransactionOverhead)
	if err != nil || !maximum.Eq(&reservation.Maximum) || reservation.Maximum.Gt(&ledger.policy.Global.PerTransaction) || reservation.Maximum.Gt(&network.Limits.PerTransaction) {
		return ErrCorrupt
	}
	switch reservation.State {
	case ReservationHeld:
		if reservation.TxHash != (common.Hash{}) || !reservation.Actual.IsZero() || !reservation.ExposedAt.IsZero() {
			return ErrCorrupt
		}
	case ReservationExposed:
		if reservation.TxHash == (common.Hash{}) || !reservation.Actual.IsZero() || !validPersistentTime(reservation.ExposedAt) ||
			reservation.ExposedAt.Before(reservation.CreatedAt) || reservation.UpdatedAt.Before(reservation.ExposedAt) {
			return ErrCorrupt
		}
	case ReservationCommitted:
		if reservation.TxHash == (common.Hash{}) || reservation.Actual.IsZero() || reservation.Actual.Gt(&reservation.Maximum) ||
			!validPersistentTime(reservation.ExposedAt) || reservation.ExposedAt.Before(reservation.CreatedAt) || reservation.UpdatedAt.Before(reservation.ExposedAt) {
			return ErrCorrupt
		}
	case ReservationReleased:
		if reservation.TxHash != (common.Hash{}) || !reservation.Actual.IsZero() || !reservation.ExposedAt.IsZero() {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func (ledger *BudgetLedger) reservationByID(tx *bolt.Tx, id ReservationID) (Reservation, error) {
	data := tx.Bucket(ledgerReservationsBucket).Get(id[:])
	if data == nil {
		return Reservation{}, ErrStateConflict
	}
	reservation, err := decodeReservation(data)
	if err != nil || reservation.ID != id || ledger.validateReservation(reservation) != nil {
		return Reservation{}, ErrCorrupt
	}
	return reservation, nil
}

func (ledger *BudgetLedger) purgeOldestReleased(tx *bolt.Tx) (bool, error) {
	reservations := tx.Bucket(ledgerReservationsBucket)
	attempts := tx.Bucket(ledgerAttemptsBucket)
	var selected *Reservation
	err := reservations.ForEach(func(_, value []byte) error {
		reservation, err := decodeReservation(value)
		if err != nil || ledger.validateReservation(reservation) != nil {
			return ErrCorrupt
		}
		if reservation.State != ReservationReleased {
			return nil
		}
		if selected == nil || reservation.UpdatedAt.Before(selected.UpdatedAt) ||
			(reservation.UpdatedAt.Equal(selected.UpdatedAt) && bytes.Compare(reservation.ID[:], selected.ID[:]) < 0) {
			copy := reservation
			selected = &copy
		}
		return nil
	})
	if err != nil || selected == nil {
		return false, err
	}
	if err := reservations.Delete(selected.ID[:]); err != nil {
		return false, err
	}
	if err := attempts.Delete(attemptKey(selected.Request.Attempt)); err != nil {
		return false, err
	}
	return true, nil
}

func (ledger *BudgetLedger) collectUsage(tx *bolt.Tx, now time.Time) (aggregateUsage, error) {
	usage := aggregateUsage{
		networks: make(map[domain.NetworkID]scopeUsage, len(ledger.networks)),
		sponsors: make(map[sponsorKey]uint256.Int, len(ledger.networks)),
	}
	reservations := tx.Bucket(ledgerReservationsBucket)
	attempts := tx.Bucket(ledgerAttemptsBucket)
	if reservations == nil || attempts == nil {
		return aggregateUsage{}, ErrCorrupt
	}
	reservationCount := 0
	err := reservations.ForEach(func(key, value []byte) error {
		if len(key) != len(ReservationID{}) {
			return ErrCorrupt
		}
		reservation, err := decodeReservation(value)
		if err != nil || !bytes.Equal(key, reservation.ID[:]) || ledger.validateReservation(reservation) != nil ||
			!bytes.Equal(attempts.Get(attemptKey(reservation.Request.Attempt)), reservation.ID[:]) {
			return ErrCorrupt
		}
		reservationCount++
		network := usage.networks[reservation.Request.Network]
		switch reservation.State {
		case ReservationHeld, ReservationExposed:
			if addChecked(&usage.global.reserved, reservation.Maximum) != nil || addChecked(&network.reserved, reservation.Maximum) != nil {
				return ErrCorrupt
			}
			key := sponsorKey{network: reservation.Request.Network, sponsor: reservation.Request.Sponsor}
			sponsorReserved := usage.sponsors[key]
			if addChecked(&sponsorReserved, reservation.Maximum) != nil {
				return ErrCorrupt
			}
			usage.sponsors[key] = sponsorReserved
		case ReservationCommitted:
			if addChecked(&usage.global.allSpent, reservation.Actual) != nil || addChecked(&network.allSpent, reservation.Actual) != nil {
				return ErrCorrupt
			}
			if !reservation.UpdatedAt.Before(now.Add(-24 * time.Hour)) {
				if addChecked(&usage.global.daySpent, reservation.Actual) != nil || addChecked(&network.daySpent, reservation.Actual) != nil {
					return ErrCorrupt
				}
			}
			if !reservation.UpdatedAt.Before(now.Add(-time.Hour)) {
				if addChecked(&usage.global.hourSpent, reservation.Actual) != nil || addChecked(&network.hourSpent, reservation.Actual) != nil {
					return ErrCorrupt
				}
			}
		case ReservationReleased:
		default:
			return ErrCorrupt
		}
		usage.networks[reservation.Request.Network] = network
		return nil
	})
	if err != nil {
		return aggregateUsage{}, err
	}
	attemptCount := 0
	err = attempts.ForEach(func(key, value []byte) error {
		attempt, ok := decodeAttemptKey(key)
		if !ok || !validAttempt(attempt) || len(value) != len(ReservationID{}) {
			return ErrCorrupt
		}
		reservation, err := decodeReservation(reservations.Get(value))
		if err != nil || reservation.Request.Attempt != attempt || !bytes.Equal(value, reservation.ID[:]) {
			return ErrCorrupt
		}
		attemptCount++
		return nil
	})
	if err != nil || attemptCount != reservationCount {
		return aggregateUsage{}, ErrCorrupt
	}
	if uint64(reservationCount) > uint64(ledger.maxRecords) {
		return aggregateUsage{}, ErrCorrupt
	}
	if !scopeWithinLimits(ledger.policy.Global, usage.global) {
		return aggregateUsage{}, ErrCorrupt
	}
	for networkID, networkPolicy := range ledger.networks {
		if !scopeWithinLimits(networkPolicy.Limits, usage.networks[networkID]) {
			return aggregateUsage{}, ErrCorrupt
		}
	}
	return usage, nil
}

func addChecked(target *uint256.Int, value uint256.Int) error {
	if _, overflow := target.AddOverflow(target, &value); overflow {
		return ErrArithmeticOverflow
	}
	return nil
}

func scopeWithinLimits(limits Limits, usage scopeUsage) bool {
	return fitsExisting(limits.PerHour, usage.hourSpent, usage.reserved) &&
		fitsExisting(limits.PerDay, usage.daySpent, usage.reserved) &&
		fitsExisting(limits.Cumulative, usage.allSpent, usage.reserved)
}

func fitsExisting(limit, spent, reserved uint256.Int) bool {
	if spent.Gt(&limit) {
		return false
	}
	var remaining uint256.Int
	remaining.Sub(&limit, &spent)
	return !reserved.Gt(&remaining)
}

func checkScopeBudget(limits Limits, usage scopeUsage, maximum uint256.Int) error {
	if maximum.Gt(&limits.PerTransaction) || !hasCapacity(limits.PerHour, usage.hourSpent, usage.reserved, maximum) ||
		!hasCapacity(limits.PerDay, usage.daySpent, usage.reserved, maximum) ||
		!hasCapacity(limits.Cumulative, usage.allSpent, usage.reserved, maximum) {
		return ErrBudgetExceeded
	}
	return nil
}

func hasCapacity(limit, spent, reserved, requested uint256.Int) bool {
	if spent.Gt(&limit) {
		return false
	}
	var remaining uint256.Int
	remaining.Sub(&limit, &spent)
	if reserved.Gt(&remaining) {
		return false
	}
	remaining.Sub(&remaining, &reserved)
	return !requested.Gt(&remaining)
}

func sponsorHasCapacity(balance, emergency, reserved, requested uint256.Int) bool {
	if emergency.Gt(&balance) {
		return false
	}
	var remaining uint256.Int
	remaining.Sub(&balance, &emergency)
	if reserved.Gt(&remaining) {
		return false
	}
	remaining.Sub(&remaining, &reserved)
	return !requested.Gt(&remaining)
}

func makeScopeSnapshot(limits Limits, usage scopeUsage) (ScopeSnapshot, error) {
	remainingHour, ok := remaining(limits.PerHour, usage.hourSpent, usage.reserved)
	if !ok {
		return ScopeSnapshot{}, ErrCorrupt
	}
	remainingDay, ok := remaining(limits.PerDay, usage.daySpent, usage.reserved)
	if !ok {
		return ScopeSnapshot{}, ErrCorrupt
	}
	remainingAll, ok := remaining(limits.Cumulative, usage.allSpent, usage.reserved)
	if !ok {
		return ScopeSnapshot{}, ErrCorrupt
	}
	reserved := Totals{PerHour: usage.reserved, PerDay: usage.reserved, Cumulative: usage.reserved}
	return ScopeSnapshot{
		Spent:     Totals{PerHour: usage.hourSpent, PerDay: usage.daySpent, Cumulative: usage.allSpent},
		Reserved:  reserved,
		Remaining: Totals{PerHour: remainingHour, PerDay: remainingDay, Cumulative: remainingAll},
	}, nil
}

func remaining(limit, spent, reserved uint256.Int) (uint256.Int, bool) {
	if spent.Gt(&limit) {
		return uint256.Int{}, false
	}
	var result uint256.Int
	result.Sub(&limit, &spent)
	if reserved.Gt(&result) {
		return uint256.Int{}, false
	}
	result.Sub(&result, &reserved)
	return result, true
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	return ctx.Err()
}

func (ledger *BudgetLedger) publicError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	for _, public := range []error{
		ErrClosed,
		ErrInvalidRequest,
		ErrArithmeticOverflow,
		ErrBudgetExceeded,
		ErrSponsorReserve,
		ErrStateConflict,
		ErrCapacity,
		ErrCorrupt,
	} {
		if errors.Is(err, public) {
			return public
		}
	}
	return ErrCorrupt
}
