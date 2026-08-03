package budget

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"guard-daemon/internal/domain"

	"github.com/holiman/uint256"
)

const reservationRecordVersion = byte(1)

var errInvalidRecord = errors.New("budget ledger: некорректная запись")

func encodePolicy(policy Policy) []byte {
	networks := append([]NetworkPolicy(nil), policy.Networks...)
	sort.Slice(networks, func(i, j int) bool { return networks[i].Network < networks[j].Network })
	result := make([]byte, 0, 1+4*32+4+len(networks)*(8+20+6*32))
	result = append(result, reservationRecordVersion)
	result = appendLimits(result, policy.Global)
	result = appendUint32(result, uint32(len(networks)))
	for _, network := range networks {
		result = appendUint64(result, uint64(network.Network))
		result = append(result, network.Sponsor[:]...)
		result = appendLimits(result, network.Limits)
		result = appendUint256(result, network.TransactionOverhead)
		result = appendUint256(result, network.EmergencySponsorReserve)
	}
	return result
}

func appendLimits(target []byte, limits Limits) []byte {
	target = appendUint256(target, limits.PerTransaction)
	target = appendUint256(target, limits.PerHour)
	target = appendUint256(target, limits.PerDay)
	return appendUint256(target, limits.Cumulative)
}

func encodeReservation(reservation Reservation) ([]byte, error) {
	if !validPersistentTime(reservation.CreatedAt) || !validPersistentTime(reservation.UpdatedAt) {
		return nil, errInvalidRecord
	}
	result := make([]byte, 0, 330)
	result = append(result, reservationRecordVersion, byte(reservation.State))
	result = append(result, reservation.ID[:]...)
	result = appendUint64(result, uint64(reservation.Request.Network))
	result = append(result, reservation.Request.Sponsor[:]...)
	result = append(result, reservation.Request.Candidate[:]...)
	result = append(result, reservation.Request.Attempt.Incident[:]...)
	result = appendUint32(result, reservation.Request.Attempt.Number)
	result = appendUint64(result, reservation.Request.Quote.GasLimit)
	result = appendUint256(result, reservation.Request.Quote.MaxFeePerGas)
	result = appendUint256(result, reservation.Request.Quote.MaximumCost)
	result = appendUint256(result, reservation.Request.SponsorBalance)
	result = appendTime(result, reservation.Request.ObservedAt)
	result = appendUint256(result, reservation.Maximum)
	result = appendUint256(result, reservation.Actual)
	result = append(result, reservation.TxHash[:]...)
	result = appendTime(result, reservation.CreatedAt)
	result = appendTime(result, reservation.ExposedAt)
	result = appendTime(result, reservation.UpdatedAt)
	return result, nil
}

func decodeReservation(data []byte) (Reservation, error) {
	const recordSize = 2 + 32 + 8 + 20 + 32 + 32 + 4 + 8 + 32 + 32 + 12 + 32 + 32 + 32 + 32 + 12 + 12 + 12
	if len(data) != recordSize || data[0] != reservationRecordVersion {
		return Reservation{}, errInvalidRecord
	}
	offset := 1
	reservation := Reservation{State: ReservationState(data[offset])}
	offset++
	if !readFixed(data, &offset, reservation.ID[:]) {
		return Reservation{}, errInvalidRecord
	}
	network, ok := readUint64(data, &offset)
	if !ok || network > uint64(^uint64(0)>>1) {
		return Reservation{}, errInvalidRecord
	}
	reservation.Request.Network = domain.NetworkID(int64(network))
	if !readFixed(data, &offset, reservation.Request.Sponsor[:]) ||
		!readFixed(data, &offset, reservation.Request.Candidate[:]) ||
		!readFixed(data, &offset, reservation.Request.Attempt.Incident[:]) {
		return Reservation{}, errInvalidRecord
	}
	if reservation.Request.Attempt.Number, ok = readUint32(data, &offset); !ok {
		return Reservation{}, errInvalidRecord
	}
	if reservation.Request.Quote.GasLimit, ok = readUint64(data, &offset); !ok ||
		!readUint256(data, &offset, &reservation.Request.Quote.MaxFeePerGas) ||
		!readUint256(data, &offset, &reservation.Request.Quote.MaximumCost) ||
		!readUint256(data, &offset, &reservation.Request.SponsorBalance) ||
		!readTimeInto(data, &offset, &reservation.Request.ObservedAt) ||
		!readUint256(data, &offset, &reservation.Maximum) ||
		!readUint256(data, &offset, &reservation.Actual) ||
		!readFixed(data, &offset, reservation.TxHash[:]) {
		return Reservation{}, errInvalidRecord
	}
	if reservation.CreatedAt, ok = readTime(data, &offset); !ok {
		return Reservation{}, errInvalidRecord
	}
	if reservation.ExposedAt, ok = readTime(data, &offset); !ok {
		return Reservation{}, errInvalidRecord
	}
	if reservation.UpdatedAt, ok = readTime(data, &offset); !ok || offset != len(data) {
		return Reservation{}, errInvalidRecord
	}
	return reservation, nil
}

