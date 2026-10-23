package lab

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The beekeeper central proof runs beekeeper serve, the central instance,
// as beekeeper-decisions-test does (on the host, over the lab cluster's
// beekeeper resources, registered in the lab's muster with the person's
// token forwarded), and two local beekeepers beside it, one per lab user,
// each with a configuration and state of its own whose central context is
// the lab's muster. A lease one claims is refused to the other with its
// holder; a hold on a central lane set on one machine stops the other's;
// two people's merges into one central lane take their turns one after the
// other; a machine's agents appear on the central roster; and a central
// verb that cannot reach the central instance is refused with exit 69 while
// the machine's own resources stay claimable; and a decision one person's
// CLI files for the other is answered on the other's machine, its answer
// printed once by the filer's watch across a restart
// (beekeepercentralnotes.go).

// BeekeeperCentralVersionDefault is the first beekeeper release whose local
// binary reaches the central instance.
const BeekeeperCentralVersionDefault = "v0.80.0-rc.5"

// The proof's names: the installation the two machines share, its lane and
// the repository merging into it, and the resource each machine keeps.
const (
	centralEnvironment = "agentlab-shared"
	centralLane        = "agentlab-shared-lane"
	centralRepo        = "giantswarm/agentlab-central-proof"
	centralLocal       = "kind-local"
	centralContext     = "agentlab"
	// centralSession is the session the admin's machine registers as an
	// agent, under a fixed id of the proof's.
	centralSession = "Agent one"
	// The two machines' agents.
	centralAnaAgent  = "ana-agent"
	centralPiaAgent  = "pia-agent"
	centralSessionID = "4b1d1c64-0000-4000-8000-000000000157"
	// exitCentral is beekeeper's refusal of a central verb whose instance
	// does not answer; exitRefused a refusal.
	exitCentral = 69
	exitRefused = 3
)

// BeekeeperCentralTestOptions tunes the beekeeper central proof.
type BeekeeperCentralTestOptions struct {
	// Version is the beekeeper release whose image and CRDs run (default
	// BeekeeperCentralVersionDefault); Binary, a local static linux build,
	// takes precedence for serve and the two machines — the proof of a branch.
	Version string
	Binary  string
	// Port is beekeeper serve's in the node's network, Port+1 its
	// Postgres'. Default 18093.
	Port   int
	RunDir string
	// Chart, a beekeeper chart directory or oci:// reference (at Version),
	// runs serve in the lab cluster from the chart instead of on the host,
	// as an installation runs it (beekeepercentralchart.go); Binary then
	// runs the two machines only.
	Chart string
	// Part, one of CentralParts, runs that step alone.
	Part string
}

