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
