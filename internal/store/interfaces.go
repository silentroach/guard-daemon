package store

import (
	"context"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

// CandidateQueue сохраняет candidate до возврата из Put. Повторный Put с тем
// же stable ID не создаёт новую работу, включая промежуток после Ack и до
// продвижения checkpoint. Nack сохраняет связь с incident и откладывает работу
// до retryAt, не позволяя одной ошибке блокировать готовую очередь.
type CandidateQueue interface {
	Put(context.Context, domain.RescueCandidate) (PutResult, error)
	Next(context.Context, domain.NetworkID) (domain.RescueCandidate, error)
	Replay(context.Context, domain.NetworkID) ([]domain.RescueCandidate, error)
	Nack(context.Context, domain.CandidateID, domain.IncidentID, time.Time) error
	Ack(context.Context, domain.CandidateID, domain.IncidentID) error
}

// IncidentStore идемпотентно сохраняет связь candidate с incident. При replay
// совпадение ID, Candidate и Network определяет ту же запись; исходный CreatedAt
// сохраняется и не заменяется временем повторной обработки.
type IncidentStore interface {
	PutIncident(context.Context, Incident) error
	IncidentByCandidate(context.Context, domain.CandidateID) (Incident, bool, error)
}

// RescueStateStore хранит transaction state отдельно от watcher handoff.
// PutRescueIncident идемпотентен по ID, UpdateRescueIncident принимает только
// допустимый переход, а RescueIncidents возвращает снимок для restart/nonce
// reconciliation.
type RescueStateStore interface {
	PutRescueIncident(context.Context, RescueIncident) (RescueIncident, error)
	UpdateRescueIncident(context.Context, RescueIncident) error
	RescueIncident(context.Context, domain.IncidentID) (RescueIncident, bool, error)
	RescueIncidents(context.Context, domain.NetworkID) ([]RescueIncident, error)
	NonceFloor(context.Context) (uint64, error)
	RaiseNonceFloor(context.Context, uint64) error
	PruneRescueIncidents(context.Context, int) error
}

// CheckpointStore разделяет scan cursor и подтверждённый checkpoint. Scanner
// может продолжать durable backfill, пока checkpoint ждёт incident и Ack.
type CheckpointStore interface {
	LoadCheckpoint(context.Context, domain.NetworkID) (Checkpoint, bool, error)
	LoadScanCursor(context.Context, domain.NetworkID) (Checkpoint, bool, error)
	CommitCanonicalBlock(context.Context, CanonicalBlock) error
}

// ObservationStore хранит provisional WebSocket observations. Они не доступны
// consumer до подтверждения canonical scanner и могут быть отозваны Removed log.
type ObservationStore interface {
	PutObserved(context.Context, domain.RescueCandidate) (PutResult, error)
	MarkRemoved(context.Context, domain.CandidateID) error
	DiscoveredTokens(context.Context, domain.NetworkID) ([]common.Address, error)
	DiscoveryOverflowed(context.Context, domain.NetworkID) (bool, error)
}

// HandoffStore объединяет queue, incidents и watcher state в одной
// транзакционной границе.
type HandoffStore interface {
	CandidateQueue
	IncidentStore
	RescueStateStore
	CheckpointStore
	ObservationStore
	LeaseManager
	Close() error
}

type LeaseManager interface {
	Acquire(context.Context, LeaseKey, string, time.Duration) (Lease, error)
	Renew(context.Context, Lease, time.Duration) (Lease, error)
	Validate(context.Context, Lease) error
	Lost(Lease) <-chan struct{}
	Release(context.Context, Lease) error
}
