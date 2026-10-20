package main

import (
	"os"
	"testing"
)

// TestMain points every freedesktop folder and HOME at a temporary directory:
// a test that enters, settles or self-updates a lab never touches the
// person's own labs, registry or binary.
func TestMain(m *testing.M) {
	os.Exit(isolatedFolders(m))
}

func isolatedFolders(m *testing.M) int {
	dir, err := os.MkdirTemp("", "agentlab-test-home-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for _, v := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_BIN_HOME"} {
		if err := os.Setenv(v, dir+"/"+v); err != nil {
			panic(err)
		}
	}
	return m.Run()
}
