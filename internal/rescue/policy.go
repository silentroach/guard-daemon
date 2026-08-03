package rescue

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

var (
	ErrInvalidAdmissionConfig  = errors.New("rescue admission: некорректная конфигурация")
	ErrInvalidAdmissionRequest = errors.New("rescue admission: некорректный запрос")
	ErrAdmissionDenied         = errors.New("rescue admission: попытка отклонена policy")
	ErrAdmissionPersistence    = errors.New("rescue admission: persistent state недоступен")
)

type AdmissionReason string

const (
	AdmissionAllowed                 AdmissionReason = "allowed"
	AdmissionGlobalRateLimited       AdmissionReason = "global_rate_limited"
	AdmissionTokenAttemptsLimited    AdmissionReason = "token_attempts_limited"
	AdmissionSourceEventLimited      AdmissionReason = "source_event_attempts_limited"
	AdmissionUnknownTokenRateLimited AdmissionReason = "new_unknown_tokens_limited"
	AdmissionCapacityLimited         AdmissionReason = "capacity_limited"
	AdmissionInvalidRequest          AdmissionReason = "invalid_request"
	AdmissionAttemptIdentityConflict AdmissionReason = "attempt_identity_conflict"
)

// AdmissionClock is deliberately smaller than the service clock: admission
// performs no sleeps and starts no background work.
type AdmissionClock interface {
	Now() time.Time
}

type AdmissionConfig struct {
	Window                    time.Duration
	RateWindow                time.Duration
	RateLimit                 uint32
	MaxAttemptsPerToken       uint32
	MaxAttemptsPerSourceEvent uint32
	MaxNewUnknownTokens       uint32
	Capacity                  int
}

type AdmissionRequest struct {
	Network     domain.NetworkID
	Incident    domain.IncidentID
	Attempt     uint32
	Token       common.Address
	Parent      domain.IncidentID
	SourceEvent domain.CandidateID
	Unknown     bool
	ObservedAt  time.Time
}

type AdmissionDecision struct {
	Allowed bool
	Reason  AdmissionReason
	RetryAt time.Time
}

// AdmissionError is retryable only for rolling-window limits. Invalid or
// conflicting identities require caller intervention instead of a timed retry.
type AdmissionError struct {
	Reason    AdmissionReason
	RetryAt   time.Time
	Retryable bool
	cause     error
}

func (err *AdmissionError) Error() string {
	return "rescue admission: " + string(err.Reason)
}

func (err *AdmissionError) Unwrap() error {
	return err.cause
}

func (err *AdmissionError) NextRetryAt() time.Time {
	return err.RetryAt
}

type admissionAttemptKey struct {
	network  domain.NetworkID
	incident domain.IncidentID
	attempt  uint32
}

type admissionTokenKey struct {
	network domain.NetworkID
	token   common.Address
}

type admissionSourceEventKey struct {
	network domain.NetworkID
	event   domain.CandidateID
}

type admissionRecord struct {
	request    AdmissionRequest
	admittedAt time.Time
}

// AdmissionController is an in-memory rolling-window guard. Its state is
// bounded by Capacity and it never creates timers or goroutines.
type AdmissionController struct {
	mu          sync.Mutex
	config      AdmissionConfig
	clock       AdmissionClock
	lastNow     time.Time
	records     list.List
	attempts    map[admissionAttemptKey]*list.Element
	tokens      map[admissionTokenKey]uint32
	events      map[admissionSourceEventKey]uint32
	unknown     map[admissionTokenKey]uint32
	latest      map[admissionTokenKey]time.Time
	persistence admissionPersistence
}

const maximumAdmissionObservedSkew = 2 * time.Minute

func NewAdmissionController(config AdmissionConfig, clock AdmissionClock) (*AdmissionController, error) {
	if config.RateWindow == 0 {
		config.RateWindow = config.Window
	}
	if clock == nil || config.Window <= 0 || config.RateLimit == 0 || config.MaxAttemptsPerToken == 0 ||
		config.RateWindow <= 0 || config.RateWindow > config.Window || config.MaxAttemptsPerSourceEvent == 0 || config.MaxNewUnknownTokens == 0 || config.Capacity <= 0 {
		return nil, ErrInvalidAdmissionConfig
	}
	return &AdmissionController{
		config:   config,
		clock:    clock,
		attempts: make(map[admissionAttemptKey]*list.Element, config.Capacity),
		tokens:   make(map[admissionTokenKey]uint32),
		events:   make(map[admissionSourceEventKey]uint32),
		unknown:  make(map[admissionTokenKey]uint32),
		latest:   make(map[admissionTokenKey]time.Time),
	}, nil
}

