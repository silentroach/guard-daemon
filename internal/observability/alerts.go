package observability

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"guard-daemon/internal/domain"
)

const MaxAlertEntries = 4096

var (
	ErrInvalidAlertConfig = errors.New("настройки alert manager недопустимы")
	ErrInvalidAlert       = errors.New("alert имеет недопустимый chain ID или код")
	ErrAlertCapacity      = errors.New("достигнут предел записей alert manager")
	ErrAlertPersistence   = errors.New("persistent alert state недоступен")
	ErrAlertDelivery      = errors.New("доставка alert не подтверждена")
)

// AlertCode является закрытым набором причин operator alert.
type AlertCode uint8

const (
	AlertRPCDegraded AlertCode = iota + 1
	AlertBudgetBlocked
	AlertAmbiguousRescue
	AlertPaidActionsStopped
	AlertDelegationUnexpected
	AlertReconciliationStale
)

func (code AlertCode) String() string {
	switch code {
	case AlertRPCDegraded:
		return "rpc_degraded"
	case AlertBudgetBlocked:
		return "budget_blocked"
	case AlertAmbiguousRescue:
		return "ambiguous_rescue"
	case AlertPaidActionsStopped:
		return "paid_actions_stopped"
	case AlertDelegationUnexpected:
		return "delegation_unexpected"
	case AlertReconciliationStale:
		return "reconciliation_stale"
	default:
		return "unknown"
	}
}

type AlertState uint8

const (
	AlertFiring AlertState = iota + 1
	AlertResolved
)

// Alert не содержит свободного message и поэтому безопасен для внешнего sink.
type Alert struct {
	ChainID domain.NetworkID
	Code    AlertCode
	State   AlertState
	At      time.Time
}

type AlertSink interface {
	Notify(Alert) error
}

type AlertSinkFunc func(Alert) error

func (notify AlertSinkFunc) Notify(alert Alert) error {
	return notify(alert)
}

type AlertClock interface {
	Now() time.Time
}

type AlertManagerConfig struct {
	Cooldown  time.Duration
	Capacity  int
	StatePath string
}

type AlertStatus struct {
	ChainID      domain.NetworkID
	Code         AlertCode
	Active       bool
	LastNotified time.Time
}

type alertKey struct {
	chainID domain.NetworkID
	code    AlertCode
}

type alertEntry struct {
	active       bool
	lastNotified time.Time
	touched      time.Time
	pendingState AlertState
	pendingAt    time.Time
	delivering   bool
}

type committedAlertPersistenceError struct{}

func (committedAlertPersistenceError) Error() string { return ErrAlertPersistence.Error() }
func (committedAlertPersistenceError) Unwrap() error { return ErrAlertPersistence }

// AlertManager дедуплицирует alerts по chain+code. Sink всегда вызывается без
// удержания внутренней блокировки и может безопасно читать/resolve manager.
type AlertManager struct {
	mu        sync.RWMutex
	clock     AlertClock
	sink      AlertSink
	cooldown  time.Duration
	capacity  int
	entries   map[alertKey]*alertEntry
	statePath string
}

func NewAlertManager(config AlertManagerConfig, clock AlertClock, sink AlertSink) (*AlertManager, error) {
	if config.Cooldown <= 0 || config.Capacity <= 0 || config.Capacity > MaxAlertEntries || clock == nil {
		return nil, ErrInvalidAlertConfig
	}
	if sink == nil {
		sink = AlertSinkFunc(func(Alert) error { return nil })
	}
	manager := &AlertManager{
		clock:     clock,
		sink:      sink,
		cooldown:  config.Cooldown,
		capacity:  config.Capacity,
		entries:   make(map[alertKey]*alertEntry, config.Capacity),
		statePath: config.StatePath,
	}
	if err := manager.loadState(); err != nil {
		return nil, err
	}
	if err := manager.replayPending(); err != nil {
		return nil, err
	}
	return manager, nil
}

