package lab

// The lab's questions outside `agentlab configure`: whether to trust the lab
// CA before an https URL goes to a browser (`agentlab open`, and the end of
// `up`). One implementation, so the wording and the sudo prompt are the same
// wherever it is asked — and asked only on a terminal: off one the lab prints
// the fact and goes on, it never blocks a scripted run.

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/forms"
)

// onTerminal reports whether stdin is a terminal — the gate on every question
// the lab asks. A variable so tests can stand in for the terminal.
var onTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// askConfirm is the lab's yes/no question, the huh confirm internal/forms
// owns. A variable so tests can answer without a TUI.
var askConfirm = forms.Confirm

// runTrust is `agentlab trust` — the command's own code path, with its own
// output. A variable so tests can assert the answer without touching a trust
// store.
var runTrust = Trust

// trustStep is what to do about an untrusted lab CA before an https lab URL
// goes to a browser.
type trustStep int

const (
	trustNothing trustStep = iota // the CA is in the system store: nothing to say
	trustAsk                      // on a terminal: offer `agentlab trust` first
	trustWarn                     // off a terminal: one line, then open anyway
)

// decideTrust is the whole decision, pure so the matrix is a table test.
func decideTrust(trusted, terminal bool) trustStep {
	switch {
	case trusted:
		return trustNothing
	case terminal:
		return trustAsk
	default:
		return trustWarn
	}
}

// offerTrust is the trust step of both `agentlab open` and the end of a boot:
// take the pre-answer or ask, and on a yes run `agentlab trust`. installed
// reports that the CA entered the stores in this run (the caller adds the
// restart hint), aborted that the person pressed Ctrl-C on the question.
//
// Nothing here fails the command: the offer sits on top of work that has
// already succeeded — a boot, or a URL that is about to open — so a refused
// sudo or a keychain that says no is a warning, and `agentlab trust` is still
// there on the next run.
//
// The answer is runTrust's error, never a re-probe: SystemTrusted verifies
// against Go's system pool, snapshotted once per process (trust.go), so it
// would still say untrusted right after a successful install.
func offerTrust(cfg *config.Config, pre *bool) (installed, aborted bool) {
	yes := false
	switch {
	case pre != nil:
		yes = *pre
	case onTerminal():
		fmt.Println()
		answer, err := askConfirm("Trust the lab CA now?",
			fmt.Sprintf("Browsers get a green lock on https://*.%s and the Dex login, and Node\naccepts the edge. One sudo prompt; `agentlab untrust` reverts it.",
				cfg.Platform.Domain),
			"Trust it", "Not now", true)
		switch {
		case errors.Is(err, forms.ErrAborted):
			return false, true
		case err != nil:
			warn("could not ask about the lab CA (%v) — `agentlab trust` installs it", err)
			return false, false
		}
		yes = answer
	}
	if !yes {
		return false, false
	}
	if err := runTrust(cfg); err != nil {
		warn("installing the lab CA failed: %v", err)
		warn("browsers keep warning until `agentlab trust` succeeds; nothing else about this run changed.")
		return false, false
	}
	return true, false
}

// Offers pre-answer the questions `up` and `platform` end with (the --trust
// and --open flags): nil asks on a terminal and stays silent off one, a value
// is the answer wherever the command runs.
type Offers struct {
	Trust *bool
	Open  *bool
}

// offerTrustAndOpen ends a boot with the two steps the summary used to only
// describe: trust the lab CA while it is untrusted, then open the portal.
// Nothing is asked off a terminal — the summary's own hints stay the answer
// there — and nothing here can fail the boot: every step is optional, so the
// caller keeps its exit status and its remaining bookkeeping either way.
//
// portalUp is the summary's reachability verdict: an unreachable portal is not
// worth a browser tab.
func offerTrustAndOpen(cfg *config.Config, offers Offers, portalUp bool) {
	justTrusted := false
	if !systemTrusted() {
		installed, aborted := offerTrust(cfg, offers.Trust)
		if aborted {
			return
		}
		justTrusted = installed
	}
	if !cfg.Backstage.Enabled || !portalUp {
		return
	}
	t := openTargets[openTargetPortal]
	open := false
	switch {
	case offers.Open != nil:
		open = *offers.Open
	case onTerminal():
		fmt.Println()
		yes, err := askConfirm("Open the portal in the browser now?", cfg.BackstageBaseURL(),
			"Open it", "Not now", true)
		switch {
		case errors.Is(err, forms.ErrAborted):
			return
		case err != nil:
			warn("could not ask about the portal (%v) — `agentlab open portal` opens it", err)
			return
		}
		open = yes
	}
	if open {
		announceAndOpen(t.what, t.url(cfg), openNotes(t, cfg, justTrusted))
	}
}

// warnUntrusted is what replaces the question off a terminal: the fact and the
// one command that fixes it.
func warnUntrusted(url string) {
	warn("the lab CA is not in the system trust store — the browser will warn on %s.", url)
	warn("one-time fix, reverted by `agentlab untrust`:  agentlab trust")
}