// BeekeeperCentralTest is the headless proof of the central resources in
// the local beekeeper.
func BeekeeperCentralTest(cfg *config.Config, opts BeekeeperCentralTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("the platform is off in %s — the machines reach serve through its muster; enable it and run `agentlab platform` first", config.File)
	}
	if opts.Part != "" && !slices.Contains(CentralParts, opts.Part) {
		return fmt.Errorf("--part %q is none of %s", opts.Part, strings.Join(CentralParts, ", "))
	}
	ana, pia, group, err := beekeeperUsers(cfg)
	if err != nil {
		return err
	}
	opts.Version = cmp.Or(opts.Version, BeekeeperCentralVersionDefault)
	opts.Port = cmp.Or(opts.Port, 18093)
	if opts.Chart == "" {
		if err := portsFree(opts.Port, opts.Port+1); err != nil {
			return err
		}
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	tokens := map[string]string{}
	for _, u := range []*config.User{ana, pia} {
		step("Logging in to Dex as %s", u.Email)
		tok, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		tokens[u.Email] = tok
	}

	caFile, err := filepath.Abs(trustBundleFile(cfg))
	if err != nil {
		return err
	}
	removeNS, err := ensureBeekeeperNamespace()
	if err != nil {
		return err
	}
	defer removeNS()
	image := beekeeperImage + ":" + strings.TrimPrefix(opts.Version, "v")
	var url string
	var stopServe func()
	if opts.Chart != "" {
		url, stopServe, err = startBeekeeperChart(cfg, opts.Chart, opts.Version, group)
	} else {
		url, stopServe, err = startBeekeeperHost(cfg, opts, image, group, caFile, runDir)
	}
	if err != nil {
		return err
	}
	defer stopServe()
	step("Registering MCPServer %s at %s in muster, the person's token forwarded", beekeeperServer, url)
	removeServer, err := registerBeekeeperServer(url)
	if err != nil {
		return err
	}
	defer removeServer()
	anaS, err := openMusterSession(cfg, tokens[ana.Email], "agentlab-beekeeper-central")
	if err != nil {
		return err
	}
	if err := waitBeekeeperTools(anaS); err != nil {
		return err
	}
	piaS, err := openMusterSession(cfg, tokens[pia.Email], "agentlab-beekeeper-central-other")
	if err != nil {
		return err
	}

	bin := opts.Binary
	if bin == "" {
		if bin, err = beekeeperFromImage(image, runDir); err != nil {
			return err
		}
	}
	endpoint := cfg.MusterBaseURL() + "/mcp"
	m := &centralProof{bin: bin, ca: caFile, anaS: anaS, piaS: piaS, ana: ana.Email, pia: pia.Email}
	if m.anaCfg, err = centralMachine(runDir, "ana", ana.Email, endpoint, tokens[ana.Email]); err != nil {
		return err
	}
	if m.piaCfg, err = centralMachine(runDir, "pia", pia.Email, endpoint, tokens[pia.Email]); err != nil {
		return err
	}
	if m.downCfg, err = centralMachine(runDir, "down", pia.Email, "http://127.0.0.1:1/mcp", tokens[pia.Email]); err != nil {
		return err
	}

	if opts.Part == CentralPartNotes {
		return m.notesStep()
	}
	step("1. %s's machine claims %s; %s's claim is refused with its holder, its list shows it; released, %s claims it", ana.Email, centralEnvironment, pia.Email, pia.Email)
	if err := m.leases(); err != nil {
		return err
	}
	step("2. A hold on lane %s set on %s's machine stops %s's merges, listed as central there", centralLane, ana.Email, pia.Email)
	if err := m.holds(); err != nil {
		return err
	}
	step("3. Two people's merges into lane %s take their turns: the second's comes once the first merged and left", centralLane)
	if err := m.lanes(); err != nil {
		return err
	}
	step("4. %s's machine publishes its agent to the central roster (watch --once)", ana.Email)
	if err := m.roster(); err != nil {
		return err
	}
	step("5. Unreachable: a central claim is refused with exit %d, the machine's own %s is claimed", exitCentral, centralLocal)
	if err := m.unreachable(); err != nil {
		return err
	}
	step("6. A note %s files for team %s is listed for %s, who answers it; it is closed for both", ana.Email, beekeeperTeam, pia.Email)
	if err := m.notes(); err != nil {
		return err
	}
	if err := m.notesStep(); err != nil {
		return err
	}
	note("the central leases, holds, lanes, roster and notes proven across two machines")
	return nil
}

// notesStep is step 7, the central notes through the CLI.
func (p *centralProof) notesStep() error {
	step("7. A decision %s's CLI files --for %s is listed, queued and answered on %s's machine; %s's watch prints the answer once across a restart; a second one only its filer withdraws", p.ana, p.pia, p.pia, p.ana)
	if err := p.centralNotes(); err != nil {
		return err
	}
	note("central notes proven through both machines' CLIs and the filer's watch")
	return nil
}

