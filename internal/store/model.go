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

type LeaseKey struct {
	Network domain.NetworkID
	Sponsor common.Address
}

type Lease struct {
	Key       LeaseKey
	Owner     string
	ExpiresAt time.Time
}
