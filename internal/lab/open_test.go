package lab

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/forms"
)

// openStubs is the world Open and the end-of-boot offers reach outside this
// process — the trust probe, the terminal, the question, `agentlab trust`, the
// node's docker state, the reachability probe and the OS opener — as told to
// them, plus what they did with it. install swaps the package vars for one
// test and restores them afterwards.
type openStubs struct {
	// What the world says.
	caTrusted bool
	terminal  bool
	answer    bool
	answerErr error
	trustErr  error
	nodeState string
	nodeErr   error
	up        bool
	probed    bool

	// What the code did with it.
	asked    int
	trustRun int
	opened   []string
	probedAt []string
}

// runningLab is the happy path every test starts from: the node is up, the
// target answers, the lab CA is trusted, nothing is a terminal.
func runningLab() openStubs {
	return openStubs{caTrusted: true, nodeState: stateRunning, up: true, probed: true}
}

func (s *openStubs) install(t *testing.T) *openStubs {
	t.Helper()
	oldTrusted, oldTerm, oldAsk, oldTrust, oldState, oldProbe, oldOpen :=
		systemTrusted, onTerminal, askConfirm, runTrust, clusterNodeState, probeReachable, openBrowser
	t.Cleanup(func() {
		systemTrusted, onTerminal, askConfirm, runTrust, clusterNodeState, probeReachable, openBrowser =
			oldTrusted, oldTerm, oldAsk, oldTrust, oldState, oldProbe, oldOpen
	})
	systemTrusted = func() bool { return s.caTrusted }
	onTerminal = func() bool { return s.terminal }
	askConfirm = func(_, _, _, _ string, _ bool) (bool, error) {
		s.asked++
		return s.answer, s.answerErr
	}
	runTrust = func(*config.Config) error {
		s.trustRun++
		return s.trustErr
	}
	clusterNodeState = func(string) (string, error) { return s.nodeState, s.nodeErr }
	probeReachable = func(url string) (bool, bool) {
		s.probedAt = append(s.probedAt, url)
		return s.up, s.probed
	}
	openBrowser = func(url string) { s.opened = append(s.opened, url) }
	return s
}

func TestOpenTargetsFeedValidArgs(t *testing.T) {
	got := OpenTargets()
	if want := []string{openTargetAgents, openTargetPortal}; !slices.Equal(got, want) {
		t.Fatalf("OpenTargets() = %v, want %v", got, want)
	}
	if _, ok := openTargets[openTargetPortal]; !ok {
		t.Errorf("openTargetPortal %q is not in the table", openTargetPortal)
	}
}

// The URLs are the configuration's own, port suffix included: an edge that
// left 443 must be opened on the port it actually serves.
func TestOpenTargetURLsFollowTheConfiguration(t *testing.T) {
	for _, tc := range []struct {
		port                  int
		agentsPort            int
		wantPortal, wantAgent string
	}{
		{443, 8081, "https://backstage.127.0.0.1.nip.io", "http://localhost:8081"},
		{8443, 9090, "https://backstage.127.0.0.1.nip.io:8443", "http://localhost:9090"},
	} {
		t.Run(fmt.Sprint(tc.port), func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.GatewayPort = tc.port
			cfg.Platform.AgentsPort = tc.agentsPort
			if got := openTargets[openTargetPortal].url(cfg); got != tc.wantPortal {
				t.Errorf("portal url = %q, want %q", got, tc.wantPortal)
			}
			if got := openTargets[openTargetAgents].url(cfg); got != tc.wantAgent {
				t.Errorf("agents url = %q, want %q", got, tc.wantAgent)
			}
		})
	}
}

// Without an argument the refusal names both targets: that is how a person
// discovers there is more than one thing to open.
func TestOpenWithoutATargetNamesThem(t *testing.T) {
	lab := runningLab()
	s := lab.install(t)
	err := Open(config.Default(), "")
	if err == nil {
		t.Fatal("Open(cfg, \"\") = nil, want a refusal")
	}
	for _, want := range OpenTargets() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if len(s.opened) != 0 {
		t.Errorf("opened %v", s.opened)
	}
}

func TestOpenUnknownTarget(t *testing.T) {
	lab := runningLab()
	s := lab.install(t)
	err := Open(config.Default(), "prometheus")
	if err == nil || !strings.Contains(err.Error(), `"prometheus"`) || !strings.Contains(err.Error(), "portal") {
		t.Fatalf("Open(cfg, \"prometheus\") = %v, want a refusal naming the target and the valid ones", err)
	}
	if len(s.opened) != 0 {
		t.Errorf("opened %v", s.opened)
	}
}