// startBeekeeperHost runs serve from image (or opts.Binary) in the node's
// network on opts.Port, beside a Postgres on the next port, over the
// release's CRDs and the proof's Environment and lane; it returns serve's
// MCP URL and the func that removes everything it created.
func startBeekeeperHost(cfg *config.Config, opts BeekeeperCentralTestOptions, image, group, caFile, runDir string) (string, func(), error) {
	var undo []func()
	stop := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	fail := func(err error) (string, func(), error) {
		stop()
		return "", nil, err
	}
	servePort, pgPort := opts.Port, opts.Port+1
	node := cfg.ControlPlaneNode()
	nodeIP, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, node)
	if err != nil || strings.TrimSpace(nodeIP) == "" {
		return fail(fmt.Errorf("the address of node %s on network %s: %v", node, kindDockerNetwork, err))
	}
	nodeIP = strings.TrimSpace(nodeIP)
	inNode := "container:" + node
	run := strings.ToUpper(randomSuffix())

	// serve reads the lab cluster through it, from the node's network.
	if _, err := nodeKubeconfig(runDir); err != nil {
		return fail(err)
	}
	if err := copyFile(caFile, filepath.Join(runDir, beekeeperCAFile)); err != nil {
		return fail(err)
	}

	step("Applying beekeeper %s's CRDs, Environment %s and MergeLane %s", opts.Version, centralEnvironment, centralLane)
	removeCRDs, err := applyBeekeeperCRDs(opts.Version)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, removeCRDs)
	removeShared, err := applyCentralShared()
	if err != nil {
		return fail(err)
	}
	undo = append(undo, removeShared)

	step("Starting Postgres for serve's mailboxes in node %s's network on :%d (%s)", node, pgPort, beekeeperPostgresImage)
	stopPG, err := startBeekeeperPostgres(run, inNode, pgPort)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, stopPG)
	serveCfg := fmt.Sprintf("serve:\n  issuer: %q\n  clientIDs: [%q, %q]\n  organization: %q\n  teams: {%q: %q}\n",
		cfg.Issuer(), config.AgentPlatformClientID, config.KubernetesClientID, group, group, beekeeperTeam)
	step("Starting beekeeper serve (%s) in node %s's network on :%d", cmp.Or(opts.Binary, image), node, servePort)
	stopServe, err := startBeekeeperServe(image, opts.Binary, run, inNode, runDir, serveCfg, nodeIP, servePort, pgPort)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, stopServe)
	return "http://127.0.0.1:" + strconv.Itoa(servePort) + "/mcp", stop, nil
}

// applyCentralShared creates the shared installation and its lane; the
// returned func deletes them.
func applyCentralShared() (func(), error) {
	manifest := fmt.Sprintf(`apiVersion: beekeeper.giantswarm.io/v1alpha1
kind: Environment
metadata:
  name: %[1]s
  labels: {app.kubernetes.io/managed-by: %[4]s}
spec: {installation: %[1]s, team: %[5]s}
---
apiVersion: beekeeper.giantswarm.io/v1alpha1
kind: MergeLane
metadata:
  name: %[2]s
  labels: {app.kubernetes.io/managed-by: %[4]s}
spec: {installation: %[1]s, repositories: [%[3]s]}
`, centralEnvironment, centralLane, centralRepo, managedByAgentlabValue, beekeeperTeam)
	if _, err := applyManifests(context.Background(), []byte(manifest)); err != nil {
		return nil, err
	}
	return func() {
		for kind, name := range map[string]string{"environments.beekeeper.giantswarm.io": centralEnvironment, "mergelanes.beekeeper.giantswarm.io": centralLane} {
			if gvr, err := gvrFor(kind); err == nil {
				_ = deleteObject(context.Background(), gvr, "", name, 30*time.Second)
			}
		}
	}, nil
}

// beekeeperFromImage copies the image's binary into the run directory.
func beekeeperFromImage(image, runDir string) (string, error) {
	id, err := outputQuiet(dockerBin, "create", image)
	if err != nil {
		return "", fmt.Errorf("creating a container of %s: %w", image, err)
	}
	id = strings.TrimSpace(id)
	defer func() { _ = runQuiet(dockerBin, "rm", id) }()
	bin := filepath.Join(runDir, "beekeeper")
	if err := runQuiet(dockerBin, "cp", id+":/beekeeper", bin); err != nil {
		return "", fmt.Errorf("copying beekeeper out of %s: %w", image, err)
	}
	return bin, nil
}