// Raise активирует alert и возвращает true, только если sink получил initial
// notification либо cooldown reminder.
func (manager *AlertManager) Raise(chainID domain.NetworkID, code AlertCode) (bool, error) {
	if chainID <= 0 || !validAlertCode(code) {
		return false, ErrInvalidAlert
	}
	now := manager.now()
	key := alertKey{chainID: chainID, code: code}
	if err := manager.deliverPending(key); err != nil {
		return false, err
	}

	manager.mu.Lock()
	before := cloneAlertEntries(manager.entries)
	entry, exists := manager.entries[key]
	if !exists {
		if len(manager.entries) == manager.capacity && !manager.evictResolvedLocked() {
			manager.mu.Unlock()
			return false, ErrAlertCapacity
		}
		entry = &alertEntry{}
		manager.entries[key] = entry
	}
	wasActive := entry.active
	entry.active = true
	entry.touched = now
	lastNotification := entry.lastNotified
	if entry.pendingAt.After(lastNotification) {
		lastNotification = entry.pendingAt
	}
	emit := !wasActive || lastNotification.IsZero() || !now.Before(lastNotification.Add(manager.cooldown))
	if emit {
		entry.pendingState = AlertFiring
		entry.pendingAt = now
	}
	if err := manager.persistLocked(); err != nil {
		if !persistenceMayHaveCommitted(err) {
			manager.entries = before
		}
		manager.mu.Unlock()
		return false, err
	}
	manager.mu.Unlock()

	if emit {
		if err := manager.deliverPending(key); err != nil {
			return false, err
		}
	}
	return emit, nil
}

// Resolve посылает ровно одно resolved notification для активной записи.
func (manager *AlertManager) Resolve(chainID domain.NetworkID, code AlertCode) (bool, error) {
	if chainID <= 0 || !validAlertCode(code) {
		return false, ErrInvalidAlert
	}
	now := manager.now()
	key := alertKey{chainID: chainID, code: code}
	if err := manager.deliverPending(key); err != nil {
		return false, err
	}

	manager.mu.Lock()
	before := cloneAlertEntries(manager.entries)
	entry, exists := manager.entries[key]
	if !exists || !entry.active {
		manager.mu.Unlock()
		return false, nil
	}
	entry.active = false
	entry.touched = now
	entry.pendingState = AlertResolved
	entry.pendingAt = now
	if err := manager.persistLocked(); err != nil {
		if !persistenceMayHaveCommitted(err) {
			manager.entries = before
		}
		manager.mu.Unlock()
		return false, err
	}
	manager.mu.Unlock()

	if err := manager.deliverPending(key); err != nil {
		return false, err
	}
	return true, nil
}

func (manager *AlertManager) replayPending() error {
	keys := make([]alertKey, 0, len(manager.entries))
	for key, entry := range manager.entries {
		if entry.pendingState != 0 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return alertKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		if err := manager.deliverPending(key); err != nil {
			return err
		}
	}
	return nil
}

func (manager *AlertManager) deliverPending(key alertKey) error {
	for {
		manager.mu.Lock()
		entry := manager.entries[key]
		if entry == nil || entry.pendingState == 0 || entry.delivering {
			manager.mu.Unlock()
			return nil
		}
		alert := Alert{ChainID: key.chainID, Code: key.code, State: entry.pendingState, At: entry.pendingAt}
		entry.delivering = true
		manager.mu.Unlock()

		deliveryErr := manager.sink.Notify(alert)
		manager.mu.Lock()
		entry = manager.entries[key]
		if entry == nil {
			manager.mu.Unlock()
			return ErrAlertPersistence
		}
		entry.delivering = false
		if deliveryErr != nil {
			manager.mu.Unlock()
			return ErrAlertDelivery
		}
		if entry.pendingState == alert.State && entry.pendingAt.Equal(alert.At) {
			before := *entry
			entry.lastNotified = alert.At
			entry.pendingState = 0
			entry.pendingAt = time.Time{}
			if err := manager.persistLocked(); err != nil {
				if !persistenceMayHaveCommitted(err) {
					*entry = before
				}
				manager.mu.Unlock()
				return err
			}
		}
		more := entry.pendingState != 0
		manager.mu.Unlock()
		if !more {
			return nil
		}
	}
}

func cloneAlertEntries(entries map[alertKey]*alertEntry) map[alertKey]*alertEntry {
	cloned := make(map[alertKey]*alertEntry, len(entries))
	for key, entry := range entries {
		copy := *entry
		cloned[key] = &copy
	}
	return cloned
}

func persistenceMayHaveCommitted(err error) bool {
	var committed committedAlertPersistenceError
	return errors.As(err, &committed)
}

func (manager *AlertManager) Active(chainID domain.NetworkID, code AlertCode) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	entry, exists := manager.entries[alertKey{chainID: chainID, code: code}]
	return exists && entry.active
}

