package store

import "testing"

func TestOpenRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	if opened, err := Open("", OpenOptions{}); err == nil || opened != nil {
		t.Fatalf("Open() = (%v, %v), ожидался безопасный отказ", opened, err)
	}
}
