//go:build unix

package shaper

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadModeFileRefusesLinksAndFIFOs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ModeFile)
	// A link is not followed, even to a valid mode file.
	if err := os.WriteFile(path, []byte("pause\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if m, err := readModeFile(link); err == nil || m.Kind != ModeAuto {
		t.Errorf("symlinked mode file = %v, %v; want auto and an error", m, err)
	}
	// A FIFO neither counts nor blocks the shaper's tick loop.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		m, err := readModeFile(fifo)
		if err == nil || m.Kind != ModeAuto {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO as the mode file counted, or gave no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO as the mode file blocked")
	}
}
