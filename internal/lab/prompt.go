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

// askTrustNow asks the trust question and, on yes, runs `agentlab trust`.
// aborted reports Ctrl-C on the question, which callers treat as "never mind".
//
// The answer is runTrust's error: SystemTrusted verifies against Go's system
// pool, snapshotted once per process (trust.go), so re-probing after the
// install would still say untrusted.
func askTrustNow(cfg *config.Config) (aborted bool, err error) {
	yes, err := askConfirm("Trust the lab CA now?",
		fmt.Sprintf("Browsers get a green lock on https://*.%s and the Dex login, and Node\naccepts the edge. One sudo prompt; `agentlab untrust` reverts it.",
			cfg.Platform.Domain),
		"Trust it", "Not now", true)
	switch {
	case errors.Is(err, forms.ErrAborted):
		return true, nil
	case err != nil:
		return false, err
	case !yes:
		return false, nil
	}
	return false, runTrust(cfg)
}

// Offers pre-answer the questions `up` and `platform` end with (the --trust
// and --open flags): nil asks on a terminal and stays silent off one, a value
// is the answer wherever the command runs.
type Offers struct {
	Trust *bool
	Open  *bool
}

// offerTrustAndOpen ends a boot with the two steps the summary used to only
// describe: trust the lab CA while it is untrusted, then open the portal —
// in that order, so the page opens without a certificate warning. Nothing is
// asked off a terminal; the summary's own hints stay the answer there.
//
// portalUp is the summary's reachability verdict: an unreachable portal is not
// worth a browser tab.
func offerTrustAndOpen(cfg *config.Config, offers Offers, portalUp bool) error {
	if !systemTrusted() {
		switch {
		case offers.Trust != nil:
			if *offers.Trust {
				if err := runTrust(cfg); err != nil {
					return err
				}
			}
		case onTerminal():
			fmt.Println()
			aborted, err := askTrustNow(cfg)
			if err != nil {
				return err
			}
			if aborted {
				return nil
			}
		}
	}
	if !cfg.Backstage.Enabled || !portalUp {
		return nil
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
			return nil
		case err != nil:
			return err
		}
		open = yes
	}
	if open {
		announceAndOpen(t.what, t.url(cfg), t.notes(cfg))
	}
	return nil
}

// warnUntrusted is what replaces the question off a terminal: the fact and the
// one command that fixes it.
func warnUntrusted(url string) {
	warn("the lab CA is not in the system trust store — the browser will warn on %s.", url)
	warn("one-time fix, reverted by `agentlab untrust`:  agentlab trust")
}
