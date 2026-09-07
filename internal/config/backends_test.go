package config

import (
	"slices"
	"testing"
)

// Fixture vocabulary, hoisted so the linter's constant check stays quiet.
const (
	// backendKServe is a backend model-manager knows and the lab does not:
	// it needs GPU nodes and a KServe install.
	backendKServe = "kserve"
	// wantHTTPURL is the endpoint validation error.
	wantHTTPURL = "http(s) URL"
)

// The canonical order is a compatibility contract: Primary() is the first
// entry, so a new server appended to the table must not move an existing
// lab's default backend.
func TestBackendOrderIsStable(t *testing.T) {
	want := []string{ModelManagerBackendOllama, ModelManagerBackendLemonade, ModelManagerBackendLMStudio}
	if !slices.Equal(ModelManagerBackends, want) {
		t.Fatalf("ModelManagerBackends = %v, want %v (new servers go last)", ModelManagerBackends, want)
	}
}

func TestBackendPortAndName(t *testing.T) {
	cases := []struct {
		backend string
		port    int
		name    string
	}{
		{ModelManagerBackendOllama, 11434, "Ollama"},
		{ModelManagerBackendLemonade, 13305, "Lemonade Server"},
		{ModelManagerBackendLMStudio, 1234, "LM Studio"},
	}
	for _, tc := range cases {
		if got := BackendPort(tc.backend); got != tc.port {
			t.Errorf("BackendPort(%q) = %d, want %d", tc.backend, got, tc.port)
		}
		if got := BackendServerName(tc.backend); got != tc.name {
			t.Errorf("BackendServerName(%q) = %q, want %q", tc.backend, got, tc.name)
		}
	}
}

// An unknown kind must not be mistaken for one of the known servers: the old
// two-way branches answered "Ollama" on 11434 for anything unrecognised,
// which reads as a working configuration. Validate rejects such a kind when
// the file loads; these answers make a slip past it visible instead.
func TestUnknownBackendIsNotAnOllama(t *testing.T) {
	if got := BackendPort(backendKServe); got != 0 {
		t.Errorf("BackendPort(kserve) = %d, want 0", got)
	}
	if got := BackendServerName("kserve"); got != "kserve" {
		t.Errorf("BackendServerName(kserve) = %q, want the kind itself", got)
	}
}

// Every backend the lab accepts has a table entry with a port and a name.
func TestEveryBackendIsInTheTable(t *testing.T) {
	for _, b := range ModelManagerBackends {
		if BackendPort(b) == 0 {
			t.Errorf("backend %q has no default port", b)
		}
		if BackendServerName(b) == b {
			t.Errorf("backend %q has no display name", b)
		}
	}
}
