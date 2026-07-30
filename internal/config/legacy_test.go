package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestLoadLegacyFromDerivesAddresses(t *testing.T) {
	runtime, err := LoadLegacyFrom(mapLookup(validTestEnvironment()))
	if err != nil {
		t.Fatalf("LoadLegacyFrom() вернул ошибку: %v", err)
	}

	if runtime.SourcePrivateKey == nil || runtime.SponsorPrivateKey == nil {
		t.Fatal("LoadLegacyFrom() не вернул разобранные приватные ключи")
	}
	if got, want := runtime.SourceAddress.Hex(), "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"; got != want {
		t.Fatalf("адрес source = %s, нужен %s", got, want)
	}
	if got, want := runtime.SponsorAddress.Hex(), "0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF"; got != want {
		t.Fatalf("адрес sponsor = %s, нужен %s", got, want)
	}
}

func TestLoadLegacyFromRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		want      string
	}{
		{
			name: "required source key",
			configure: func(values map[string]string) {
				delete(values, "SOURCE_PRIVATE_KEY")
			},
			want: "не задана обязательная переменная окружения SOURCE_PRIVATE_KEY",
		},
		{
			name: "required sponsor key",
			configure: func(values map[string]string) {
				values["SPONSOR_PRIVATE_KEY"] = ""
			},
			want: "не задана обязательная переменная окружения SPONSOR_PRIVATE_KEY",
		},
		{
			name: "required destination",
			configure: func(values map[string]string) {
				delete(values, "DESTINATION_ADDRESS")
			},
			want: "не задана обязательная переменная окружения DESTINATION_ADDRESS",
		},
		{
			name: "invalid private key",
			configure: func(values map[string]string) {
				values["SOURCE_PRIVATE_KEY"] = "test-only-invalid-secret"
			},
			want: "переменная SOURCE_PRIVATE_KEY содержит некорректный приватный ключ",
		},
		{
			name: "invalid destination",
			configure: func(values map[string]string) {
				values["DESTINATION_ADDRESS"] = "test-only-invalid-address"
			},
			want: "переменная DESTINATION_ADDRESS содержит некорректный EVM-адрес",
		},
		{
			name: "invalid rescuer",
			configure: func(values map[string]string) {
				values["RESCUER_BASE"] = "test-only-invalid-rescuer"
			},
			want: "переменная RESCUER_BASE содержит некорректный EVM-адрес",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validTestEnvironment()
			test.configure(values)

			_, err := LoadLegacyFrom(mapLookup(values))
			if err == nil {
				t.Fatal("LoadLegacyFrom() не вернул ошибку")
			}
			if err.Error() != test.want {
				t.Fatalf("ошибка = %q, нужна %q", err, test.want)
			}
			for _, value := range values {
				if strings.Contains(err.Error(), value) && strings.HasPrefix(value, "test-only-") {
					t.Fatalf("ошибка раскрывает входное значение: %q", err)
				}
			}
		})
	}
}

func TestLoadLegacyFromAppliesRescuerOverride(t *testing.T) {
	values := validTestEnvironment()
	values["RESCUER_BASE"] = "0x000000000000000000000000000000000000bEEF"

	runtime, err := LoadLegacyFrom(mapLookup(values))
	if err != nil {
		t.Fatalf("LoadLegacyFrom() вернул ошибку: %v", err)
	}

	base := runtime.Networks[0]
	if !base.HasRescuer {
		t.Fatal("override RESCUER_BASE не включил rescuer")
	}
	if got, want := base.Rescuer, common.HexToAddress(values["RESCUER_BASE"]); got != want {
		t.Fatalf("rescuer Base = %s, нужен %s", got, want)
	}
	for _, network := range runtime.Networks[1:] {
		if network.HasRescuer || network.Rescuer != (common.Address{}) {
			t.Fatalf("для сети %s появился rescuer без override", network.Name)
		}
	}
}

func TestDefaultNetworksReturnsDeepCopies(t *testing.T) {
	first := DefaultNetworks()
	second := DefaultNetworks()

	wantName := second[0].Name
	wantToken := second[0].Tokens[0]
	first[0].Name = "test-only-mutated"
	first[0].Tokens[0].Symbol = "MUTATED"
	first[0].Tokens[0].Address = common.Address{}

	if second[0].Name != wantName {
		t.Fatalf("изменение первой копии затронуло имя второй: %q", second[0].Name)
	}
	if second[0].Tokens[0] != wantToken {
		t.Fatalf("изменение первой копии затронуло токен второй: %#v", second[0].Tokens[0])
	}
}

func validTestEnvironment() map[string]string {
	return map[string]string{
		"SOURCE_PRIVATE_KEY":  fmt.Sprintf("%064x", 1),
		"SPONSOR_PRIVATE_KEY": fmt.Sprintf("%064x", 2),
		"DESTINATION_ADDRESS": "0x00000000000000000000000000000000000000dE",
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
