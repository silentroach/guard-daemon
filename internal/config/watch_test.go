package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestWatchPolicyParsing(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		want      WatchPolicy
	}{
		{name: "defaults", want: WatchPolicy{StateDirectory: "state", LookbackBlocks: 64}},
		{name: "explicit values", configure: func(values map[string]string) {
			values["STATE_DIRECTORY"] = "var/lib/guard-state"
			values["WATCH_LOOKBACK_BLOCKS"] = "73"
		}, want: WatchPolicy{StateDirectory: "var/lib/guard-state", LookbackBlocks: 73}},
		{name: "upper bound", configure: func(values map[string]string) {
			values["WATCH_LOOKBACK_BLOCKS"] = "10000"
		}, want: WatchPolicy{StateDirectory: "state", LookbackBlocks: 10000}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			if test.configure != nil {
				test.configure(values)
			}
			runtimeConfig, err := LoadFrom(mapLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			if runtimeConfig.Watch != test.want {
				t.Fatalf("watch policy = %#v, want %#v", runtimeConfig.Watch, test.want)
			}
		})
	}
}

func TestWatchPolicyValidation(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		value     string
		wantError string
	}{
		{name: "empty state directory", field: "STATE_DIRECTORY", value: "", wantError: "STATE_DIRECTORY must specify a non-empty normalized path without NUL bytes"},
		{name: "unnormalized state directory", field: "STATE_DIRECTORY", value: "private-state-path-canary/../state", wantError: "STATE_DIRECTORY must specify a non-empty normalized path without NUL bytes"},
		{name: "repeated state directory separator", field: "STATE_DIRECTORY", value: "private-state-path-canary//nested", wantError: "STATE_DIRECTORY must specify a non-empty normalized path without NUL bytes"},
		{name: "NUL in state directory", field: "STATE_DIRECTORY", value: "private-state-path-canary\x00/nested", wantError: "STATE_DIRECTORY must specify a non-empty normalized path without NUL bytes"},
		{name: "empty lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "zero lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "0", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "negative lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "-1", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "plus sign in lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "+1", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "space in lookback", field: "WATCH_LOOKBACK_BLOCKS", value: " 1", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "fractional lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "1.5", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "uint64 overflow", field: "WATCH_LOOKBACK_BLOCKS", value: "18446744073709551616", wantError: "WATCH_LOOKBACK_BLOCKS must be a positive decimal integer"},
		{name: "limit exceeded", field: "WATCH_LOOKBACK_BLOCKS", value: "10001", wantError: "WATCH_LOOKBACK_BLOCKS must not exceed 10000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			values[test.field] = test.value
			_, err := LoadFrom(mapLookup(values))
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("LoadFrom() returned error %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestWatchPolicyRedactsStateDirectory(t *testing.T) {
	const canary = "private-state-path-canary"
	values := validEnvironment()
	values["STATE_DIRECTORY"] = canary
	values["WATCH_LOOKBACK_BLOCKS"] = "73"
	runtimeConfig, err := LoadFrom(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}

	formatted := []struct {
		name  string
		value string
	}{
		{name: "runtime default", value: fmt.Sprintf("%v", runtimeConfig)},
		{name: "runtime detailed", value: fmt.Sprintf("%+v", runtimeConfig)},
		{name: "runtime Go syntax", value: fmt.Sprintf("%#v", runtimeConfig)},
		{name: "policy default", value: fmt.Sprintf("%v", runtimeConfig.Watch)},
		{name: "policy detailed", value: fmt.Sprintf("%+v", runtimeConfig.Watch)},
		{name: "policy Go syntax", value: fmt.Sprintf("%#v", runtimeConfig.Watch)},
	}
	for _, output := range formatted {
		t.Run(output.name, func(t *testing.T) {
			if strings.Contains(output.value, canary) {
				t.Fatalf("formatting exposes STATE_DIRECTORY: %q", output.value)
			}
		})
	}

	values["STATE_DIRECTORY"] = canary + "/../state"
	_, err = LoadFrom(mapLookup(values))
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), values["STATE_DIRECTORY"]) {
		t.Fatalf("error exposes STATE_DIRECTORY: %v", err)
	}
}
