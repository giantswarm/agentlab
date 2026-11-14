//go:build !linux

package shaper

import "context"

// Proof is Linux only: it builds its topology in Linux namespaces.
func Proof(context.Context, ProofOptions) error { return errNotLinux }

// ProofInNamespace is Linux only.
func ProofInNamespace(context.Context, ProofOptions) error { return errNotLinux }