func readTimeInto(data []byte, offset *int, target *time.Time) bool {
	value, ok := readTime(data, offset)
	if ok {
		*target = value
	}
	return ok
}

func attemptKey(attempt Attempt) []byte {
	key := make([]byte, 0, len(attempt.Incident)+4)
	key = append(key, attempt.Incident[:]...)
	return appendUint32(key, attempt.Number)
}

func decodeAttemptKey(key []byte) (Attempt, bool) {
	if len(key) != 32+4 {
		return Attempt{}, false
	}
	var attempt Attempt
	copy(attempt.Incident[:], key[:32])
	attempt.Number = binary.BigEndian.Uint32(key[32:])
	return attempt, true
}

func appendUint256(target []byte, value uint256.Int) []byte {
	encoded := value.Bytes32()
	return append(target, encoded[:]...)
}

func readUint256(data []byte, offset *int, target *uint256.Int) bool {
	if *offset < 0 || len(data)-*offset < 32 {
		return false
	}
	target.SetBytes(data[*offset : *offset+32])
	*offset += 32
	return true
}

func appendTime(target []byte, value time.Time) []byte {
	if value.IsZero() {
		return append(target, make([]byte, 12)...)
	}
	value = value.Round(0).UTC()
	target = appendUint64(target, uint64(value.Unix()))
	return appendUint32(target, uint32(value.Nanosecond()))
}

func readTime(data []byte, offset *int) (time.Time, bool) {
	seconds, ok := readUint64(data, offset)
	if !ok {
		return time.Time{}, false
	}
	nanoseconds, ok := readUint32(data, offset)
	if !ok || nanoseconds >= uint32(time.Second) {
		return time.Time{}, false
	}
	if seconds == 0 && nanoseconds == 0 {
		return time.Time{}, true
	}
	return time.Unix(int64(seconds), int64(nanoseconds)).UTC(), true
}

func validPersistentTime(value time.Time) bool {
	return !value.IsZero() && value.Unix() >= 0 && (value.Unix() != 0 || value.Nanosecond() != 0)
}

func appendUint32(target []byte, value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return append(target, encoded[:]...)
}

func encodeUint32(value uint32) []byte {
	return appendUint32(nil, value)
}

func appendUint64(target []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(target, encoded[:]...)
}

func readUint32(data []byte, offset *int) (uint32, bool) {
	if *offset < 0 || len(data)-*offset < 4 {
		return 0, false
	}
	value := binary.BigEndian.Uint32(data[*offset : *offset+4])
	*offset += 4
	return value, true
}

func readUint64(data []byte, offset *int) (uint64, bool) {
	if *offset < 0 || len(data)-*offset < 8 {
		return 0, false
	}
	value := binary.BigEndian.Uint64(data[*offset : *offset+8])
	*offset += 8
	return value, true
}

func readFixed(data []byte, offset *int, target []byte) bool {
	if *offset < 0 || len(target) > len(data)-*offset {
		return false
	}
	copy(target, data[*offset:*offset+len(target)])
	*offset += len(target)
	return true
}

func sameReservationRequest(left, right ReservationRequest) bool {
	left.SponsorBalance.Clear()
	right.SponsorBalance.Clear()
	left.ObservedAt = time.Time{}
	right.ObservedAt = time.Time{}
	return left == right
}

func sortedReservations(reservations []Reservation) {
	sort.Slice(reservations, func(i, j int) bool {
		return bytes.Compare(reservations[i].ID[:], reservations[j].ID[:]) < 0
	})
}
