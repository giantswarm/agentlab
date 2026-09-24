package lab

import (
	"testing"
)

// devChannelBranch is the dev channel the tests follow: the agent-platform
// branch whose meta chart builds carry kagent API v2.
const devChannelBranch = "poc/kagent-main"

// The dev-channel filter reads gitsemver's dev tags: a branch's own builds
// in gitsemver 3's shape, carrying the branch's hash (`gitsemver branch-hash
// poc/kagent-main` is d384adaf), or in the superseded shape, spelled with
// its sanitized name in either the full or the `--`-shortened form; never a
// release, never another branch's hash, never a sibling branch whose
// sanitized name shares a prefix, never a renovate branch.
func TestDevTagFilter(t *testing.T) {
	matches := devTagFilter(devChannelBranch)
	for _, ok := range []string{
		"rd384adaft20260924043558hcde53c0",
		"rd384adaft20260101000000h0000000",
		"dev.poc-kagent-main.2026-09-09.20-23-54.h28f7f50",
		"dev.poc-kagent-main.2026-09-10.08-12-33",
	} {
		if !matches(ok) {
			t.Errorf("%q must match poc/kagent-main", ok)
		}
	}
	for _, bad := range []string{
		"",
		"rc.1",
		"rbf28cd64t20260924043558hcde53c0",  // main's hash
		"r15b36cd0t20260924043558hcde53c0",  // renovate/axios-1.x's hash
		"rD384ADAFt20260924043558hcde53c0",  // gitsemver prints lowercase hex
		"rd384adaft2026092404355hcde53c0",   // 13 digits: not a time stamp
		"rd384adaft20260924043558cde53c0",   // no h before the commit
		"rd384adaft20260924043558hcde53c0x", // nothing after the 7 hex digits
		"rd384adaf.t20260924043558hcde53c0", // no dots: one identifier
		"dev.poc-kagent-main-2.2026-09-09.20-23-54.h28f7f50",
		"dev.poc-kagent.2026-09-09.20-23-54.h28f7f50",
		"dev.renovate-axios-1-x.2026-09-09.19-47-39.h4fb8c37",
		"dev.poc-kagent-main.2026-09-09.h28f7f50",
		"dev.poc-kagent-main.2026-09-09.20-23-54.28f7f50",
		"dev.POC-kagent-main.2026-09-09.20-23-54.h28f7f50",
	} {
		if matches(bad) {
			t.Errorf("%q must not match poc/kagent-main", bad)
		}
	}

	// gitsemver shortens a long branch to head--tail within its version
	// budget; the head and tail are the sanitized name's own ends.
	long := devTagFilter("feat/agent-workspaces-management-v2")
	if !long("dev.feat-agent-w--management-v2.2026-09-10.08-12-33.h1234567") {
		t.Error("the truncated form of the branch must match")
	}
	if long("dev.feat-agent-w--management-v3.2026-09-10.08-12-33.h1234567") {
		t.Error("a truncated tail of another branch must not match")
	}
	if long("dev.feat-agent-workspaces-management-v2--x.2026-09-10.08-12-33.h1234567") {
		t.Error("a marker that keeps more than the name must not match")
	}
	if devTagFilter("main")("dev.ma--in.2026-09-10.08-12-33.h1234567") {
		t.Error("a name that fits is never truncated: head+tail must be shorter than the name")
	}
}

// newestSuperseded is the branch's newest build of the superseded shape
// among the tags below.
const newestSuperseded = "3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be"

// Among a repository's tags the newest dev build of the branch wins: a later
// timestamp beats an earlier one whatever the list order, a higher base
// version beats a lower one, releases and other branches never count.
func TestPickDevTag(t *testing.T) {
	tags := []string{
		"3.22.0",
		"3.21.2",
		newestSuperseded,
		"3.22.1-dev.poc-kagent-main.2026-09-09.20-23-54.h28f7f50",
		"3.22.1-dev.renovate-axios-1-x.2026-09-11.19-47-39.h4fb8c37",
		"3.22.1-dev.poc-kagent-main-2.2026-09-12.10-00-00.haaaaaaa",
		"3.22.1-rc.1",
		"not-a-version",
	}
	got, ok := pickDevTag(tags, devChannelBranch)
	if !ok || got != newestSuperseded {
		t.Errorf("pickDevTag = %q, %v; want the 2026-09-10 build", got, ok)
	}
	// A newer base version (the branch rebased over a release) beats an
	// older base with a later timestamp: that is the semver order Flux and
	// Helm resolve by too, and the branch's next build carries it forward.
	got, _ = pickDevTag(append(tags, "3.23.1-dev.poc-kagent-main.2026-09-01.00-00-00.h0000000"), devChannelBranch)
	if got != "3.23.1-dev.poc-kagent-main.2026-09-01.00-00-00.h0000000" {
		t.Errorf("a higher base must win: %q", got)
	}
	if got, ok := pickDevTag(tags, "renovate/axios-1.x"); !ok || got != "3.22.1-dev.renovate-axios-1-x.2026-09-11.19-47-39.h4fb8c37" {
		t.Errorf("renovate branch: %q, %v", got, ok)
	}

	// The branch's first build after the move to gitsemver 3 carries the
	// same base and a prerelease that sorts above every superseded one
	// (`r` > `d`), so the channel moves onto it; among current builds the
	// fixed-width time stamps order them, and another branch's hash at a
	// later time never counts.
	current := append(tags,
		"3.22.1-rd384adaft20260923221721h39c3e43",
		"3.22.1-rd384adaft20260924043558hcde53c0",
		"3.22.1-rbf28cd64t20260925000000h1111111",
		"3.22.1-r15b36cd0t20260925000000h2222222",
	)
	if got, ok := pickDevTag(current, devChannelBranch); !ok || got != "3.22.1-rd384adaft20260924043558hcde53c0" {
		t.Errorf("the newest current build must win: %q, %v", got, ok)
	}
	if got, ok := pickDevTag(current, "renovate/axios-1.x"); !ok || got != "3.22.1-r15b36cd0t20260925000000h2222222" {
		t.Errorf("renovate branch, current shape: %q, %v", got, ok)
	}
	// A current build of a lower base (the branch's re-pin or rebase moved
	// its ancestry below the release it used to count from) does not
	// outrank the superseded build of the higher base: semver order rules,
	// as it does for Flux.
	if got, _ := pickDevTag(append(tags, "3.21.3-rd384adaft20260924043558hcde53c0"), devChannelBranch); got != newestSuperseded {
		t.Errorf("a lower base loses whatever its shape: %q", got)
	}
	if _, ok := pickDevTag(tags, "main"); ok {
		t.Error("a branch without builds must not resolve")
	}
	if _, ok := pickDevTag(nil, devChannelBranch); ok {
		t.Error("no tags must not resolve")
	}
}
