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

// WatchPolicy задаёт локальное состояние и ограниченное окно первого backfill.
type WatchPolicy struct {
	StateDirectory string
	LookbackBlocks uint64
}

// Format исключает локальный путь из случайного форматирования.
func (policy WatchPolicy) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "WatchPolicy{LookbackBlocks:"+strconv.FormatUint(policy.LookbackBlocks, 10)+"}")
}

func loadWatchPolicy(lookup func(string) (string, bool)) (WatchPolicy, error) {
	directory, ok := lookup("STATE_DIRECTORY")
	if !ok {
		directory = defaultStateDirectory
	}
	if directory == "" || strings.IndexByte(directory, 0) >= 0 || filepath.Clean(directory) != directory {
		return WatchPolicy{}, fmt.Errorf("переменная STATE_DIRECTORY должна задавать непустой нормализованный путь без NUL")
	}

	lookbackValue, ok := lookup("WATCH_LOOKBACK_BLOCKS")
	if !ok {
		lookbackValue = defaultLookbackBlocks
	}
	if !decimalDigits(lookbackValue) {
		return WatchPolicy{}, fmt.Errorf("переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом")
	}
	lookback, err := strconv.ParseUint(lookbackValue, 10, 64)
	if err != nil || lookback == 0 {
		return WatchPolicy{}, fmt.Errorf("переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом")
	}
	if lookback > maxLookbackBlocks {
		return WatchPolicy{}, fmt.Errorf("переменная WATCH_LOOKBACK_BLOCKS не должна превышать 10000")
	}

	return WatchPolicy{StateDirectory: directory, LookbackBlocks: lookback}, nil
}
