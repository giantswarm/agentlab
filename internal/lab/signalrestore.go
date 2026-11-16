package lab

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// signalRestoreTimeout bounds the restore an interrupt runs: one write to
// the apiserver (kubeReadTimeout), and the end of a write the run may have
// in flight, before the program ends anyway.
const signalRestoreTimeout = kubeReadTimeout + 5*time.Second

// restoreOnSignal runs restore when the program is interrupted (SIGINT or
// SIGTERM) before the returned func disarms it, then ends the program with
// the signal's exit status (128 + its number). The restore is bounded by
// signalRestoreTimeout, and a second signal ends the program at once. While
// it is not armed, a signal takes its default course.
func restoreOnSignal(restore func()) (disarm func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	stop := onSignal(sigs, restore, signalRestoreTimeout, func(sig os.Signal) {
		signal.Stop(sigs)
		os.Exit(signalExitCode(sig))
	})
	return func() {
		signal.Stop(sigs)
		stop()
	}
}

// onSignal waits for the first signal on sigs until disarm is called. On a
// signal it runs restore and calls exit with that signal once restore
// returns, timeout passed, or a second signal arrived, whichever is first.
// disarm returns once the watch has stopped waiting, so a signal sent after
// it stays unread; a signal read before it still runs restore.
func onSignal(sigs <-chan os.Signal, restore func(), timeout time.Duration, exit func(os.Signal)) (disarm func()) {
	done := make(chan struct{})
	waiting := make(chan struct{})
	go func() {
		var sig os.Signal
		select {
		case sig = <-sigs:
			close(waiting)
		case <-done:
			close(waiting)
			return
		}
		restored := make(chan struct{})
		go func() {
			defer close(restored)
			restore()
		}()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-restored:
		case <-timer.C:
			warn("%s: the clean-up did not end within %s; ending without it", sig, timeout)
		case again := <-sigs:
			warn("%s: ending without the clean-up", again)
		}
		exit(sig)
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-waiting
	}
}

// signalExitCode is the exit status a shell gives a program a signal ended:
// 128 plus the signal's number.
func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