// A target this configuration does not serve is refused by name — and before
// docker is asked anything, so the refusal works on any machine.
func TestOpenRefusesDisabledTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		mutate func(*config.Config)
		want   string
	}{
		{"backstage off", "portal", func(c *config.Config) { c.Backstage.Enabled = false }, "backstage.enabled"},
		{"platform off", openTargetAgents, func(c *config.Config) { c.Platform.Enabled = false; c.Backstage.Enabled = false }, "platform.enabled"},
		{"agents off", openTargetAgents, func(c *config.Config) { c.Platform.Agents = false }, "platform.agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab := runningLab()
			s := lab.install(t)
			clusterNodeState = func(string) (string, error) {
				t.Error("asked docker for the node state on the refusal path")
				return stateRunning, nil
			}
			cfg := config.Default()
			tc.mutate(cfg)
			err := Open(cfg, tc.target)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open() = %v, want a refusal naming %s", err, tc.want)
			}
			if len(s.opened) != 0 {
				t.Errorf("opened %v", s.opened)
			}
		})
	}
}

// A lab that is not running, or whose node is stopped, is refused with the
// command that fixes it — no browser tab on a dead URL.
func TestOpenRefusesWhileTheClusterIsNotRunning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		err   error
	}{
		{"no node container", "", errors.New("no such object")},
		{"node exited", "exited", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab := runningLab()
			lab.nodeState, lab.nodeErr = tc.state, tc.err
			s := lab.install(t)
			err := Open(config.Default(), openTargetPortal)
			if err == nil || !strings.Contains(err.Error(), "agentlab up") {
				t.Fatalf("Open() = %v, want a refusal pointing at `agentlab up`", err)
			}
			if len(s.opened) != 0 {
				t.Errorf("opened %v", s.opened)
			}
		})
	}
}

func TestOpenHandsTheTargetURLToTheOpener(t *testing.T) {
	lab := runningLab()
	s := lab.install(t)
	cfg := config.Default()
	if err := Open(cfg, openTargetPortal); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := []string{cfg.BackstageBaseURL()}; !slices.Equal(s.opened, want) {
		t.Fatalf("opened %v, want %v", s.opened, want)
	}
	if !slices.Equal(s.probedAt, []string{cfg.BackstageBaseURL()}) {
		t.Errorf("probed %v, want the target URL", s.probedAt)
	}
}

// The probe decides between "not answering yet" and an error page in the
// browser — unless the probe itself could not run, which is not the target's
// fault.
func TestOpenAndTheReachabilityProbe(t *testing.T) {
	t.Run("unreachable refuses with the hint", func(t *testing.T) {
		lab := runningLab()
		lab.up = false
		s := lab.install(t)
		err := Open(config.Default(), openTargetPortal)
		if err == nil || !strings.Contains(err.Error(), "not answering") || !strings.Contains(err.Error(), "agentlab logs backstage") {
			t.Fatalf("Open() = %v, want the not-answering refusal with the hint", err)
		}
		if len(s.opened) != 0 {
			t.Errorf("opened %v", s.opened)
		}
	})
	t.Run("an unusable probe opens anyway", func(t *testing.T) {
		lab := runningLab()
		lab.up, lab.probed = false, false
		s := lab.install(t)
		if err := Open(config.Default(), openTargetPortal); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if len(s.opened) != 1 {
			t.Fatalf("opened %v, want the target once", s.opened)
		}
	})
}

// A target that is not answering is refused before the trust question, so its
// sudo prompt is never spent on a command that then opens nothing.
func TestOpenDoesNotAskAboutTrustBeforeRefusing(t *testing.T) {
	lab := runningLab()
	lab.caTrusted, lab.terminal, lab.answer, lab.up = false, true, true, false
	s := lab.install(t)
	if err := Open(config.Default(), openTargetPortal); err == nil {
		t.Fatal("Open() = nil, want the not-answering refusal")
	}
	if s.asked != 0 || s.trustRun != 0 {
		t.Errorf("asked %d questions and ran trust %d times before refusing", s.asked, s.trustRun)
	}
}

