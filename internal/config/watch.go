package config

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultStateDirectory = "state"
	defaultLookbackBlocks = "64"
	maxLookbackBlocks     = uint64(10000)
)

// WatchPolicy задаёт каталог локального состояния и глубину первоначального
// ретроспективного сканирования.
type WatchPolicy struct {
	StateDirectory string
	LookbackBlocks uint64
}

// Format исключает локальный путь из форматированного вывода.
func (policy WatchPolicy) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "WatchPolicy{LookbackBlocks:"+strconv.FormatUint(policy.LookbackBlocks, 10)+"}")
}

func loadWatchPolicy(lookup func(string) (string, bool)) (WatchPolicy, error) {
	directory, ok := lookup("STATE_DIRECTORY")
	if !ok {
		directory = defaultStateDirectory
	}
	if directory == "" || strings.IndexByte(directory, 0) >= 0 || filepath.Clean(directory) != directory {
		return WatchPolicy{}, fmt.Errorf("STATE_DIRECTORY must specify a non-empty normalized path without NUL bytes")
	}

	lookbackValue, ok := lookup("WATCH_LOOKBACK_BLOCKS")
	if !ok {
		lookbackValue = defaultLookbackBlocks
	}
	if !decimalDigits(lookbackValue) {
		return WatchPolicy{}, fmt.Errorf("WATCH_LOOKBACK_BLOCKS must be a positive decimal integer")
	}
	lookback, err := strconv.ParseUint(lookbackValue, 10, 64)
	if err != nil || lookback == 0 {
		return WatchPolicy{}, fmt.Errorf("WATCH_LOOKBACK_BLOCKS must be a positive decimal integer")
	}
	if lookback > maxLookbackBlocks {
		return WatchPolicy{}, fmt.Errorf("WATCH_LOOKBACK_BLOCKS must not exceed 10000")
	}

	return WatchPolicy{StateDirectory: directory, LookbackBlocks: lookback}, nil
}
