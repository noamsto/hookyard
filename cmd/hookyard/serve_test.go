package main

import (
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/serve"
)

func TestUsageIncludesServe(t *testing.T) {
	if !strings.Contains(usage, "hookyard serve") {
		t.Error("usage should include 'hookyard serve'")
	}
	if !strings.Contains(usage, "web view") {
		t.Error("usage should describe serve as a web view")
	}
}

func TestServeDefaultPortIsPositive(t *testing.T) {
	if serve.DefaultPort <= 0 || serve.DefaultPort > 65535 {
		t.Errorf("DefaultPort = %d, want a valid TCP port", serve.DefaultPort)
	}
}
