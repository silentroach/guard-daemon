package store

import (
	"context"
	"time"

	"guard-daemon/internal/domain"
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

// CheckpointStore продвигается только после Put candidate, PutIncident и Ack.
type CheckpointStore interface {
	LoadCheckpoint(context.Context, domain.NetworkID) (Checkpoint, bool, error)
	AdvanceCheckpoint(context.Context, Checkpoint) error
}

type LeaseManager interface {
	Acquire(context.Context, LeaseKey, string, time.Duration) (Lease, error)
	Renew(context.Context, Lease, time.Duration) (Lease, error)
	Lost(Lease) <-chan struct{}
	Release(context.Context, Lease) error
}
