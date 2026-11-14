//go:build !unix

package shaper

// modeFileFlags: the shaper runs on Linux only; elsewhere the mode file is
// written by `agentlab shaper` and read by nothing but its status.
const modeFileFlags = 0
