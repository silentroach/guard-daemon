package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

func TestBoltStoreLeaseContentionAndExpiryTakeover(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 16, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	key := testLeaseKey(options)
	first, err := store.Acquire(ctx, key, "coordinator-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	firstLost := store.Lost(first)
	if leaseSignalClosed(firstLost) {
		t.Fatal("new lease is already lost")
	}
	if _, err := store.Acquire(ctx, key, "coordinator-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("contending Acquire returned error %v, want ErrLeaseHeld", err)
	} else if strings.Contains(err.Error(), "coordinator") || strings.Contains(err.Error(), key.Sponsor.Hex()) {
		t.Fatalf("safe contention error exposed lease identifier: %v", err)
	}

	clock.Advance(time.Minute)
	second, err := store.Acquire(ctx, key, "coordinator-b", time.Minute)
	if err != nil {
		t.Fatalf("taking over expired lease: %v", err)
	}
	if second.Owner == first.Owner || second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("taken-over lease = %v, want new owner and expiration", second)
	}
	if !leaseSignalClosed(firstLost) {
		t.Fatal("takeover did not close the expired owner's loss signal")
	}
}

func TestBoltStoreLeaseIsDurableAcrossReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 17, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	key := testLeaseKey(options)
	if _, err := store.Acquire(ctx, key, "coordinator-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	store = reopenTestStore(t, store, path, options)
	defer store.Close()
	if _, err := store.Acquire(ctx, key, "coordinator-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("Acquire after reopening returned error %v, want persistent ErrLeaseHeld", err)
	}
	clock.Advance(time.Minute)
	if _, err := store.Acquire(ctx, key, "coordinator-b", time.Minute); err != nil {
		t.Fatalf("taking over expired persistent lease: %v", err)
	}
}

func TestBoltStoreLeaseRenewAndValidateExactRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 18, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	defer store.Close()
	lease, err := store.Acquire(ctx, testLeaseKey(options), "coordinator-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	oldLost := store.Lost(lease)
	if err := store.Validate(ctx, lease); err != nil {
		t.Fatalf("Validate on current lease returned an error: %v", err)
	}
	clock.Advance(30 * time.Second)
	renewed, err := store.Renew(ctx, lease, time.Minute)
	if err != nil {
		t.Fatalf("Renew returned an error: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renewed expiration %s did not advance beyond %s", renewed.ExpiresAt, lease.ExpiresAt)
	}
	if !leaseSignalClosed(oldLost) {
		t.Fatal("renewal did not invalidate the previous exact lease signal")
	}
	if err := store.Validate(ctx, lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Validate on stale lease returned error %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Release on stale lease returned error %v, want ErrLeaseLost", err)
	}
	if err := store.Validate(ctx, renewed); err != nil {
		t.Fatalf("Validate on renewed lease returned an error: %v", err)
	}

	mismatch := renewed
	mismatch.Owner = "coordinator-b"
	if err := store.Validate(ctx, mismatch); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Validate with owner mismatch returned error %v, want ErrLeaseLost", err)
	}
}

func TestBoltStoreRejectsCorruptLeaseRecordOnReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 20, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	path := filepath.Join(t.TempDir(), "handoff.db")
	store := openTestStore(t, path, options)
	lease, err := store.Acquire(ctx, testLeaseKey(options), "coordinator-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	mutateStore(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(leasesBucket).Put(encodeLeaseKey(lease.Key), []byte{leaseRecordVersion})
	})
	if opened, err := Open(path, options); err == nil {
		opened.Close()
		t.Fatal("database with corrupt lease record reopened")
	}
}

func TestBoltStoreLeaseLostSignalsOnExpiryReleaseAndClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := &fakeStoreClock{now: time.Date(2026, time.August, 2, 19, 0, 0, 0, time.UTC)}
	options := testOpenOptions(clock)
	store := openTestStore(t, filepath.Join(t.TempDir(), "handoff.db"), options)
	key := testLeaseKey(options)

	expiring, err := store.Acquire(ctx, key, "coordinator-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expiredLost := store.Lost(expiring)
	clock.Advance(time.Minute)
	if err := store.Validate(ctx, expiring); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Validate on expired lease returned error %v, want ErrLeaseLost", err)
	}
	if !leaseSignalClosed(expiredLost) {
		t.Fatal("expiration detection did not close loss signal")
	}

	released, err := store.Acquire(ctx, key, "coordinator-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	releasedLost := store.Lost(released)
	if err := store.Release(ctx, released); err != nil {
		t.Fatalf("Release returned an error: %v", err)
	}
	if !leaseSignalClosed(releasedLost) {
		t.Fatal("Release did not close loss signal")
	}

	closing, err := store.Acquire(ctx, key, "coordinator-c", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	closingLost := store.Lost(closing)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !leaseSignalClosed(closingLost) {
		t.Fatal("store Close did not close lease loss signal")
	}
}

func testLeaseKey(options OpenOptions) LeaseKey {
	var sponsor common.Address
	sponsor[len(sponsor)-1] = 0xc3
	return LeaseKey{Network: options.Network, Sponsor: sponsor}
}

func leaseSignalClosed(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}