// centralMachine writes one person's machine: its beekeeper configuration
// with a state of its own and the central context, and the muster it calls,
// which names endpoint and prints the person's token.
func centralMachine(runDir, name, email, endpoint, token string) (string, error) {
	dir := filepath.Join(runDir, "machine-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return "", err
	}
	muster := filepath.Join(dir, "muster")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\ncontext) printf '{\"name\":\"%s\",\"endpoint\":\"%s\"}' ;;\nauth) cat '%s' ;;\n*) exit 1 ;;\nesac\n", centralContext, endpoint, tokenFile)
	if err := os.WriteFile(muster, []byte(script), 0o700); err != nil { //nolint:gosec // the run directory's own script
		return "", err
	}
	cfgFile := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`stateDir: %[1]s/state
leaseDir: %[1]s/leases
resources: [%[2]s]
identity: {person: %[3]q, team: %[4]s, host: agentlab-%[5]s}
central: {context: %[6]s, muster: %[7]s}
lanes: [{name: %[8]s, installation: %[9]s, repositories: [%[10]s]}]
`, dir, centralLocal, email, beekeeperTeam, name, centralContext, muster, centralLane, centralEnvironment, centralRepo)
	return cfgFile, os.WriteFile(cfgFile, []byte(cfg), 0o600)
}

type centralProof struct {
	bin, ca                 string
	anaCfg, piaCfg, downCfg string
	anaS, piaS              *musterSession
	ana, pia                string
}

// beekeeper runs the local binary on a machine's configuration as agent and
// returns its output and exit code.
//
// An agent named session runs as a Claude Code session of that name (its
// own commands, agents register among them, refuse --as); any other as
// --as agent.
func (p *centralProof) beekeeper(cfgFile, agent string, args ...string) (string, int) {
	argv, env := append([]string{flagConfig, cfgFile, "--as", agent}, args...), []string{"CLAUDE_CODE_SESSION_ID="}
	if agent == centralSession {
		argv = append([]string{flagConfig, cfgFile}, args...)
		env = []string{"CLAUDE_CODE_SESSION_ID=" + centralSessionID, "CLAUDE_CODE_SESSION_NAME=" + centralSession}
	}
	cmd := exec.Command(p.bin, argv...) //nolint:gosec // the proof's beekeeper
	cmd.Env = append(append(withoutClaudeSession(os.Environ()), "SSL_CERT_FILE="+p.ca), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return string(out), e.ExitCode()
	}
	return string(out) + err.Error(), -1
}

// withoutClaudeSession drops the Claude Code session the proof may itself run
// in (CLAUDE_CODE_HOST_SESSION_ID, CLAUDE_PID, …): beekeeper would otherwise
// name the machine's agent after that session instead of the proof's own.
func withoutClaudeSession(environ []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE")
	})
}

// expect runs beekeeper and checks its exit code and that its output says
// each of want.
func (p *centralProof) expect(cfgFile, agent string, code int, args []string, want ...string) error {
	out, got := p.beekeeper(cfgFile, agent, args...)
	if got != code {
		return fmt.Errorf("beekeeper %s: exit %d, want %d: %s", strings.Join(args, " "), got, code, excerpt(out, 400))
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			return fmt.Errorf("beekeeper %s does not say %q: %s", strings.Join(args, " "), w, excerpt(out, 400))
		}
	}
	note("beekeeper %s: exit %d: %s", strings.Join(args, " "), got, excerpt(strings.TrimSpace(out), 160))
	return nil
}

// The beekeeper verbs the proof runs more than once.
const (
	verbLease         = "lease"
	verbRelease       = "release"
	verbHold          = "hold"
	verbClaim         = "claim"
	beekeeperVerbList = "list"
	verbNote          = "note"
	// flagConfig names a machine's configuration.
	flagConfig = "--config"
	// centralRollOption is the first option of the proof's decisions.
	centralRollOption = "Roll tonight: the lane clears at 22:00"
	// centralHoldReason is the lane hold's reason, which the other machine
	// is told.
	centralHoldReason = "proving window"
)