// Admit accepts an attempt at most once per network+incident+attempt key. An
// accepted replay returns Allowed without consuming any additional quota.
func (controller *AdmissionController) Admit(request AdmissionRequest) (AdmissionDecision, error) {
	if controller == nil {
		return admissionFailure(AdmissionInvalidRequest, time.Time{}, false, ErrInvalidAdmissionRequest)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()

	now := controller.clock.Now().UTC().Round(0)
	chainObserved := request.ObservedAt.UTC().Round(0)
	request.ObservedAt = time.Time{}
	if !chainObserved.IsZero() {
		delta := chainObserved.Sub(now)
		if delta < -maximumAdmissionObservedSkew || delta > maximumAdmissionObservedSkew {
			return admissionFailure(AdmissionInvalidRequest, time.Time{}, false, ErrInvalidAdmissionRequest)
		}
	}
	if now.Before(controller.lastNow) {
		now = controller.lastNow
	} else {
		controller.lastNow = now
	}
	controller.prune(now)
	if controller.persistence != nil {
		if err := controller.persistence.Store(&controller.records); err != nil {
			return admissionFailure(AdmissionCapacityLimited, time.Time{}, false, ErrAdmissionPersistence)
		}
	}

	if !validAdmissionRequest(request) {
		return admissionFailure(AdmissionInvalidRequest, time.Time{}, false, ErrInvalidAdmissionRequest)
	}
	attemptKey := admissionAttemptKey{network: request.Network, incident: request.Incident, attempt: request.Attempt}
	if existing, found := controller.attempts[attemptKey]; found {
		if existing.Value.(*admissionRecord).request != request {
			return admissionFailure(AdmissionAttemptIdentityConflict, time.Time{}, false, ErrInvalidAdmissionRequest)
		}
		return AdmissionDecision{Allowed: true, Reason: AdmissionAllowed}, nil
	}

	tokenKey := admissionTokenKey{network: request.Network, token: request.Token}
	eventKey := admissionSourceEventKey{network: request.Network, event: request.SourceEvent}
	if controller.rateCount(now) >= uint64(controller.config.RateLimit) {
		return admissionFailure(AdmissionGlobalRateLimited, controller.rateRetryAt(now), true, ErrAdmissionDenied)
	}
	if controller.tokens[tokenKey] >= controller.config.MaxAttemptsPerToken {
		return admissionFailure(AdmissionTokenAttemptsLimited, controller.tokenRetryAt(tokenKey), true, ErrAdmissionDenied)
	}
	if controller.events[eventKey] >= controller.config.MaxAttemptsPerSourceEvent {
		return admissionFailure(AdmissionSourceEventLimited, controller.sourceEventRetryAt(eventKey), true, ErrAdmissionDenied)
	}
	if request.Unknown && controller.unknown[tokenKey] == 0 && uint64(len(controller.unknown)) >= uint64(controller.config.MaxNewUnknownTokens) {
		return admissionFailure(AdmissionUnknownTokenRateLimited, controller.unknownRetryAt(), true, ErrAdmissionDenied)
	}
	if controller.records.Len() >= controller.config.Capacity {
		return admissionFailure(AdmissionCapacityLimited, controller.firstExpiry(), true, ErrAdmissionDenied)
	}

	record := &admissionRecord{request: request, admittedAt: now}
	element := controller.records.PushBack(record)
	controller.attempts[attemptKey] = element
	controller.tokens[tokenKey]++
	controller.events[eventKey]++
	if request.Unknown {
		controller.unknown[tokenKey]++
		controller.latest[tokenKey] = now
	}
	if controller.persistence != nil {
		if err := controller.persistence.Store(&controller.records); err != nil {
			controller.remove(element, record)
			return admissionFailure(AdmissionCapacityLimited, time.Time{}, false, ErrAdmissionPersistence)
		}
	}
	return AdmissionDecision{Allowed: true, Reason: AdmissionAllowed}, nil
}

func (controller *AdmissionController) Close() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.persistence == nil {
		return nil
	}
	err := controller.persistence.Close()
	controller.persistence = nil
	return err
}

type admissionPersistence interface {
	Store(*list.List) error
	Close() error
}

