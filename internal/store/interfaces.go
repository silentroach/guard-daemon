package store

import (
	"context"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

// CandidateQueue гарантирует, что к моменту возврата из Put кандидат сохранён. Повторный Put
// с тем же неизменным ID не добавляет задание заново, в том числе после Ack и до
// продвижения контрольной точки. Nack сохраняет связь с инцидентом и откладывает
// обработку до retryAt, чтобы ошибка одного кандидата не задерживала остальных
// готовых кандидатов.
type CandidateQueue interface {
	Put(context.Context, domain.RescueCandidate) (PutResult, error)
	Next(context.Context, domain.NetworkID) (domain.RescueCandidate, error)
	Replay(context.Context, domain.NetworkID) ([]domain.RescueCandidate, error)
	Nack(context.Context, domain.CandidateID, domain.IncidentID, time.Time) error
	Ack(context.Context, domain.CandidateID, domain.IncidentID) error
}

// IncidentStore идемпотентно сохраняет связь кандидата с инцидентом. При повторной
// обработке запись считается той же, если совпадают ID, Candidate и Network; исходное
// значение CreatedAt сохраняется.
type IncidentStore interface {
	PutIncident(context.Context, Incident) error
	IncidentByCandidate(context.Context, domain.CandidateID) (Incident, bool, error)
}

// RescueStateStore хранит состояние операций восстановления отдельно от очереди наблюдателя.
// PutRescueIncident идемпотентен по ID, UpdateRescueIncident принимает только
// допустимый переход, а RescueIncidents возвращает снимок для повторной проверки
// операций после перезапуска и восстановления нижней границы nonce.
type RescueStateStore interface {
	PutRescueIncident(context.Context, RescueIncident) (RescueIncident, error)
	UpdateRescueIncident(context.Context, RescueIncident) error
	RescueIncident(context.Context, domain.IncidentID) (RescueIncident, bool, error)
	RescueIncidents(context.Context, domain.NetworkID) ([]RescueIncident, error)
	NonceFloor(context.Context) (uint64, error)
	RaiseNonceFloor(context.Context, uint64) error
	PruneRescueIncidents(context.Context, int) error
}

// CheckpointStore отдельно хранит позицию сканирования и подтверждённую контрольную
// точку. Сканер может сохранять прогресс поиска пропущенных блоков, а продвижение
// контрольной точки откладывается до создания инцидента и вызова Ack.
type CheckpointStore interface {
	LoadCheckpoint(context.Context, domain.NetworkID) (Checkpoint, bool, error)
	LoadScanCursor(context.Context, domain.NetworkID) (Checkpoint, bool, error)
	CommitCanonicalBlock(context.Context, CanonicalBlock) error
}

// ObservationStore хранит предварительные наблюдения WebSocket. Они недоступны
// обработчику до подтверждения каноническим сканером и удаляются после получения
// лога с признаком Removed.
type ObservationStore interface {
	PutObserved(context.Context, domain.RescueCandidate) (PutResult, error)
	MarkRemoved(context.Context, domain.CandidateID) error
	DiscoveredTokens(context.Context, domain.NetworkID) ([]common.Address, error)
	DiscoveryOverflowed(context.Context, domain.NetworkID) (bool, error)
}

// HandoffStore предоставляет единое транзакционное хранилище очереди, инцидентов
// и состояния наблюдателя.
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
