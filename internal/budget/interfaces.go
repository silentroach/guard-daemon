package budget

import (
	"context"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

type ReservationID string

type Attempt struct {
	Incident domain.IncidentID
	Number   uint32
}

type ReservationRequest struct {
	Network   domain.NetworkID
	Sponsor   common.Address
	Candidate domain.CandidateID
	Attempt   Attempt
	Maximum   uint256.Int
}

type Reservation struct {
	ID        ReservationID
	Request   ReservationRequest
	CreatedAt time.Time
}

// Ledger является единым persistent ledger для всех сетей процесса.
// Денежные значения передаются как value-type uint256.Int: реализация обязана
// хранить собственную копию request и обеспечивать идемпотентность Attempt.
type Ledger interface {
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	CommitActual(context.Context, ReservationID, uint256.Int) error
	ReleaseProvenUnused(context.Context, ReservationID) error
	Reconcile(context.Context) error
	Reserved(context.Context) (uint256.Int, error)
}
