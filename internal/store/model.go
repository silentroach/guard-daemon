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

type RescueStatus uint8

const (
	RescuePending RescueStatus = iota + 1
	RescuePrepared
	RescueSigned
	RescueBroadcast
	RescueRetryable
	RescueAmbiguous
	RescueExhausted
	RescueFailed
	RescueTrustedSuccess
	RescueTokenReported
	RescueLostRace
)

type RescuePolicySnapshot struct {
	MaxAttempts     uint32
	RetryDelay      time.Duration
	FinalityTimeout time.Duration
}

// RescueIncident является crash-consistent состоянием одной asset operation.
// Amount-поля хранят unsigned uint256 в big-endian форме без доверия к token
// metadata.
type RescueIncident struct {
	ID           domain.IncidentID
	Parent       domain.IncidentID
	Candidate    domain.CandidateID
	Network      domain.NetworkID
	Kind         domain.CandidateKind
	Asset        common.Address
	Generation   uint64
	Trusted      bool
	Policy       RescuePolicySnapshot
	Status       RescueStatus
	Attempts     uint32
	SponsorNonce uint64
	SourceNonce  uint64
	TxHash       common.Hash
	// SignedTransaction is sensitive local recovery state and must never be logged.
	SignedTransaction   []byte
	SnapshotBlockNumber uint64
	SnapshotBlockHash   common.Hash
	SourceBefore        [32]byte
	DestinationBefore   [32]byte
	RetryAt             time.Time
	ReconcileUntil      time.Time
	LastCode            domain.ErrorCode
	CreatedAt           time.Time
	UpdatedAt           time.Time
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
	Sponsor             common.Address
	Destination         common.Address
	Rescuer             common.Address
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
