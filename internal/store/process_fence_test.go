//go:build linux || darwin

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/sys/unix"
)

func TestProcessFenceIsExclusiveAcrossProcessesWithDifferentEnvironments(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	parentHome := t.TempDir()
	parentCache := t.TempDir()
	parentState := t.TempDir()
	helperState := t.TempDir()
	t.Setenv("HOME", parentHome)
	t.Setenv("XDG_CACHE_HOME", parentCache)
	t.Setenv("STATE_DIRECTORY", parentState)

	command, input := startProcessFenceHelper(t, key, helperState, "exit")
	if fence, err := AcquireProcessFence(key); !errors.Is(err, ErrFenceHeld) || fence != nil {
		t.Fatalf("AcquireProcessFence() under subprocess contention = (%v, %v)", fence, err)
	} else if strings.Contains(err.Error(), key.Sponsor.Hex()) || strings.Contains(err.Error(), parentState) || strings.Contains(err.Error(), helperState) {
		t.Fatalf("fence contention error exposed identity or state path: %q", err)
	}

	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}

	fence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatalf("AcquireProcessFence() after helper exit = %v", err)
	}
	if err := fence.Release(); err != nil {
		t.Fatalf("Release() after helper exit = %v", err)
	}
}

func TestProcessFenceKeySeparation(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	first, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	differentSponsor := key
	differentSponsor.Sponsor[0] ^= 0xff
	differentNetwork := key
	differentNetwork.Network++
	for _, separated := range []LeaseKey{differentSponsor, differentNetwork} {
		fence, err := AcquireProcessFence(separated)
		if err != nil {
			t.Fatalf("separated AcquireProcessFence() = %v", err)
		}
		if err := fence.Release(); err != nil {
			t.Fatalf("separated Release() = %v", err)
		}
	}
}

func TestProcessFenceReleaseIsIdempotentAndInvalidatesFence(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	fence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fence.Validate(); err != nil {
		t.Fatalf("Validate() while held = %v", err)
	}
	if err := fence.Release(); err != nil {
		t.Fatalf("first Release() = %v", err)
	}
	if err := fence.Release(); err != nil {
		t.Fatalf("second Release() = %v", err)
	}
	if err := fence.Validate(); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("Validate() after Release() = %v", err)
	}
	path, err := processFencePath(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("normal Release() removed stable lock file: %v", err)
	}
}

func TestProcessFenceKernelLockIsReleasedOnCrash(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	command, _ := startProcessFenceHelper(t, key, t.TempDir(), "block")
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper exited successfully")
	}

	fence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatalf("AcquireProcessFence() after crash = %v", err)
	}
	if err := fence.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessFenceUsesStablePrivateOpaquePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("STATE_DIRECTORY", t.TempDir())
	key := uniqueProcessFenceKey(t)

	fence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Release()
	path, err := processFencePath(key)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != processFenceRoot {
		t.Fatalf("fence root = %q, want stable production root", filepath.Dir(path))
	}
	if name := filepath.Base(path); len(name) != sha256.Size*2 || strings.Contains(name, strings.ToLower(strings.TrimPrefix(key.Sponsor.Hex(), "0x"))) {
		t.Fatalf("fence filename is not an opaque SHA-256 digest: %q", name)
	}
	for _, checked := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(checked)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", filepath.Base(checked), info.Mode().Perm(), want)
		}
	}
}

func TestProcessFenceDirectoryCreationFailsClosed(t *testing.T) {
	blockedDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDirectory, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ensurePrivateFenceDirectory(blockedDirectory)
	if !errors.Is(err, errFenceUnavailable) {
		t.Fatalf("ensurePrivateFenceDirectory() = %v", err)
	}
	if strings.Contains(err.Error(), blockedDirectory) {
		t.Fatalf("stable-root error exposed path: %q", err)
	}
}

func TestProcessFenceDetectsUnlinkAndReplacement(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	oldFence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	path, err := processFencePath(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	replacement, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatalf("AcquireProcessFence() for replacement inode = %v", err)
	}
	defer replacement.Release()
	if err := oldFence.Validate(); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("old fence Validate() after replacement = %v", err)
	}
	if err := replacement.Validate(); err != nil {
		t.Fatalf("replacement Validate() = %v", err)
	}
	if err := oldFence.Release(); err != nil {
		t.Fatalf("old fence Release() after replacement = %v", err)
	}
}

func TestProcessFenceReleaseHandlesMissingPath(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	fence, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	path, err := processFencePath(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := fence.Release(); err != nil {
		t.Fatalf("Release() with missing path = %v", err)
	}

	replacement, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessFenceValidateDetectsUnlockedDescriptor(t *testing.T) {
	key := uniqueProcessFenceKey(t)
	acquired, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	fence := acquired.(*unixProcessFence)
	if err := unix.Flock(fence.fd, unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := fence.Validate(); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("Validate() after descriptor unlock = %v", err)
	}
	if err := fence.Release(); err != nil {
		t.Fatal(err)
	}

	replacement, err := AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessFenceHelper(t *testing.T) {
	if os.Getenv("GUARD_DAEMON_PROCESS_FENCE_HELPER") != "1" {
		return
	}
	network, err := strconv.ParseInt(os.Getenv("GUARD_DAEMON_PROCESS_FENCE_NETWORK"), 10, 64)
	sponsorText := os.Getenv("GUARD_DAEMON_PROCESS_FENCE_SPONSOR")
	if err != nil || network <= 0 || !common.IsHexAddress(sponsorText) {
		os.Exit(2)
	}
	fence, err := AcquireProcessFence(LeaseKey{Network: domain.NetworkID(network), Sponsor: common.HexToAddress(sponsorText)})
	if err != nil {
		os.Exit(3)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		os.Exit(4)
	}
	var signal [1]byte
	if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
		os.Exit(5)
	}
	if os.Getenv("GUARD_DAEMON_PROCESS_FENCE_MODE") == "release" {
		if err := fence.Release(); err != nil {
			os.Exit(6)
		}
	}
	os.Exit(0)
}

func startProcessFenceHelper(t *testing.T, key LeaseKey, stateDirectory, mode string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestProcessFenceHelper$")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Env = append(os.Environ(),
		"GUARD_DAEMON_PROCESS_FENCE_HELPER=1",
		"GUARD_DAEMON_PROCESS_FENCE_NETWORK="+strconv.FormatInt(int64(key.Network), 10),
		"GUARD_DAEMON_PROCESS_FENCE_SPONSOR="+key.Sponsor.Hex(),
		"GUARD_DAEMON_PROCESS_FENCE_MODE="+mode,
		"HOME="+filepath.Join(stateDirectory, "different-home"),
		"XDG_CACHE_HOME="+filepath.Join(stateDirectory, "different-cache"),
		"STATE_DIRECTORY="+stateDirectory,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || ready != "ready\n" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("process fence helper did not become ready: output=%q error=%v stderr=%q", ready, err, stderr.String())
	}
	return command, input
}

func uniqueProcessFenceKey(t *testing.T) LeaseKey {
	t.Helper()
	digest := sha256.Sum256([]byte(t.Name() + strconv.FormatInt(time.Now().UnixNano(), 10)))
	network := int64(0)
	for _, value := range digest[:8] {
		network = network<<7 | int64(value&0x7f)
	}
	if network == 0 {
		network = 1
	}
	return LeaseKey{Network: domain.NetworkID(network), Sponsor: common.BytesToAddress(digest[sha256.Size-common.AddressLength:])}
}
