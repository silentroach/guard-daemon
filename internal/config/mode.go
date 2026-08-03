package config

import (
	"crypto/ecdsa"
	"fmt"
	"io"
)

// Mode задаёт безопасный dry-run либо явно включённый live-режим.
type Mode uint8

const (
	// ModeDryRun запрещает использование приватных ключей и отправку транзакций.
	ModeDryRun Mode = iota
	// ModeLive разрешает последующее создание signer после внешней аттестации.
	ModeLive
)

// IsDryRun сообщает, включён ли безопасный режим.
func (mode Mode) IsDryRun() bool {
	return mode == ModeDryRun
}

// IsLive сообщает, включён ли live-режим.
func (mode Mode) IsLive() bool {
	return mode == ModeLive
}

// String возвращает безопасное имя режима.
func (mode Mode) String() string {
	switch mode {
	case ModeDryRun:
		return "dry-run"
	case ModeLive:
		return "live"
	default:
		return "неизвестный"
	}
}

func loadMode(lookup func(string) (string, bool)) (Mode, error) {
	value, ok := lookup("DRY_RUN")
	if !ok {
		return ModeDryRun, nil
	}
	switch value {
	case "true":
		return ModeDryRun, nil
	case "false":
		return ModeLive, nil
	default:
		return 0, fmt.Errorf("переменная DRY_RUN должна быть равна true или false")
	}
}

// LiveSecrets непрозрачно хранит ключи, разобранные только для live-режима.
type LiveSecrets struct {
	sourcePrivateKey  *ecdsa.PrivateKey
	sponsorPrivateKey *ecdsa.PrivateKey
}

// PrivateKeys возвращает ключи только при наличии полной live-конфигурации.
func (secrets LiveSecrets) PrivateKeys() (*ecdsa.PrivateKey, *ecdsa.PrivateKey, bool) {
	if secrets.sourcePrivateKey == nil || secrets.sponsorPrivateKey == nil {
		return nil, nil, false
	}
	return secrets.sourcePrivateKey, secrets.sponsorPrivateKey, true
}

// Format всегда скрывает содержимое ключей независимо от формата вывода.
func (LiveSecrets) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "LiveSecrets{скрыто}")
}

func loadLiveSecrets(lookup func(string) (string, bool), mode Mode, emergencyStop bool) (LiveSecrets, error) {
	if mode.IsDryRun() || emergencyStop {
		return LiveSecrets{}, nil
	}
	source, err := loadPrivateKey(lookup, "SOURCE_PRIVATE_KEY")
	if err != nil {
		return LiveSecrets{}, err
	}
	sponsor, err := loadPrivateKey(lookup, "SPONSOR_PRIVATE_KEY")
	if err != nil {
		return LiveSecrets{}, err
	}
	return LiveSecrets{sourcePrivateKey: source, sponsorPrivateKey: sponsor}, nil
}