func (p *centralProof) leases() error {
	purpose := "e2e for agentlab-central-proof#1"
	for _, c := range []struct {
		cfg, agent string
		code       int
		args       []string
		want       []string
	}{
		{p.anaCfg, centralAnaAgent, 0, []string{verbLease, verbClaim, centralEnvironment, "-p", purpose}, []string{"claimed " + centralEnvironment}},
		{p.piaCfg, centralPiaAgent, exitRefused, []string{verbLease, verbClaim, centralEnvironment, "-p", "mine"}, []string{fmt.Sprintf("%s is held by %q", centralEnvironment, p.ana+"/"+centralAnaAgent), purpose}},
		{p.piaCfg, centralPiaAgent, 0, []string{verbLease, beekeeperVerbList}, []string{"central (muster context " + centralContext + ")", centralEnvironment, centralAnaAgent}},
		{p.piaCfg, centralPiaAgent, exitRefused, []string{verbLease, verbRelease, centralEnvironment}, []string{"not by you"}},
		{p.anaCfg, centralAnaAgent, 0, []string{verbLease, verbRelease, centralEnvironment}, []string{"released " + centralEnvironment}},
		{p.piaCfg, centralPiaAgent, 0, []string{verbLease, verbClaim, centralEnvironment, "-p", "mine"}, []string{"claimed " + centralEnvironment}},
		{p.piaCfg, centralPiaAgent, 0, []string{verbLease, verbRelease, centralEnvironment}, nil},
	} {
		if err := p.expect(c.cfg, c.agent, c.code, c.args, c.want...); err != nil {
			return err
		}
	}
	return nil
}

func (p *centralProof) holds() error {
	pr := centralRepo + "#2"
	for _, c := range []struct {
		cfg, agent string
		code       int
		args       []string
		want       []string
	}{
		{p.anaCfg, centralAnaAgent, 0, []string{verbHold, "set", "--lane", centralLane, "-r", centralHoldReason}, []string{"lane:" + centralLane}},
		{p.piaCfg, centralPiaAgent, exitRefused, []string{verbHold, "check", pr}, []string{centralHoldReason}},
		{p.piaCfg, centralPiaAgent, 0, []string{verbHold, beekeeperVerbList}, []string{"central (muster context " + centralContext + ")", centralHoldReason}},
		{p.anaCfg, centralAnaAgent, 0, []string{verbHold, "lift", "--lane", centralLane}, nil},
		{p.piaCfg, centralPiaAgent, 0, []string{verbHold, "check", pr}, nil},
	} {
		if err := p.expect(c.cfg, c.agent, c.code, c.args, c.want...); err != nil {
			return err
		}
	}
	return nil
}

// laneCall calls a lane tool through muster as one person's agent and
// returns its structured result.
func laneCall(s *musterSession, tool, agent string, pr int, extra map[string]any) (map[string]any, error) {
	args := map[string]any{"repo": centralRepo, "pr": pr, beekeeperKeyHost: centralContext}
	args[beekeeperKeyAgent] = agent
	for k, v := range extra {
		args[k] = v
	}
	env, err := s.callToolEnvelope("x_"+beekeeperServer+"_"+tool, args)
	if err != nil {
		return nil, err
	}
	if env.IsError {
		return nil, fmt.Errorf("%s #%d: %s", tool, pr, excerpt(env.Content[0].Text, 300))
	}
	return env.StructuredContent, nil
}

func (p *centralProof) lanes() error {
	turn := func(s *musterSession, tool, agent string, pr int, want bool) error {
		t, err := laneCall(s, tool, agent, pr, nil)
		if err != nil {
			return err
		}
		if got, _ := t["turn"].(bool); got != want {
			return fmt.Errorf("%s #%d: turn %v, want %v (%v)", tool, pr, got, want, t)
		}
		note("%s #%d: turn %v, ahead %v", tool, pr, want, t["ahead"])
		return nil
	}
	if err := turn(p.anaS, "lane_queue", centralAnaAgent, 1, true); err != nil {
		return err
	}
	if err := turn(p.piaS, "lane_queue", centralPiaAgent, 2, false); err != nil {
		return err
	}
	if _, err := laneCall(p.anaS, "lane_settle", centralAnaAgent, 1, nil); err != nil {
		return err
	}
	if _, err := laneCall(p.anaS, "lane_settle", centralAnaAgent, 1, map[string]any{"merged": true}); err != nil {
		return err
	}
	if err := turn(p.piaS, "lane_turn", centralPiaAgent, 2, false); err != nil {
		return err
	}
	if _, err := laneCall(p.piaS, "lane_leave", centralPiaAgent, 1, nil); err == nil {
		return fmt.Errorf("%s took %s's merge out of the lane", p.pia, p.ana)
	}
	if _, err := laneCall(p.anaS, "lane_leave", centralAnaAgent, 1, map[string]any{"reason": "rolled"}); err != nil {
		return err
	}
	if err := turn(p.piaS, "lane_turn", centralPiaAgent, 2, true); err != nil {
		return err
	}
	_, err := laneCall(p.piaS, "lane_leave", centralPiaAgent, 2, nil)
	return err
}

