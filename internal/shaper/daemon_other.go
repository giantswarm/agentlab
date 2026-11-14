//go:build !linux

package shaper

import (
	"context"
	"errors"
	"log"
	"runtime"
)

// errNotLinux is every kernel operation off Linux: the shaper runs in a
// container on the Linux kernel that runs the mirror.
var errNotLinux = errors.New("the bandwidth shaper runs on Linux only, not on " + runtime.GOOS)

// Run is Linux only.
func Run(context.Context, Config, *log.Logger) error { return errNotLinux }

// Cleanup is Linux only.
func Cleanup(int, *log.Logger) error { return errNotLinux }
