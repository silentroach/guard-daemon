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
			return nil, fmt.Errorf("credential %s is forbidden in the initial environment", credential.name)
		}
	}

	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	if directory == "" {
		return map[string]string{}, nil
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, fmt.Errorf("credentials directory is not canonical")
	}
	metadata, err := os.Lstat(directory)
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid credentials directory")
	}

	values := make(map[string]string, len(systemdCredentials))
	for _, credential := range systemdCredentials {
		path := filepath.Join(directory, credential.identifier)
		metadata, err := os.Lstat(path)
		if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() > maximumCredentialBytes {
			return nil, fmt.Errorf("credential %s is not a valid file", credential.name)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read credential %s", credential.name)
		}
		if len(value) > maximumCredentialBytes {
			return nil, fmt.Errorf("credential %s exceeds the maximum size", credential.name)
		}
		if len(value) == 0 {
			continue
		}
		if bytes.IndexAny(value, "\x00\r\n\t ") >= 0 {
			return nil, fmt.Errorf("credential %s contains whitespace", credential.name)
		}
		values[credential.name] = string(value)
	}
	return values, nil
}
