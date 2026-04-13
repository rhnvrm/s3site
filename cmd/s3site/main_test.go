package main

import (
	"errors"
	"flag"
	"testing"
)

func TestRunRefreshHelp(t *testing.T) {
	if err := runRefresh([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}
}
