package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var ErrInvalidCandidate = errors.New("candidate имеет некорректную идентичность")

type NetworkID int64

type Network struct {
	Name       string
	ChainID    NetworkID
	HTTPURL    string
	WSURL      string
	Rescuer    common.Address
	HasRescuer bool
	Tokens     []Token
	// AllowUnknownTokens is true only after an explicit config opt-in.
	AllowUnknownTokens bool
}

// Format excludes private RPC endpoints from accidental logs.
func (network Network) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "Network{Name:"+network.Name+" ChainID:"+strconv.FormatInt(int64(network.ChainID), 10)+"}")
}

type Token struct {
	Address  common.Address
	Symbol   string
	Decimals uint8
}

type CandidateKind uint8

const (
	CandidateToken CandidateKind = iota + 1
	CandidateNative
	CandidatePeriodic
)

type CandidateID [sha256.Size]byte

func (id CandidateID) String() string {
	return hex.EncodeToString(id[:])
}

type RescueCandidate struct {
	ID          CandidateID
	Network     NetworkID
	Kind        CandidateKind
	Source      common.Address
	Token       Token
	BlockHash   common.Hash
	BlockNumber uint64
	TxHash      common.Hash
	LogIndex    uint
	Generation  uint64
	Observation uint64
}

func NewLogCandidate(network NetworkID, source common.Address, token Token, blockHash, txHash common.Hash, blockNumber uint64, logIndex uint) RescueCandidate {
	candidate := RescueCandidate{
		Network:     network,
		Kind:        CandidateToken,
		Source:      source,
		Token:       token,
		BlockHash:   blockHash,
		BlockNumber: blockNumber,
		TxHash:      txHash,
		LogIndex:    logIndex,
	}
	candidate.ID = candidateID(candidate)
	return candidate
}

func NewBlockCandidate(network NetworkID, kind CandidateKind, source common.Address, blockHash common.Hash, blockNumber uint64) RescueCandidate {
	candidate := RescueCandidate{
		Network:     network,
		Kind:        kind,
		Source:      source,
		BlockHash:   blockHash,
		BlockNumber: blockNumber,
	}
	candidate.ID = candidateID(candidate)
	return candidate
}

func NewPeriodicCandidate(network NetworkID, source common.Address, generation, observation uint64) RescueCandidate {
	candidate := RescueCandidate{
		Network:     network,
		Kind:        CandidatePeriodic,
		Source:      source,
		Generation:  generation,
		Observation: observation,
	}
	candidate.ID = candidateID(candidate)
	return candidate
}

func NewTokenReconciliationCandidate(network NetworkID, source common.Address, token Token, generation, observation uint64) RescueCandidate {
	candidate := RescueCandidate{
		Network:     network,
		Kind:        CandidateToken,
		Source:      source,
		Token:       token,
		Generation:  generation,
		Observation: observation,
	}
	candidate.ID = candidateID(candidate)
	return candidate
}

// ValidateCandidate подтверждает, что deserialized candidate сохранил stable ID
// и обязательные identity-поля.
func ValidateCandidate(candidate RescueCandidate) error {
	if candidate.Network <= 0 || candidate.Source == (common.Address{}) || candidate.ID != candidateID(candidate) {
		return ErrInvalidCandidate
	}
	switch candidate.Kind {
	case CandidateToken:
		if candidate.Token.Address == (common.Address{}) {
			return ErrInvalidCandidate
		}
	case CandidateNative, CandidatePeriodic:
		if candidate.Token.Address != (common.Address{}) {
			return ErrInvalidCandidate
		}
	default:
		return ErrInvalidCandidate
	}
	return nil
}

func candidateID(candidate RescueCandidate) CandidateID {
	h := sha256.New()
	h.Write([]byte("guard-daemon/candidate/v1"))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(candidate.Network))
	h.Write(number[:])
	h.Write([]byte{byte(candidate.Kind)})
	h.Write(candidate.Source[:])
	h.Write(candidate.Token.Address[:])
	h.Write(candidate.BlockHash[:])
	binary.BigEndian.PutUint64(number[:], candidate.BlockNumber)
	h.Write(number[:])
	h.Write(candidate.TxHash[:])
	binary.BigEndian.PutUint64(number[:], uint64(candidate.LogIndex))
	h.Write(number[:])
	// Generation связывает обработку с RPC session, но не является частью
	// stable ID, который должен переживать reconnect и перезапуск процесса.
	binary.BigEndian.PutUint64(number[:], candidate.Observation)
	h.Write(number[:])
	var id CandidateID
	copy(id[:], h.Sum(nil))
	return id
}

type IncidentID [sha256.Size]byte

func NewIncidentID(candidate CandidateID) IncidentID {
	h := sha256.New()
	h.Write([]byte("guard-daemon/incident/v1"))
	h.Write(candidate[:])
	var id IncidentID
	copy(id[:], h.Sum(nil))
	return id
}

// NewAssetIncidentID связывает одну финансовую операцию с durable candidate.
// Asset равен нулевому адресу только для native asset.
func NewAssetIncidentID(candidate CandidateID, kind CandidateKind, asset common.Address) IncidentID {
	h := sha256.New()
	h.Write([]byte("guard-daemon/asset-incident/v1"))
	h.Write(candidate[:])
	h.Write([]byte{byte(kind)})
	h.Write(asset[:])
	var id IncidentID
	copy(id[:], h.Sum(nil))
	return id
}

func (id IncidentID) String() string {
	return hex.EncodeToString(id[:])
}

type OutcomeStatus uint8

const (
	OutcomeAttempted OutcomeStatus = iota + 1
	OutcomeBroadcast
	OutcomeConfirmed
	OutcomeFailed
	OutcomeTokenReported
)

type TransactionOutcome struct {
	Candidate CandidateID
	Status    OutcomeStatus
	TxHash    common.Hash
	Attempted time.Time
}

type RetryState struct {
	Incident IncidentID
	Attempts uint32
	NextAt   time.Time
	LastCode ErrorCode
}

type Amount struct {
	Wei *big.Int
}
