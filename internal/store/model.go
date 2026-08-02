package store

import (
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

type PutResult uint8

const (
	PutInserted PutResult = iota + 1
	PutAlreadyPending
	PutAlreadyAcknowledged
)

type CandidateStatus uint8

const (
	CandidateObserved CandidateStatus = iota + 1
	CandidateStaged
	CandidateReady
	CandidateDelayed
	CandidateAcknowledged
	CandidateRemoved
)

type Incident struct {
	ID        domain.IncidentID
	Candidate domain.CandidateID
	Network   domain.NetworkID
	CreatedAt time.Time
}

type Checkpoint struct {
	Network     domain.NetworkID
	BlockNumber uint64
	BlockHash   common.Hash
}

// CanonicalBlock является атомарной единицей scanner: заголовок, полный набор
// candidates и seal пустого блока сохраняются одной транзакцией.
type CanonicalBlock struct {
	Checkpoint
	ParentHash common.Hash
	Candidates []domain.RescueCandidate
}

type OpenOptions struct {
	Network             domain.NetworkID
	Source              common.Address
	PolicyFingerprint   [32]byte
	MaxPending          uint32
	MaxDiscoveredTokens uint32
	Clock               interface{ Now() time.Time }
}

type LeaseKey struct {
	Network domain.NetworkID
	Sponsor common.Address
}

type Lease struct {
	Key       LeaseKey
	Owner     string
	ExpiresAt time.Time
}
