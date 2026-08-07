package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const maximumCredentialBytes = 128

type systemdCredential struct {
	name       string
	identifier string
}

var systemdCredentials = []systemdCredential{
	{name: "SOURCE_PRIVATE_KEY", identifier: "source-private-key"},
	{name: "SPONSOR_PRIVATE_KEY", identifier: "sponsor-private-key"},
}

func loadSystemdCredentials() (map[string]string, error) {
	for _, credential := range systemdCredentials {
		if _, present := os.LookupEnv(credential.name); present {
			return nil, fmt.Errorf("credential %s запрещён в initial environment", credential.name)
		}
	}

	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	if directory == "" {
		return map[string]string{}, nil
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, fmt.Errorf("неканонический credentials directory")
	}
	metadata, err := os.Lstat(directory)
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("некорректный credentials directory")
	}

	values := make(map[string]string, len(systemdCredentials))
	for _, credential := range systemdCredentials {
		path := filepath.Join(directory, credential.identifier)
		metadata, err := os.Lstat(path)
		if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() > maximumCredentialBytes {
			return nil, fmt.Errorf("credential %s не является допустимым файлом", credential.name)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("credential %s не удалось прочитать", credential.name)
		}
		if len(value) > maximumCredentialBytes {
			return nil, fmt.Errorf("credential %s превышает допустимый размер", credential.name)
		}
		if len(value) == 0 {
			continue
		}
		if bytes.IndexAny(value, "\x00\r\n\t ") >= 0 {
			return nil, fmt.Errorf("credential %s содержит whitespace", credential.name)
		}
		values[credential.name] = string(value)
	}
	return values, nil
}
