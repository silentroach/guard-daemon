package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSystemdCredentialsRejectsInitialEnvironment(t *testing.T) {
	unsetCredentialEnvironment(t)
	t.Setenv("SOURCE_PRIVATE_KEY", "")

	_, err := loadSystemdCredentials()
	if err == nil || !strings.Contains(err.Error(), "SOURCE_PRIVATE_KEY") {
		t.Fatalf("loadSystemdCredentials() error = %v", err)
	}
}

func TestLoadSystemdCredentialsReadsCredentialFiles(t *testing.T) {
	unsetCredentialEnvironment(t)
	directory := t.TempDir()
	source := strings.Repeat("1", 64)
	sponsor := "0x" + strings.Repeat("2", 64)
	writeCredential(t, directory, "source-private-key", source)
	writeCredential(t, directory, "sponsor-private-key", sponsor)
	t.Setenv("CREDENTIALS_DIRECTORY", directory)

	credentials, err := loadSystemdCredentials()
	if err != nil {
		t.Fatalf("loadSystemdCredentials() error = %v", err)
	}
	if credentials["SOURCE_PRIVATE_KEY"] != source {
		t.Fatal("source credential does not match")
	}
	if credentials["SPONSOR_PRIVATE_KEY"] != sponsor {
		t.Fatal("sponsor credential does not match")
	}
	if _, present := os.LookupEnv("SOURCE_PRIVATE_KEY"); present {
		t.Fatal("loader wrote source key to the environment")
	}
	if _, present := os.LookupEnv("SPONSOR_PRIVATE_KEY"); present {
		t.Fatal("loader wrote sponsor key to the environment")
	}
}

func TestLoadSystemdCredentialsAllowsEmptyEmergencyFiles(t *testing.T) {
	unsetCredentialEnvironment(t)
	directory := t.TempDir()
	writeCredential(t, directory, "source-private-key", "")
	writeCredential(t, directory, "sponsor-private-key", "")
	t.Setenv("CREDENTIALS_DIRECTORY", directory)

	credentials, err := loadSystemdCredentials()
	if err != nil {
		t.Fatalf("loadSystemdCredentials() error = %v", err)
	}
	if len(credentials) != 0 {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestLoadSystemdCredentialsRejectsUnsafeFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, directory string)
	}{
		{
			name: "newline",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				writeCredential(t, directory, "source-private-key", strings.Repeat("1", 64)+"\n")
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "outside")
				writeCredential(t, filepath.Dir(target), filepath.Base(target), strings.Repeat("1", 64))
				if err := os.Remove(filepath.Join(directory, "source-private-key")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(directory, "source-private-key")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversize",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				writeCredential(t, directory, "sponsor-private-key", strings.Repeat("2", maximumCredentialBytes+1))
			},
		},
		{
			name: "directory",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.Remove(filepath.Join(directory, "sponsor-private-key")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(directory, "sponsor-private-key"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetCredentialEnvironment(t)
			directory := t.TempDir()
			writeCredential(t, directory, "source-private-key", strings.Repeat("1", 64))
			writeCredential(t, directory, "sponsor-private-key", strings.Repeat("2", 64))
			test.mutate(t, directory)
			t.Setenv("CREDENTIALS_DIRECTORY", directory)

			if _, err := loadSystemdCredentials(); err == nil {
				t.Fatal("loadSystemdCredentials() accepted an unsafe credential")
			}
		})
	}
}

func TestLoadSystemdCredentialsRejectsNonCanonicalDirectory(t *testing.T) {
	unsetCredentialEnvironment(t)
	t.Setenv("CREDENTIALS_DIRECTORY", "relative/credentials")

	if _, err := loadSystemdCredentials(); err == nil {
		t.Fatal("loadSystemdCredentials() accepted a relative directory")
	}
}

func unsetCredentialEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"SOURCE_PRIVATE_KEY", "SPONSOR_PRIVATE_KEY", "CREDENTIALS_DIRECTORY"} {
		previous, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, previous)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func writeCredential(t *testing.T, directory, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
