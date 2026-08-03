package store

import (
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestOpenRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	if opened, err := Open("", OpenOptions{}); err == nil || opened != nil {
		t.Fatalf("Open() = (%v, %v), ожидался безопасный отказ", opened, err)
	}
}

func TestOpenRejectsUnsafeRoleBindings(t *testing.T) {
	t.Parallel()

	base := testOpenOptions(nil)
	tests := map[string]func(*OpenOptions){
		"zero sponsor":        func(options *OpenOptions) { options.Sponsor = common.Address{} },
		"zero destination":    func(options *OpenOptions) { options.Destination = common.Address{} },
		"zero rescuer":        func(options *OpenOptions) { options.Rescuer = common.Address{} },
		"source sponsor":      func(options *OpenOptions) { options.Sponsor = options.Source },
		"source destination":  func(options *OpenOptions) { options.Destination = options.Source },
		"sponsor destination": func(options *OpenOptions) { options.Destination = options.Sponsor },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if opened, err := Open(filepath.Join(t.TempDir(), "state.db"), options); err == nil || opened != nil {
				t.Fatalf("Open() = (%v, %v), expected role validation failure", opened, err)
			}
		})
	}
}
