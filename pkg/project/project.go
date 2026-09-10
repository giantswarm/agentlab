// Package project carries the build identity of the agentlab binary: its
// version, git commit and build time. The generated Makefile (devctl) and the
// architect CI stamp them through `-ldflags -X`; a plain `go build` or
// `go install` has no ldflags and falls back to the module build info, which
// Go 1.24+ derives from the VCS tag of the checkout.
package project

import (
	"runtime/debug"
	"strings"
)

// Name and Source identify the project in messages and reports.
const (
	Name   = "agentlab"
	Source = "https://github.com/giantswarm/agentlab"
)

// Set at link time by the Makefile / architect (`-X <module>/pkg/project.version=…`).
var (
	version        string
	buildTimestamp string
	gitSHA         string
)

// Version returns the agentlab version in the form the tags use ("v0.17.0"):
// the linker-stamped one when the binary came out of make or the release
// pipeline; else the module version Go recorded, which is the tag for
// `go install …@v0.17.0` and for a `go build` at the tag, a pseudo-version
// between tags, and carries "+dirty" over local edits; else "dev".
func Version() string {
	if v := strings.TrimSpace(version); v != "" {
		if v[0] >= '0' && v[0] <= '9' {
			v = "v" + v
		}
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// GitSHA returns the commit the binary was built from, or "" when unknown.
func GitSHA() string {
	if gitSHA != "" {
		return gitSHA
	}
	return buildSetting("vcs.revision")
}

// ShortSHA is GitSHA cut to the seven characters git shows, or "" when
// unknown — the build identifier the usage signal carries.
func ShortSHA() string {
	sha := GitSHA()
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return sha
}

// BuildTimestamp returns the build (or, unstamped, the commit) time in
// RFC 3339, or "" when unknown.
func BuildTimestamp() string {
	if buildTimestamp != "" {
		return buildTimestamp
	}
	return buildSetting("vcs.time")
}

// VersionLine is what `agentlab --version` prints after the name:
// "v0.17.0 (commit 8b5caa0, built 2026-09-07T10:00:00Z)".
func VersionLine() string {
	line := Version()
	var details []string
	if sha := ShortSHA(); sha != "" {
		details = append(details, "commit "+sha)
	}
	if ts := BuildTimestamp(); ts != "" {
		details = append(details, "built "+ts)
	}
	if len(details) > 0 {
		line += " (" + strings.Join(details, ", ") + ")"
	}
	return line
}

// ModuleVersion is the version of a dependency this binary was built with
// ("v4.2.4"), read from the module list Go records in every build — a release,
// a `go install`, a `go build` from a checkout alike — so what the binary
// reports about the libraries it embeds is true for this very build. "" when
// the module is not part of the build or the binary carries no build info.
func ModuleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != path {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version
		}
		return dep.Version
	}
	return ""
}

func buildSetting(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
