package rescue

import (
	"container/list"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"guard-daemon/internal/domain"

	bolt "go.etcd.io/bbolt"
)

var (
	admissionMetaBucket    = []byte("meta")
	admissionRecordsBucket = []byte("records")
	admissionConfigKey     = []byte("config-v1")
)

type boltAdmissionPersistence struct {
	db *bolt.DB
}

// OpenAdmissionController restores one bounded global admission window shared
// by every network coordinator.
func OpenAdmissionController(path string, config AdmissionConfig, serviceClock AdmissionClock) (*AdmissionController, error) {
	controller, err := NewAdmissionController(config, serviceClock)
	if err != nil || path == "" {
		return nil, ErrInvalidAdmissionConfig
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrAdmissionPersistence
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrAdmissionPersistence
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrAdmissionPersistence
	}
	created := false
	file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case createErr == nil:
		created = true
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return nil, ErrAdmissionPersistence
		}
	case errors.Is(createErr, os.ErrExist):
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, ErrAdmissionPersistence
		}
	default:
		return nil, ErrAdmissionPersistence
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 250 * time.Millisecond, NoSync: false})
	if err != nil {
		if created {
			_ = os.Remove(path)
		}
		return nil, ErrAdmissionPersistence
	}
	fail := func() (*AdmissionController, error) {
		_ = db.Close()
		if created {
			_ = os.Remove(path)
		}
		return nil, ErrAdmissionPersistence
	}
	persistence := &boltAdmissionPersistence{db: db}
	var records []admissionRecord
	load := func(tx *bolt.Tx) error {
		meta := tx.Bucket(admissionMetaBucket)
		bucket := tx.Bucket(admissionRecordsBucket)
		if meta == nil || bucket == nil {
			return ErrAdmissionPersistence
		}
		encodedConfig := encodeAdmissionConfig(controller.config)
		if stored := meta.Get(admissionConfigKey); stored == nil || string(stored) != string(encodedConfig) {
			return ErrInvalidAdmissionConfig
		}
		return bucket.ForEach(func(_, value []byte) error {
			record, err := decodeAdmissionRecord(value)
			if err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	}
	if created {
		err = db.Update(func(tx *bolt.Tx) error {
			meta, err := tx.CreateBucket(admissionMetaBucket)
			if err != nil {
				return err
			}
			if _, err := tx.CreateBucket(admissionRecordsBucket); err != nil {
				return err
			}
			return meta.Put(admissionConfigKey, encodeAdmissionConfig(controller.config))
		})
	} else {
		err = db.View(load)
	}
	if err != nil || len(records) > controller.config.Capacity {
		return fail()
	}
	sort.Slice(records, func(i, j int) bool { return records[i].admittedAt.Before(records[j].admittedAt) })
	for index := range records {
		record := records[index]
		if !validAdmissionRequest(record.request) {
			return fail()
		}
		element := controller.records.PushBack(&record)
		key := admissionAttemptKey{network: record.request.Network, incident: record.request.Incident, attempt: record.request.Attempt}
		if _, duplicate := controller.attempts[key]; duplicate {
			return fail()
		}
		controller.attempts[key] = element
		tokenKey := admissionTokenKey{network: record.request.Network, token: record.request.Token}
		controller.tokens[tokenKey]++
		controller.events[admissionSourceEventKey{network: record.request.Network, event: record.request.SourceEvent}]++
		if record.request.Unknown {
			controller.unknown[tokenKey]++
			if record.admittedAt.After(controller.latest[tokenKey]) {
				controller.latest[tokenKey] = record.admittedAt
			}
		}
		if record.admittedAt.After(controller.lastNow) {
			controller.lastNow = record.admittedAt
		}
	}
	controller.persistence = persistence
	return controller, nil
}

func (persistence *boltAdmissionPersistence) Store(records *list.List) error {
	return persistence.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(admissionRecordsBucket); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		bucket, err := tx.CreateBucket(admissionRecordsBucket)
		if err != nil {
			return err
		}
		for element := records.Front(); element != nil; element = element.Next() {
			record := element.Value.(*admissionRecord)
			key := encodeAdmissionAttemptKey(record.request)
			if err := bucket.Put(key, encodeAdmissionRecord(*record)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (persistence *boltAdmissionPersistence) Close() error {
	if err := persistence.db.Close(); err != nil {
		return ErrAdmissionPersistence
	}
	return nil
}

func encodeAdmissionConfig(config AdmissionConfig) []byte {
	result := make([]byte, 40)
	binary.BigEndian.PutUint64(result[0:8], uint64(config.Window))
	binary.BigEndian.PutUint64(result[8:16], uint64(config.RateWindow))
	binary.BigEndian.PutUint32(result[16:20], config.RateLimit)
	binary.BigEndian.PutUint32(result[20:24], config.MaxAttemptsPerToken)
	binary.BigEndian.PutUint32(result[24:28], config.MaxAttemptsPerSourceEvent)
	binary.BigEndian.PutUint32(result[28:32], config.MaxNewUnknownTokens)
	binary.BigEndian.PutUint64(result[32:40], uint64(config.Capacity))
	return result
}

func encodeAdmissionAttemptKey(request AdmissionRequest) []byte {
	result := make([]byte, 44)
	binary.BigEndian.PutUint64(result[0:8], uint64(request.Network))
	copy(result[8:40], request.Incident[:])
	binary.BigEndian.PutUint32(result[40:44], request.Attempt)
	return result
}

func encodeAdmissionRecord(record admissionRecord) []byte {
	result := make([]byte, 149)
	offset := 0
	binary.BigEndian.PutUint64(result[offset:offset+8], uint64(record.request.Network))
	offset += 8
	copy(result[offset:offset+32], record.request.Incident[:])
	offset += 32
	binary.BigEndian.PutUint32(result[offset:offset+4], record.request.Attempt)
	offset += 4
	copy(result[offset:offset+20], record.request.Token[:])
	offset += 20
	copy(result[offset:offset+32], record.request.Parent[:])
	offset += 32
	copy(result[offset:offset+32], record.request.SourceEvent[:])
	offset += 32
	if record.request.Unknown {
		result[offset] = 1
	}
	offset++
	binary.BigEndian.PutUint64(result[offset:offset+8], uint64(record.admittedAt.Unix()))
	offset += 8
	binary.BigEndian.PutUint32(result[offset:offset+4], uint32(record.admittedAt.Nanosecond()))
	return result
}

func decodeAdmissionRecord(value []byte) (admissionRecord, error) {
	if len(value) != 149 {
		return admissionRecord{}, ErrAdmissionPersistence
	}
	offset := 0
	request := AdmissionRequest{Network: domain.NetworkID(int64(binary.BigEndian.Uint64(value[offset : offset+8])))}
	offset += 8
	copy(request.Incident[:], value[offset:offset+32])
	offset += 32
	request.Attempt = binary.BigEndian.Uint32(value[offset : offset+4])
	offset += 4
	copy(request.Token[:], value[offset:offset+20])
	offset += 20
	copy(request.Parent[:], value[offset:offset+32])
	offset += 32
	copy(request.SourceEvent[:], value[offset:offset+32])
	offset += 32
	if value[offset] > 1 {
		return admissionRecord{}, ErrAdmissionPersistence
	}
	request.Unknown = value[offset] == 1
	offset++
	seconds := binary.BigEndian.Uint64(value[offset : offset+8])
	offset += 8
	nanoseconds := binary.BigEndian.Uint32(value[offset : offset+4])
	if seconds == 0 || nanoseconds >= uint32(time.Second) {
		return admissionRecord{}, ErrAdmissionPersistence
	}
	return admissionRecord{request: request, admittedAt: time.Unix(int64(seconds), int64(nanoseconds)).UTC()}, nil
}