func (p *centralProof) roster() error {
	if err := p.expect(p.anaCfg, centralSession, 0, []string{"agents", "register"}); err != nil {
		return err
	}
	if out, code := p.beekeeper(p.anaCfg, centralSession, "watch", "--once"); code != 0 && code != exitRefused {
		return fmt.Errorf("watch --once: exit %d: %s", code, excerpt(out, 400))
	}
	text, err := p.anaS.callServerTool("x_"+beekeeperServer+"_list_agents", map[string]any{})
	if err != nil {
		return err
	}
	if !strings.Contains(text, "local:agentlab-ana/"+centralSession) {
		return fmt.Errorf("the central roster lacks local:agentlab-ana/%s: %s", centralSession, excerpt(text, 400))
	}
	note("list_agents: local:agentlab-ana/%s", centralSession)
	return nil
}

func (p *centralProof) unreachable() error {
	if err := p.expect(p.downCfg, centralPiaAgent, exitCentral, []string{verbLease, verbClaim, centralEnvironment, "-p", "x"}, "is unreachable"); err != nil {
		return err
	}
	if err := p.expect(p.downCfg, centralPiaAgent, 0, []string{verbLease, verbClaim, centralLocal, "-p", "local work"}, "claimed "+centralLocal); err != nil {
		return err
	}
	return p.expect(p.downCfg, centralPiaAgent, 0, []string{verbLease, verbRelease, centralLocal})
}

// centralQuestion is the note the proof files for the team.
const centralQuestion = "Central proof: may the shared lane roll tonight?"

// The parameters every serve tool takes to name the calling agent.
const (
	beekeeperKeyAgent = "agent"
	beekeeperKeyHost  = "host"
	beekeeperKeyDue   = "due"
)

func (p *centralProof) notes() error {
	text, err := p.anaS.callServerTool(beekeeperNoteAdd, map[string]any{
		slackKeyText: centralQuestion, "for": "team:" + beekeeperTeam, beekeeperKeyAgent: centralAnaAgent, beekeeperKeyHost: centralContext,
		"status_quo":    "The shared lane rolls one release a night.",
		"why":           "Only the team picks what rolls on the shared installation.",
		slackKeyOptions: []any{centralRollOption, "Wait: nothing rolls before Monday"},
		beekeeperKeyDue: "3h", beekeeperKeyDefault: "nothing rolls tonight",
		"refs": []any{centralRepo + "#1"},
	})
	if err != nil {
		return err
	}
	var id int
	if _, err := fmt.Sscanf(text, "note #%d", &id); err != nil {
		return fmt.Errorf("%s answered %q, not a note number", beekeeperNoteAdd, excerpt(text, 200))
	}
	note("note #%d filed for team:%s", id, beekeeperTeam)
	listed, err := p.piaS.callServerTool(beekeeperNoteList, map[string]any{beekeeperKeyAgent: centralPiaAgent, beekeeperKeyHost: centralContext})
	if err != nil {
		return err
	}
	if !strings.Contains(listed, centralQuestion) {
		return fmt.Errorf("%s's note_list lacks note #%d: %s", p.pia, id, excerpt(listed, 400))
	}
	note("%s lists note #%d", p.pia, id)
	if _, err := p.piaS.callServerTool("x_"+beekeeperServer+"_note_answer", map[string]any{
		beekeeperKeyNote: id, "choice": 2, slackKeyText: "wait for Monday", beekeeperKeyAgent: centralPiaAgent, beekeeperKeyHost: centralContext,
	}); err != nil {
		return err
	}
	listed, err = p.anaS.callServerTool(beekeeperNoteList, map[string]any{beekeeperKeyAgent: centralAnaAgent, beekeeperKeyHost: centralContext})
	if err != nil {
		return err
	}
	if strings.Contains(listed, centralQuestion) {
		return fmt.Errorf("note #%d is still open after %s's answer: %s", id, p.pia, excerpt(listed, 400))
	}
	note("%s answered note #%d; it is closed for %s", p.pia, id, p.ana)
	return nil
}
