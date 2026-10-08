package main

import (
	"os"
	"strings"
	"testing"
)

// `go install github.com/giantswarm/agentlab@latest`, the install path the
// README gives, refuses a module whose go.mod carries a replace or exclude
// directive — a dependency is moved by bumping it, never by redirecting it.
func TestGoModHasNoReplaceOrExcludeDirectives(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "replace" || fields[0] == "exclude" {
			t.Errorf("go.mod:%d: %q breaks `go install github.com/giantswarm/agentlab@latest`; bump the dependency instead", i+1, strings.TrimSpace(line))
		}
	}
}

// Renovate's gomod manager reads a require line only when its trailing
// comment is a single word (`// indirect`); a longer comment hides the module
// from every update, including the renovate-custom.json5 rules. Explanations
// belong in HACKS.md.
func TestGoModRequireCommentsAreOneWord(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		code, comment, found := strings.Cut(line, "//")
		if !found || strings.TrimSpace(code) == "" {
			continue
		}
		if len(strings.Fields(comment)) > 1 {
			t.Errorf("go.mod:%d: %q: Renovate skips a module whose comment is more than one word", i+1, strings.TrimSpace(line))
		}
	}
}
