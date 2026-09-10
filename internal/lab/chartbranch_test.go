package lab

import (
	"testing"
)

// devChannelBranch is the dev channel the tests follow: the agent-platform
// branch whose meta chart builds carry kagent API v2.
const devChannelBranch = "poc/kagent-main"

// The dev-channel filter reads gitsemver's dev tags: a branch's own builds,
// spelled with its sanitized name, in either the full or the `--`-shortened
// form; never a release, never a sibling branch whose sanitized name shares
// a prefix, never a renovate branch.
func TestDevTagFilter(t *testing.T) {
	matches := devTagFilter(devChannelBranch)
	for _, ok := range []string{
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

// Among a repository's tags the newest dev build of the branch wins: a later
// timestamp beats an earlier one whatever the list order, a higher base
// version beats a lower one, releases and other branches never count.
func TestPickDevTag(t *testing.T) {
	tags := []string{
		"3.22.0",
		"3.21.2",
		"3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be",
		"3.22.1-dev.poc-kagent-main.2026-09-09.20-23-54.h28f7f50",
		"3.22.1-dev.renovate-axios-1-x.2026-09-11.19-47-39.h4fb8c37",
		"3.22.1-dev.poc-kagent-main-2.2026-09-12.10-00-00.haaaaaaa",
		"3.22.1-rc.1",
		"not-a-version",
	}
	got, ok := pickDevTag(tags, devChannelBranch)
	if !ok || got != "3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be" {
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
	if _, ok := pickDevTag(tags, "main"); ok {
		t.Error("a branch without builds must not resolve")
	}
	if _, ok := pickDevTag(nil, devChannelBranch); ok {
		t.Error("no tags must not resolve")
	}
}
