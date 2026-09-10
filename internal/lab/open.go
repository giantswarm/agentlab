package lab

// `agentlab open <portal|agents>`: hand one lab URL to the OS opener and
// print it, so it can be copied where the opener finds no browser (SSH, WSL).

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// openTarget is one thing `agentlab open` puts in a browser: how its URL is
// built from the configuration, whether this configuration serves it at all,
// and what to check while it does not answer.
type openTarget struct {
	// what names the target in the output ("the portal (Backstage)").
	what string
	// url is what the browser gets.
	url func(*config.Config) string
	// tls marks the URLs the lab CA signs, the only ones a browser warns
	// about — so the only ones the trust question applies to.
	tls bool
	// enabled returns nil when this configuration serves the target, and
	// otherwise the refusal naming the agentlab.yaml key.
	enabled func(*config.Config) error
	// hint is where to look when the target does not answer.
	hint string
	// notes are printed between the announcement and the URL.
	notes func(*config.Config) []string
}

// openTargetPortal is the portal's target name: `up`'s open question opens the
// same target `agentlab open portal` does.
const openTargetPortal = "portal"

// openTargets maps each target name to how it is opened; the table also feeds
// cobra's ValidArgs via OpenTargets, so dispatch and completion cannot drift.
// The names say what the thing is to the person opening it, not which chart
// serves it: the portal is Backstage, the agents UI is kagent's.
var openTargets = map[string]openTarget{
	openTargetPortal: {
		what: "the portal (Backstage)",
		url:  (*config.Config).BackstageBaseURL,
		tls:  true,
		// Only backstage.enabled: Backstage without the platform is refused
		// by config.Validate, so there is no platform-off case to name here
		// (unlike the agents target below).
		enabled: func(c *config.Config) error {
			if c.Backstage.Enabled {
				return nil
			}
			return fmt.Errorf("backstage.enabled is false in %s — enable it (`agentlab configure --defaults --backstage`), then run `agentlab up`", config.File)
		},
		hint: "check `agentlab logs backstage` and `KUBECONFIG=" + labKubeconfigPath + " kubectl -n " + platformNamespace + " get pods`",
		notes: func(c *config.Config) []string {
			return []string{"Sign In -> Dex; users and passwords are in " + config.File}
		},
	},
	"agents": {
		what: "the kagent UI",
		// http://localhost:<platform.agentsPort>, a kind-mapped NodePort: no
		// certificate, so the lab CA plays no part (tls stays false).
		url: (*config.Config).KagentUIBaseURL,
		enabled: func(c *config.Config) error {
			if !c.Platform.Enabled {
				return fmt.Errorf("the agent platform is disabled in %s (platform.enabled) — the kagent UI comes with it", config.File)
			}
			if !c.Platform.Agents {
				return fmt.Errorf("platform.agents is false in %s — enable it (`agentlab configure --defaults --agents`), then run `agentlab up`", config.File)
			}
			return nil
		},
		hint: "check `KUBECONFIG=" + labKubeconfigPath + " kubectl -n " + kagentNamespace + " get pods`",
	},
}

// OpenTargets lists what Open accepts, for cobra's ValidArgs.
func OpenTargets() []string { return slices.Sorted(maps.Keys(openTargets)) }

// clusterNodeState reads the lab node container's docker state. A variable so
// tests can stand in for the cluster.
var clusterNodeState = nodeState

// openBrowser hands a URL to the OS opener. A variable so tests can assert
// what would have been opened.
var openBrowser = OpenBrowser

// probeReachable is the one-shot version of the boot summary's wait loop: does the
// target answer right now? probed is false when the check itself could not run
// (no lab CA on disk yet), which is not the target's fault.
var probeReachable = func(url string) (up, probed bool) {
	client, err := labHTTPClient(3 * time.Second)
	if err != nil {
		return false, false
	}
	return httpUp(client, url), true
}

// Open hands one lab URL to the OS opener and prints it. Refuses when the
// target is disabled in agentlab.yaml, when the cluster is not running or when
// the target does not answer yet — a plain fact beats an error page in the
// browser — and offers `agentlab trust` first while the lab CA is untrusted.
func Open(cfg *config.Config, target string) error {
	if target == "" {
		return fmt.Errorf("pick what to open: agentlab open <%s>\n  %s",
			strings.Join(OpenTargets(), "|"), strings.Join(targetSummaries(), "\n  "))
	}
	t, ok := openTargets[target]
	if !ok {
		return fmt.Errorf("unknown target %q (%s)", target, strings.Join(OpenTargets(), ", "))
	}
	if err := t.enabled(cfg); err != nil {
		return err
	}
	if err := requireRunningCluster(cfg); err != nil {
		return err
	}
	url := t.url(cfg)
	if t.tls {
		switch decideTrust(systemTrusted(), onTerminal()) {
		case trustAsk:
			aborted, err := askTrustNow(cfg)
			if err != nil {
				return err
			}
			if aborted {
				return nil
			}
		case trustWarn:
			warnUntrusted(url)
		case trustNothing:
		}
	}
	if up, probed := probeReachable(url); probed && !up {
		return fmt.Errorf("%s is not answering on %s yet — %s", t.what, url, t.hint)
	}
	var notes []string
	if t.notes != nil {
		notes = t.notes(cfg)
	}
	announceAndOpen(t.what, url, notes)
	return nil
}

// announceAndOpen is the printing and opening `agentlab open` and the
// end-of-`up` question share: the URL is always printed, because the OS opener
// is fire-and-forget and finds no browser over SSH or on WSL.
func announceAndOpen(what, url string, notes []string) {
	fmt.Printf("Opening %s in your browser...\n", what)
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}
	fmt.Printf("  if it does not open: %s\n\n", url)
	openBrowser(url)
}

// targetSummaries is the "portal: the portal (Backstage)" list the argument-less
// refusal shows, so the targets are discovered from the error itself.
func targetSummaries() []string {
	var out []string
	for _, name := range OpenTargets() {
		out = append(out, fmt.Sprintf("%-7s %s", name, openTargets[name].what))
	}
	return out
}

// requireRunningCluster refuses before a browser opens on a lab that is not
// there, or whose node is stopped: nothing behind those URLs would answer.
func requireRunningCluster(cfg *config.Config) error {
	node := cfg.ControlPlaneNode()
	state, err := clusterNodeState(node)
	if err != nil {
		return fmt.Errorf("no node container %s — the lab is not up (`agentlab up`)", node)
	}
	if state != "running" {
		return fmt.Errorf("the node container %s is %s, not running — `agentlab up` starts the lab again", node, state)
	}
	return nil
}