// A trust install that fails must not cost the person the one thing the
// command exists for: the URL still prints, the browser still opens.
func TestOpenSurvivesAFailedTrustInstall(t *testing.T) {
	lab := runningLab()
	lab.caTrusted, lab.terminal, lab.answer = false, true, true
	lab.trustErr = errors.New("installing into the system store: exit status 1")
	s := lab.install(t)
	cfg := config.Default()
	if err := Open(cfg, openTargetPortal); err != nil {
		t.Fatalf("Open() = %v, want nil: the trust step is optional", err)
	}
	if s.trustRun != 1 || !slices.Equal(s.opened, []string{cfg.BackstageBaseURL()}) {
		t.Errorf("ran trust %d times, opened %v", s.trustRun, s.opened)
	}
}

// A CA installed in this very run leaves an already-running browser with the
// old verdict, so the announcement says to restart it.
func TestOpenNotesMentionARestartOnlyAfterInstalling(t *testing.T) {
	cfg := config.Default()
	portal := openTargets[openTargetPortal]
	const restart = "restart an already-running browser"
	if notes := strings.Join(openNotes(portal, cfg, true), " "); !strings.Contains(notes, restart) {
		t.Errorf("notes after installing = %q, want the restart hint", notes)
	}
	if notes := strings.Join(openNotes(portal, cfg, false), " "); strings.Contains(notes, restart) {
		t.Errorf("notes without installing = %q, want no restart hint", notes)
	}
	// The kagent UI defines no notes at all: the nil func must not be called.
	if notes := openNotes(openTargets[openTargetAgents], cfg, false); len(notes) != 0 {
		t.Errorf("agents notes = %v, want none", notes)
	}
}

// The trust question is asked for the https target on a terminal only, and
// never for the kagent UI, which is plain HTTP on loopback.
func TestOpenAndTheTrustQuestion(t *testing.T) {
	t.Run("yes trusts, then opens", func(t *testing.T) {
		lab := runningLab()
		lab.caTrusted, lab.terminal, lab.answer = false, true, true
		s := lab.install(t)
		if err := Open(config.Default(), openTargetPortal); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if s.asked != 1 || s.trustRun != 1 || len(s.opened) != 1 {
			t.Fatalf("asked %d, trusted %d, opened %v", s.asked, s.trustRun, s.opened)
		}
	})
	t.Run("no opens untrusted", func(t *testing.T) {
		lab := runningLab()
		lab.caTrusted, lab.terminal = false, true
		s := lab.install(t)
		if err := Open(config.Default(), openTargetPortal); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if s.asked != 1 || s.trustRun != 0 || len(s.opened) != 1 {
			t.Fatalf("asked %d, trusted %d, opened %v", s.asked, s.trustRun, s.opened)
		}
	})
	t.Run("Ctrl-C opens nothing", func(t *testing.T) {
		lab := runningLab()
		lab.caTrusted, lab.terminal, lab.answerErr = false, true, forms.ErrAborted
		s := lab.install(t)
		if err := Open(config.Default(), openTargetPortal); err != nil {
			t.Fatalf("Open() = %v, want nil after an aborted question", err)
		}
		if len(s.opened) != 0 || s.trustRun != 0 {
			t.Fatalf("trusted %d, opened %v", s.trustRun, s.opened)
		}
	})
	t.Run("off a terminal it warns and opens", func(t *testing.T) {
		lab := runningLab()
		lab.caTrusted = false
		s := lab.install(t)
		if err := Open(config.Default(), openTargetPortal); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if s.asked != 0 || len(s.opened) != 1 {
			t.Fatalf("asked %d, opened %v", s.asked, s.opened)
		}
	})
	t.Run("the kagent UI never asks", func(t *testing.T) {
		lab := runningLab()
		lab.caTrusted, lab.terminal, lab.answer = false, true, true
		s := lab.install(t)
		if err := Open(config.Default(), openTargetAgents); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if s.asked != 0 || s.trustRun != 0 {
			t.Fatalf("asked %d, trusted %d for a plain-HTTP target", s.asked, s.trustRun)
		}
	})
}

// The boot summary's Try it block leads with the command that gets a person
// into the lab, and only offers a target this configuration serves.
func TestTryItBlockLeadsWithOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"the canonical lab", func(*config.Config) {}, "agentlab open portal"},
		{"no portal, but agents", func(c *config.Config) { c.Backstage.Enabled = false }, "agentlab open agents"},
		{"neither", func(c *config.Config) { c.Backstage.Enabled = false; c.Platform.Agents = false }, "agentlab login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			lines := strings.Split(strings.TrimLeft(tryItBlock(cfg), "\n"), "\n")
			if len(lines) < 2 || !strings.Contains(lines[1], tc.want) {
				t.Fatalf("Try it block:\n%s\nwant %q first", tryItBlock(cfg), tc.want)
			}
		})
	}
}
