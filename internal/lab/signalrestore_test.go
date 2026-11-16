package lab

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// exitRecorder is onSignal's exit in a test: it records the signal it is
// handed.
func exitRecorder() (func(os.Signal), <-chan os.Signal) {
	exited := make(chan os.Signal, 1)
	return func(sig os.Signal) { exited <- sig }, exited
}

// An interrupt while armed runs the restore, then ends with that signal.
func TestOnSignalRestoresThenExits(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	restored := make(chan struct{})
	exit, exited := exitRecorder()
	disarm := onSignal(sigs, func() { close(restored) }, time.Minute, exit)
	sigs <- os.Interrupt
	require.Equal(t, os.Interrupt, <-exited)
	select {
	case <-restored:
	default:
		t.Fatal("the program ended before the restore ran")
	}
	disarm()
}

// A restore that hangs cannot hang the program: it ends after the timeout.
func TestOnSignalBoundsTheRestore(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	sigs := make(chan os.Signal, 2)
	exit, exited := exitRecorder()
	onSignal(sigs, func() { <-hang }, 10*time.Millisecond, exit)
	sigs <- syscall.SIGTERM
	require.Equal(t, syscall.SIGTERM, <-exited)
}

// The range hold's handler delivers the first signal again once it resumed
// the ranges: that second signal must not end the program before the
// restore did.
func TestOnSignalWaitsForTheRestoreThroughASecondSignal(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	started, release := make(chan struct{}), make(chan struct{})
	var exitedEarly atomic.Bool
	exited := make(chan os.Signal, 1)
	onSignal(sigs, func() {
		close(started)
		<-release
		require.False(t, exitedEarly.Load(), "the program ended before the restore did")
	}, time.Minute, func(sig os.Signal) {
		exitedEarly.Store(true)
		exited <- sig
	})
	sigs <- os.Interrupt
	<-started
	sigs <- os.Interrupt
	select {
	case <-exited:
		t.Fatal("the second signal ended the program during the restore")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.Equal(t, os.Interrupt, <-exited)
}

// Once disarmed, the watch reads no signal: it keeps its default course.
func TestOnSignalDisarmed(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	exit, _ := exitRecorder()
	disarm := onSignal(sigs, func() { t.Error("restore ran after disarm") }, time.Minute, exit)
	disarm()
	disarm()
	sigs <- os.Interrupt
	require.Len(t, sigs, 1)
}

// The exit status is a shell's for a program a signal ended.
func TestSignalExitCode(t *testing.T) {
	require.Equal(t, 130, signalExitCode(syscall.SIGINT))
	require.Equal(t, 143, signalExitCode(syscall.SIGTERM))
}