// TrackedAttempts returns current rolling-window state after synchronous
// pruning. It is useful for bounded-state metrics and tests.
func (controller *AdmissionController) TrackedAttempts() int {
	if controller == nil {
		return 0
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.clock.Now()
	if now.Before(controller.lastNow) {
		now = controller.lastNow
	} else {
		controller.lastNow = now
	}
	controller.prune(now)
	return controller.records.Len()
}

func validAdmissionRequest(request AdmissionRequest) bool {
	return request.Network > 0 && request.Incident != (domain.IncidentID{}) && request.Attempt > 0 &&
		request.Parent != (domain.IncidentID{}) && request.SourceEvent != (domain.CandidateID{}) &&
		(!request.Unknown || request.Token != (common.Address{}))
}

func admissionFailure(reason AdmissionReason, retryAt time.Time, retryable bool, cause error) (AdmissionDecision, error) {
	decision := AdmissionDecision{Reason: reason, RetryAt: retryAt}
	return decision, &AdmissionError{Reason: reason, RetryAt: retryAt, Retryable: retryable, cause: cause}
}

func (controller *AdmissionController) prune(now time.Time) {
	for element := controller.records.Front(); element != nil; element = controller.records.Front() {
		record := element.Value.(*admissionRecord)
		if now.Before(record.admittedAt.Add(controller.config.Window)) {
			return
		}
		controller.remove(element, record)
	}
}

func (controller *AdmissionController) remove(element *list.Element, record *admissionRecord) {
	request := record.request
	delete(controller.attempts, admissionAttemptKey{network: request.Network, incident: request.Incident, attempt: request.Attempt})
	decrementAdmissionCount(controller.tokens, admissionTokenKey{network: request.Network, token: request.Token})
	decrementAdmissionCount(controller.events, admissionSourceEventKey{network: request.Network, event: request.SourceEvent})
	if request.Unknown {
		key := admissionTokenKey{network: request.Network, token: request.Token}
		decrementAdmissionCount(controller.unknown, key)
		if controller.unknown[key] == 0 {
			delete(controller.latest, key)
		} else if controller.latest[key].Equal(record.admittedAt) {
			controller.recomputeLatest(key)
		}
	}
	controller.records.Remove(element)
}

func decrementAdmissionCount[Key comparable](counts map[Key]uint32, key Key) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}

func (controller *AdmissionController) recomputeLatest(key admissionTokenKey) {
	var latest time.Time
	for element := controller.records.Front(); element != nil; element = element.Next() {
		record := element.Value.(*admissionRecord)
		if record.request.Unknown && record.request.Network == key.network && record.request.Token == key.token && record.admittedAt.After(latest) {
			latest = record.admittedAt
		}
	}
	controller.latest[key] = latest
}

func (controller *AdmissionController) firstExpiry() time.Time {
	return controller.records.Front().Value.(*admissionRecord).admittedAt.Add(controller.config.Window)
}

func (controller *AdmissionController) rateCount(now time.Time) uint64 {
	cutoff := now.Add(-controller.config.RateWindow)
	var count uint64
	for element := controller.records.Back(); element != nil; element = element.Prev() {
		record := element.Value.(*admissionRecord)
		if !record.admittedAt.After(cutoff) {
			break
		}
		count++
	}
	return count
}

func (controller *AdmissionController) rateRetryAt(now time.Time) time.Time {
	cutoff := now.Add(-controller.config.RateWindow)
	for element := controller.records.Front(); element != nil; element = element.Next() {
		record := element.Value.(*admissionRecord)
		if record.admittedAt.After(cutoff) {
			return record.admittedAt.Add(controller.config.RateWindow)
		}
	}
	return now.Add(controller.config.RateWindow)
}

func (controller *AdmissionController) tokenRetryAt(key admissionTokenKey) time.Time {
	for element := controller.records.Front(); element != nil; element = element.Next() {
		record := element.Value.(*admissionRecord)
		if record.request.Network == key.network && record.request.Token == key.token {
			return record.admittedAt.Add(controller.config.Window)
		}
	}
	return time.Time{}
}

func (controller *AdmissionController) sourceEventRetryAt(key admissionSourceEventKey) time.Time {
	for element := controller.records.Front(); element != nil; element = element.Next() {
		record := element.Value.(*admissionRecord)
		if record.request.Network == key.network && record.request.SourceEvent == key.event {
			return record.admittedAt.Add(controller.config.Window)
		}
	}
	return time.Time{}
}

func (controller *AdmissionController) unknownRetryAt() time.Time {
	var retryAt time.Time
	for _, latest := range controller.latest {
		expires := latest.Add(controller.config.Window)
		if retryAt.IsZero() || expires.Before(retryAt) {
			retryAt = expires
		}
	}
	return retryAt
}
