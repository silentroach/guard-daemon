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
		{name: "значения по умолчанию", want: WatchPolicy{StateDirectory: "state", LookbackBlocks: 64}},
		{name: "явные значения", configure: func(values map[string]string) {
			values["STATE_DIRECTORY"] = "var/lib/guard-state"
			values["WATCH_LOOKBACK_BLOCKS"] = "73"
		}, want: WatchPolicy{StateDirectory: "var/lib/guard-state", LookbackBlocks: 73}},
		{name: "верхняя граница", configure: func(values map[string]string) {
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
				t.Fatalf("watch policy = %#v, нужно %#v", runtimeConfig.Watch, test.want)
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
		{name: "пустой state directory", field: "STATE_DIRECTORY", value: "", wantError: "переменная STATE_DIRECTORY должна задавать непустой нормализованный путь без NUL"},
		{name: "ненормализованный state directory", field: "STATE_DIRECTORY", value: "private-state-path-canary/../state", wantError: "переменная STATE_DIRECTORY должна задавать непустой нормализованный путь без NUL"},
		{name: "повторный разделитель state directory", field: "STATE_DIRECTORY", value: "private-state-path-canary//nested", wantError: "переменная STATE_DIRECTORY должна задавать непустой нормализованный путь без NUL"},
		{name: "NUL в state directory", field: "STATE_DIRECTORY", value: "private-state-path-canary\x00/nested", wantError: "переменная STATE_DIRECTORY должна задавать непустой нормализованный путь без NUL"},
		{name: "пустой lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "нулевой lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "0", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "отрицательный lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "-1", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "знак плюс в lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "+1", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "пробел в lookback", field: "WATCH_LOOKBACK_BLOCKS", value: " 1", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "дробный lookback", field: "WATCH_LOOKBACK_BLOCKS", value: "1.5", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "переполнение uint64", field: "WATCH_LOOKBACK_BLOCKS", value: "18446744073709551616", wantError: "переменная WATCH_LOOKBACK_BLOCKS должна быть положительным целым десятичным числом"},
		{name: "превышение лимита", field: "WATCH_LOOKBACK_BLOCKS", value: "10001", wantError: "переменная WATCH_LOOKBACK_BLOCKS не должна превышать 10000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			values[test.field] = test.value
			_, err := LoadFrom(mapLookup(values))
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("LoadFrom() error = %v, нужно %q", err, test.wantError)
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
				t.Fatalf("форматирование раскрывает STATE_DIRECTORY: %q", output.value)
			}
		})
	}

	values["STATE_DIRECTORY"] = canary + "/../state"
	_, err = LoadFrom(mapLookup(values))
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), values["STATE_DIRECTORY"]) {
		t.Fatalf("ошибка раскрывает STATE_DIRECTORY: %v", err)
	}
}