// Snapshot возвращает упорядоченную копию bounded alert state.
func (manager *AlertManager) Snapshot() []AlertStatus {
	manager.mu.RLock()
	snapshot := make([]AlertStatus, 0, len(manager.entries))
	for key, entry := range manager.entries {
		snapshot = append(snapshot, AlertStatus{
			ChainID:      key.chainID,
			Code:         key.code,
			Active:       entry.active,
			LastNotified: entry.lastNotified,
		})
	}
	manager.mu.RUnlock()
	sort.Slice(snapshot, func(left, right int) bool {
		if snapshot[left].ChainID == snapshot[right].ChainID {
			return snapshot[left].Code < snapshot[right].Code
		}
		return snapshot[left].ChainID < snapshot[right].ChainID
	})
	return snapshot
}

func (manager *AlertManager) evictResolvedLocked() bool {
	var selected alertKey
	var selectedEntry *alertEntry
	for key, entry := range manager.entries {
		if entry.active {
			continue
		}
		if selectedEntry == nil || entry.touched.Before(selectedEntry.touched) ||
			(entry.touched.Equal(selectedEntry.touched) && alertKeyLess(key, selected)) {
			selected = key
			selectedEntry = entry
		}
	}
	if selectedEntry == nil {
		return false
	}
	delete(manager.entries, selected)
	return true
}

func (manager *AlertManager) now() time.Time {
	return manager.clock.Now().UTC().Round(0)
}

func alertKeyLess(left, right alertKey) bool {
	if left.chainID == right.chainID {
		return left.code < right.code
	}
	return left.chainID < right.chainID
}

func validAlertCode(code AlertCode) bool {
	return code >= AlertRPCDegraded && code <= AlertReconciliationStale
}

type persistedAlert struct {
	ChainID      domain.NetworkID `json:"chain_id"`
	Code         AlertCode        `json:"code"`
	Active       bool             `json:"active"`
	LastNotified time.Time        `json:"last_notified"`
	Touched      time.Time        `json:"touched"`
	PendingState AlertState       `json:"pending_state,omitempty"`
	PendingAt    time.Time        `json:"pending_at,omitempty"`
}

func (manager *AlertManager) loadState() error {
	if manager.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(manager.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrAlertPersistence
	}
	var records []persistedAlert
	if json.Unmarshal(data, &records) != nil || len(records) > manager.capacity {
		return ErrAlertPersistence
	}
	for _, record := range records {
		pendingValid := record.PendingState == 0 && record.PendingAt.IsZero() ||
			(record.PendingState == AlertFiring || record.PendingState == AlertResolved) && !record.PendingAt.IsZero()
		if record.ChainID <= 0 || !validAlertCode(record.Code) || record.Touched.IsZero() || !pendingValid ||
			record.LastNotified.IsZero() && record.PendingState == 0 {
			return ErrAlertPersistence
		}
		key := alertKey{chainID: record.ChainID, code: record.Code}
		if _, duplicate := manager.entries[key]; duplicate {
			return ErrAlertPersistence
		}
		manager.entries[key] = &alertEntry{
			active: record.Active, lastNotified: record.LastNotified.UTC(), touched: record.Touched.UTC(),
			pendingState: record.PendingState, pendingAt: record.PendingAt.UTC(),
		}
	}
	return nil
}

func (manager *AlertManager) persistLocked() error {
	if manager.statePath == "" {
		return nil
	}
	records := make([]persistedAlert, 0, len(manager.entries))
	for key, entry := range manager.entries {
		records = append(records, persistedAlert{
			ChainID: key.chainID, Code: key.code, Active: entry.active,
			LastNotified: entry.lastNotified, Touched: entry.touched,
			PendingState: entry.pendingState, PendingAt: entry.pendingAt,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ChainID == records[j].ChainID {
			return records[i].Code < records[j].Code
		}
		return records[i].ChainID < records[j].ChainID
	})
	data, err := json.Marshal(records)
	if err != nil {
		return ErrAlertPersistence
	}
	if err := os.MkdirAll(filepath.Dir(manager.statePath), 0o700); err != nil {
		return ErrAlertPersistence
	}
	info, err := os.Lstat(filepath.Dir(manager.statePath))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || os.Chmod(filepath.Dir(manager.statePath), 0o700) != nil {
		return ErrAlertPersistence
	}
	temporary := manager.statePath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return ErrAlertPersistence
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return ErrAlertPersistence
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return ErrAlertPersistence
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return ErrAlertPersistence
	}
	if err := os.Rename(temporary, manager.statePath); err != nil {
		_ = os.Remove(temporary)
		return ErrAlertPersistence
	}
	directory, err := os.Open(filepath.Dir(manager.statePath))
	if err != nil {
		return committedAlertPersistenceError{}
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return committedAlertPersistenceError{}
	}
	return nil
}
