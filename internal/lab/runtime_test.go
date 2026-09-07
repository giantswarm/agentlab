package lab

import (
	"errors"
	"os"
	"testing"
)

// withPodman makes the engine-detection answer fixed for one test. The real
// dockerIsPodman caches its shell-out for the process, which no test can
// undo, so every podman-branch test goes through here.
func withPodman(t *testing.T, podman bool) {
	t.Helper()
	prev := dockerIsPodman
	dockerIsPodman = func() bool { return podman }
	t.Cleanup(func() { dockerIsPodman = prev })
}

func TestIsPodmanVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"podman version 5.8.4\n", true},
		{"  Podman version 4.9.0  ", true},
		{"Docker version 29.7.2, build 1.fc44", false},
		{"docker version 20.10.7, build f0df350", false},
		{"Client: Docker Engine - Community\n 29.7.2", false},
		{"", false},
	} {
		if got := isPodmanVersion(tc.out); got != tc.want {
			t.Errorf("isPodmanVersion(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

func TestUnprivilegedPortStart(t *testing.T) {
	read := func(content string, err error) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(content), err }
	}
	for name, tc := range map[string]struct {
		read func(string) ([]byte, error)
		want int
	}{
		"kernel default":   {read("1024\n", nil), 1024},
		"lowered by admin": {read("80\n", nil), 80},
		"unreadable":       {read("", errors.New("no such file")), defaultUnprivilegedPortStart},
		"not a number":     {read("nope\n", nil), defaultUnprivilegedPortStart},
		"out of range":     {read("70000\n", nil), defaultUnprivilegedPortStart},
		"zero":             {read("0\n", nil), defaultUnprivilegedPortStart},
	} {
		if got := unprivilegedPortStart(tc.read); got != tc.want {
			t.Errorf("%s: unprivilegedPortStart = %d, want %d", name, got, tc.want)
		}
	}
}

// Docker's daemon binds as root, so the whole port range is publishable; only
// a rootless podman has a floor.
func TestMinPublishablePortUnderDocker(t *testing.T) {
	withPodman(t, false)
	if got := MinPublishablePort(); got != 1 {
		t.Errorf("MinPublishablePort() under docker = %d, want 1", got)
	}
}

func TestMinPublishablePortUnderPodman(t *testing.T) {
	withPodman(t, true)
	want := 1
	if os.Geteuid() != 0 {
		want = unprivilegedPortStart(os.ReadFile)
	}
	if got := MinPublishablePort(); got != want {
		t.Errorf("MinPublishablePort() under podman = %d, want %d", got, want)
	}
}
