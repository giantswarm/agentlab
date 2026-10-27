package lab

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// elfFixture writes a minimal ELF64 executable carrying the given program
// headers and nothing else — what linuxStaticBinary reads of a binary. A
// `go build` with cgo on a glibc host carries PT_INTERP (the dynamic loader
// it asks for), a `CGO_ENABLED=0 go build` does not.
func elfFixture(t *testing.T, progs ...elf.ProgType) string {
	t.Helper()
	header := elf.Header64{
		Ident:     [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     uint16(len(progs)), // #nosec G115 -- a handful of headers
		Shentsize: 64,
	}
	if len(progs) > 0 {
		header.Phoff = uint64(header.Ehsize)
	}
	var out bytes.Buffer
	if err := binary.Write(&out, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	for _, p := range progs {
		prog := elf.Prog64{Type: uint32(p)} // #nosec G115 -- a program header type
		if err := binary.Write(&out, binary.LittleEndian, prog); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "agentlab")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLinuxStaticBinary: a static Linux binary passes; a dynamically linked
// one (PT_INTERP) is refused with the build that makes it static, and a
// command without a flag for another binary is told to run from one; a file
// that is no ELF is refused as not a Linux executable.
func TestLinuxStaticBinary(t *testing.T) {
	static := elfFixture(t, elf.PT_LOAD, elf.PT_NOTE)
	if err := linuxStaticBinary(static, "the skill host", ""); err != nil {
		t.Errorf("a static binary: %v", err)
	}
	dynamic := elfFixture(t, elf.PT_LOAD, elf.PT_INTERP, elf.PT_DYNAMIC)
	err := linuxStaticBinary(dynamic, "the skill host", "")
	if err == nil {
		t.Fatal("a dynamically linked binary passed")
	}
	for _, want := range []string{"the skill host runs " + dynamic, "dynamically linked", "`CGO_ENABLED=0 go build`", "`make build`", "run it from a static agentlab"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a dynamically linked binary: %v\n  wants %q", err, want)
		}
	}
	if err := linuxStaticBinary(dynamic, "the mock LLM", "--mockllm-binary"); err == nil || !strings.Contains(err.Error(), "pass --mockllm-binary with a static agentlab") {
		t.Errorf("a dynamically linked binary with a flag: %v", err)
	}
	script := filepath.Join(t.TempDir(), "agentlab")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linuxStaticBinary(script, "the skill host", ""); err == nil || !strings.Contains(err.Error(), "not a Linux executable") {
		t.Errorf("a script: %v", err)
	}
}
