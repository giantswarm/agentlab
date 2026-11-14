//go:build unix

package shaper

import "syscall"

// modeFileFlags open the mode file without following a link and without
// blocking on a FIFO (readModeFile).
const modeFileFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
