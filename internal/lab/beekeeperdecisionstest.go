package lab

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The beekeeper decisions proof runs beekeeper serve, the central instance,
// on the host over the lab cluster's beekeeper resources, registered in the
// lab's muster as an MCP server that gets the person's forwarded token, and
// klaus-gateway with the fake Slack as decisions-test runs them. A decision
// filed through muster as one lab user is put to its addressee in Slack;
// the answers — a Choose click, a thread reply — come back through muster
// as the person who answered, beekeeper checks them against the addressee
// and closes the message; a decision nobody answers closes with its default
// at its due time, and a withdrawn one loses its buttons.

// BeekeeperDecisionsVersionDefault is the first beekeeper release that puts
// decisions to people through klaus-gateway.
const BeekeeperDecisionsVersionDefault = "v0.77.0"

// The proof's names: the MCPServer muster aggregates beekeeper under (its
// tools are x_beekeeper_*), the team the lab users decide as, and the due
// time of the decision left to its default.
const (
	beekeeperServer        = "beekeeper"
	beekeeperTeam          = "agentlab"
	beekeeperNamespace     = "beekeeper-" + beekeeperTeam
	beekeeperPostgresImage = "gsoci.azurecr.io/giantswarm/postgres:18.6-alpine"
	beekeeperTokenFile     = "klaus-gateway-token"
	beekeeperDefaultDue    = "1m"
	// beekeeperDefaultWait covers the due time and serve's default loop,
	// which looks every 30 seconds.
	beekeeperDefaultWait = 3 * time.Minute
	beekeeperDefault     = "the lab keeps its lane"
	beekeeperAnswerTool  = "x_beekeeper_note_answer"
	beekeeperNoteAdd     = "x_beekeeper_note_add"
	beekeeperNoteDone    = "x_beekeeper_note_done"
	beekeeperNoteList    = "x_beekeeper_note_list"
)

// beekeeperCRDs are the CustomResourceDefinitions serve's store needs, as
// the release tags them under config/crd.
var beekeeperCRDs = []string{"environments", "holds", "mergelanes", "notes", "rosterentries"}

// BeekeeperDecisionsTestOptions tunes the beekeeper decisions proof.
type BeekeeperDecisionsTestOptions struct {
	// Version is the beekeeper release whose binary and CRDs run (default
	// BeekeeperDecisionsVersionDefault); Binary, a local linux build, takes
	// precedence for the binary — the proof of a branch.
	Version string
	Binary  string
	// The gateway and the fake Slack as decisions-test runs them: Port is
	// the gateway's, Port+1 its admin endpoints, Port+2 the fake Slack Web
	// API, Port+3 beekeeper serve, Port+4 its Postgres. Default 18090.
	DecisionsTestOptions
}

// BeekeeperDecisionsTest is the headless proof of beekeeper's decisions.
func BeekeeperDecisionsTest(cfg *config.Config, opts BeekeeperDecisionsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("the platform is off in %s — the answers run through its muster; enable it and run `agentlab platform` first", config.File)
	}
	asker, other, group, err := beekeeperUsers(cfg)
	if err != nil {
		return err
	}
	if opts.Version == "" {
		opts.Version = BeekeeperDecisionsVersionDefault
	}
	if opts.GatewayImage == "" {
		opts.GatewayImage = DecisionsGatewayImageDefault
	}
	if opts.Port == 0 {
		opts.Port = 18090
	}
	servePort, pgPort := opts.Port+3, opts.Port+4
	if err := portsFree(opts.Port, opts.Port+1, opts.Port+2, servePort, pgPort); err != nil {
		return err
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	tokens := map[string]string{}
	ids := map[string]linkedIdentity{}
	for _, u := range []*config.User{asker, other} {
		step("Logging in to Dex as %s", u.Email)
		tok, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		id, err := tokenIdentity(tok, time.Now())
		if err != nil {
			return fmt.Errorf("the id_token of %s: %w", u.Email, err)
		}
		tokens[u.Email], ids[u.Email] = tok, id
	}

	run := strings.ToUpper(randomSuffix())
	askerSlack, otherSlack := slackUserPrefix+run+"A", slackUserPrefix+run+"B"
	channel := "CAGENTLAB" + run
	fake, err := startFakeSlack(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+2)), map[string]string{askerSlack: asker.Email, otherSlack: other.Email})
	if err != nil {
		return err
	}
	defer fake.close()
	keys, err := writeGatewayFiles(runDir)
	if err != nil {
		return err
	}
	for slackUser, u := range map[string]*config.User{askerSlack: asker, otherSlack: other} {
		step("Linking Slack user %s to %s in the gateway's link store", slackUser, u.Email)
		if err := seedBoltLink(filepath.Join(runDir, gatewayLinksFile), keys.store, slackUser, ids[u.Email].link(u.Email, tokens[u.Email])); err != nil {
			return err
		}
	}

	caller := "system:serviceaccount:" + platformNamespace + ":" + decisionsServiceAccount
	step("Creating ServiceAccount %s and a token for it with audience %s: beekeeper's identity toward the gateway", caller, decisionsAudience)
	saToken, removeSA, err := decisionsCallerToken()
	if err != nil {
		return err
	}
	defer removeSA()
	tokenPath := filepath.Join(runDir, beekeeperTokenFile)
	if err := os.WriteFile(tokenPath, []byte(saToken), 0o600); err != nil {
		return err
	}

	kubeconfig, err := filepath.Abs(labKubeconfigPath)
	if err != nil {
		return err
	}
	caFile, err := filepath.Abs(trustBundleFile(cfg))
	if err != nil {
		return err
	}
	edge, err := edgeHostPort(cfg.AgentgatewayBaseURL())
	if err != nil {
		return err
	}
	gw := newGatewayProcess(KlausGatewayTestOptions{GatewayImage: opts.GatewayImage, GatewayBinary: opts.GatewayBinary, Port: opts.Port},
		runDir, caFile, "grpcs://"+edge, fake.baseURL(), cfg.MusterBaseURL())
	gw.extraArgs = []string{"--reviews-enabled=true", "--reviews-allowed-callers=" + caller}
	gw.kubeconfig = kubeconfig
	gw.trustLabCA = true
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s on the host, Slack on the fake at %s", gw.describe(), fake.baseURL())
	if err := gw.start(); err != nil {
		return err
	}
	note("ready: %s", gw.version())

	step("Applying beekeeper %s's CRDs and the team namespace %s", opts.Version, beekeeperNamespace)
	if err := applyBeekeeperCRDs(opts.Version); err != nil {
		return err
	}
	removeNS, err := ensureBeekeeperNamespace()
	if err != nil {
		return err
	}
	defer removeNS()

	step("Starting Postgres for serve's mailboxes on 127.0.0.1:%d (%s)", pgPort, beekeeperPostgresImage)
	stopPG, err := startBeekeeperPostgres(run, pgPort)
	if err != nil {
		return err
	}
	defer stopPG()

	bin := opts.Binary
	if bin == "" {
		if bin, err = downloadBeekeeper(opts.Version, runDir); err != nil {
			return err
		}
	}
	serveCfg := beekeeperServeConfig(cfg, group, map[string]string{"asker": asker.Email, "other": other.Email}, channel, gw.baseURL(), tokenPath)
	step("Starting beekeeper serve (%s) on :%d: issuer %s, team %s of group %s, decisions through %s", bin, servePort, cfg.Issuer(), beekeeperTeam, group, gw.baseURL())
	stopServe, err := startBeekeeperServe(bin, runDir, serveCfg, kubeconfig, caFile, servePort, pgPort)
	if err != nil {
		return err
	}
	defer stopServe()
	gateway, err := kindGatewayIP(cfg.ControlPlaneNode())
	if err != nil {
		return err
	}
	host, err := podReachableHost(cfg.ControlPlaneNode(), gateway, servePort)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("pods reach beekeeper serve on :%d neither on the kind gateway %s nor on %s: the host's firewall drops it", servePort, gateway, hostAlias())
	}

	url := "http://" + net.JoinHostPort(host, strconv.Itoa(servePort)) + "/mcp"
	step("Registering MCPServer %s at %s in muster, the person's token forwarded", beekeeperServer, url)
	removeServer, err := registerBeekeeperServer(url)
	if err != nil {
		return err
	}
	defer removeServer()

	askerS, err := openMusterSession(cfg, tokens[asker.Email], "agentlab-beekeeper-decisions")
	if err != nil {
		return err
	}
	if err := waitBeekeeperTools(askerS); err != nil {
		return err
	}
	p := &beekeeperProof{s: askerS, fake: fake, driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
		asker: asker.Email, askerSlack: askerSlack, otherSlack: otherSlack, channel: channel}

	step("1. A decision for %s, filed through muster: a direct message; %s's click is refused, %s's answers it", asker.Email, other.Email, asker.Email)
	if err := p.personByClick(); err != nil {
		return err
	}
	step("2. A decision for team:%s in %s, answered by %s's reply in its thread", beekeeperTeam, channel, other.Email)
	if err := p.teamByThreadReply(); err != nil {
		return err
	}
	step("3. A decision nobody answers closes with its default at its due time (%s); a late click is refused", beekeeperDefaultDue)
	if err := p.defaulted(); err != nil {
		return err
	}
	step("4. A decision its filer withdraws (note_done) loses its buttons")
	if err := p.withdrawn(); err != nil {
		return err
	}
	step("5. beekeeper recorded each outcome as an Event in %s", beekeeperNamespace)
	if err := beekeeperOutcomes(); err != nil {
		return err
	}
	note("all beekeeper decisions proven")
	return nil
}

// beekeeperUsers are the asker, an addressee of the team who is not the
// asker, and the Dex group both carry: the organization and the team.
func beekeeperUsers(cfg *config.Config) (asker, other *config.User, group string, err error) {
	asker = cfg.AdminUser()
	if asker == nil {
		return nil, nil, "", fmt.Errorf("no admin user in %s", config.File)
	}
	for i := range cfg.Users {
		u := &cfg.Users[i]
		if u.Email == asker.Email {
			continue
		}
		for _, g := range u.Groups {
			if asker.HasGroup(g) {
				return asker, u, g, nil
			}
		}
	}
	return nil, nil, "", fmt.Errorf("no user in %s shares a group with %s", config.File, asker.Email)
}

// beekeeperServeConfig is serve's configuration: the lab Dex, the shared
// group as the organization and the team, the people by name, the team's
// channel and the gateway.
func beekeeperServeConfig(cfg *config.Config, group string, people map[string]string, channel, gateway, tokenFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "serve:\n  issuer: %q\n  clientIDs: [%q, %q]\n  organization: %q\n  teams: {%q: %q}\n",
		cfg.Issuer(), config.AgentPlatformClientID, config.KubernetesClientID, group, group, beekeeperTeam)
	b.WriteString("  people:\n")
	for name, email := range people {
		fmt.Fprintf(&b, "    %s: %q\n", name, email)
	}
	fmt.Fprintf(&b, "  channels: {%q: %q}\n  gateway:\n    url: %q\n    tokenFile: %q\n    answerTool: %q\n",
		beekeeperTeam, channel, gateway, tokenFile, beekeeperAnswerTool)
	return b.String()
}

// applyBeekeeperCRDs applies the release's CRDs from its tag.
func applyBeekeeperCRDs(version string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	for _, name := range beekeeperCRDs {
		u := fmt.Sprintf("https://raw.githubusercontent.com/giantswarm/beekeeper/%s/config/crd/beekeeper.giantswarm.io_%s.yaml", version, name)
		resp, err := client.Get(u) //nolint:gosec // a fixed GitHub URL of the release
		if err != nil {
			return fmt.Errorf("fetching %s: %w", u, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fetching %s: HTTP %d %v", u, resp.StatusCode, err)
		}
		if _, err := applyManifests(context.Background(), body); err != nil {
			return err
		}
	}
	return nil
}

// ensureBeekeeperNamespace creates the team namespace serve keeps the
// team's notes in; the returned func deletes it.
func ensureBeekeeperNamespace() (func(), error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: beekeeperNamespace, Labels: map[string]string{"app.kubernetes.io/managed-by": managedByAgentlabValue}}}
	if _, err := k.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, fmt.Errorf("creating namespace %s: %w", beekeeperNamespace, err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k.clientset.CoreV1().Namespaces().Delete(ctx, beekeeperNamespace, metav1.DeleteOptions{})
	}, nil
}

// startBeekeeperPostgres runs the mailboxes' Postgres in a container of its
// own; the returned func removes it.
func startBeekeeperPostgres(run string, port int) (func(), error) {
	name := "agentlab-beekeeper-pg-" + strings.ToLower(run)
	if err := runQuiet("docker", "run", "-d", "--rm", "--name", name, "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port), beekeeperPostgresImage); err != nil {
		return nil, fmt.Errorf("starting Postgres: %w", err)
	}
	stop := func() { _ = runQuiet("docker", "rm", "-f", name) }
	if !waitFor(60, time.Second, func() bool { return runQuiet("docker", "exec", name, "pg_isready", "-U", "postgres") == nil }) {
		stop()
		return nil, fmt.Errorf("the Postgres container %s was not ready within a minute", name)
	}
	return stop, nil
}

// downloadBeekeeper fetches the release's linux binary into dir.
func downloadBeekeeper(version, dir string) (string, error) {
	u := fmt.Sprintf("https://github.com/giantswarm/beekeeper/releases/download/%s/beekeeper-linux-amd64", version)
	step("Downloading beekeeper %s", version)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(u) //nolint:gosec // a fixed GitHub URL of the release
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", u, resp.StatusCode)
	}
	path := filepath.Join(dir, "beekeeper")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700) //nolint:gosec // the run directory
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

// startBeekeeperServe runs serve with its configuration over the lab's
// kubeconfig, the lab CA trusted for the Dex issuer, and waits for its
// /healthz; the returned func stops it. Its log is beekeeper.log in runDir.
func startBeekeeperServe(bin, runDir, serveCfg, kubeconfig, caFile string, port, pgPort int) (func(), error) {
	cfgPath := filepath.Join(runDir, "beekeeper.yaml")
	if err := os.WriteFile(cfgPath, []byte(serveCfg), 0o600); err != nil {
		return nil, err
	}
	logFile, err := os.Create(filepath.Join(runDir, "beekeeper.log")) //nolint:gosec // the run directory
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "serve", fmt.Sprintf("--http=:%d", port)) //nolint:gosec // the release binary or the operator's build
	cmd.Env = append(os.Environ(),
		"BEEKEEPER_CONFIG="+cfgPath,
		"BEEKEEPER_DATABASE_URL="+fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", pgPort),
		"KUBECONFIG="+kubeconfig,
		"SSL_CERT_FILE="+caFile,
		"XDG_STATE_HOME="+runDir,
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	}
	health := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	if !waitFor(60, 500*time.Millisecond, func() bool {
		resp, err := http.Get(health) //nolint:gosec,noctx // the local serve
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		stop()
		log, _ := os.ReadFile(filepath.Join(runDir, "beekeeper.log")) //nolint:gosec // the run directory
		return nil, fmt.Errorf("beekeeper serve did not answer %s within 30s: %s", health, excerptEnds(string(log), 600))
	}
	return stop, nil
}

// registerBeekeeperServer registers serve in muster with the person's token
// forwarded; the returned func removes the MCPServer.
func registerBeekeeperServer(url string) (func(), error) {
	manifest := fmt.Sprintf(`apiVersion: muster.giantswarm.io/v1alpha1
kind: MCPServer
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: %s
spec:
  description: beekeeper serve of agentlab beekeeper-decisions-test
  type: streamable-http
  url: %s
  autoStart: true
  auth:
    type: oauth
    forwardToken: true
`, beekeeperServer, platformNamespace, managedByAgentlabValue, url)
	if _, err := applyManifests(context.Background(), []byte(manifest)); err != nil {
		return nil, err
	}
	remove := func() {
		gvr, err := gvrFor(musterMCPServerResource)
		if err == nil {
			_ = deleteObject(context.Background(), gvr, platformNamespace, beekeeperServer, 30*time.Second)
		}
	}
	return remove, nil
}

// waitBeekeeperTools waits until muster serves beekeeper's tools to the
// session.
func waitBeekeeperTools(s *musterSession) error {
	var last string
	if !waitFor(40, 3*time.Second, func() bool {
		text, err := s.callServerTool(beekeeperNoteList, map[string]any{})
		if err != nil {
			last = err.Error()
			return false
		}
		last = text
		return true
	}) {
		return fmt.Errorf("muster serves no %s within 2 minutes: %s", beekeeperNoteList, excerpt(last, 300))
	}
	note("muster serves %s", beekeeperNoteList)
	return nil
}

type beekeeperProof struct {
	s                                      *musterSession
	fake                                   *fakeSlack
	driver                                 *slackDriver
	asker, askerSlack, otherSlack, channel string
}

// add files a decision through muster as the asker and returns its number.
func (p *beekeeperProof) add(forWho, question, due string) (int, error) {
	text, err := p.s.callServerTool(beekeeperNoteAdd, map[string]any{
		"text": question, "for": forWho, "kind": "decision", "agent": "beekeeper-decisions-test", "host": "agentlab",
		"status_quo": "The lab's merge lane takes one release a night.",
		"why":        "Only the lab's owners pick what rolls.",
		"options":    []any{"Roll tonight: the lane clears at 22:00", "Wait for Monday: nothing rolls before Monday"},
		"recommend":  2, "due": due, "default": beekeeperDefault,
	})
	if err != nil {
		return 0, err
	}
	var id int
	if _, err := fmt.Sscanf(text, "note #%d", &id); err != nil {
		return 0, fmt.Errorf("%s answered %q, not a note number", beekeeperNoteAdd, excerpt(text, 200))
	}
	note("note #%d filed for %s", id, forWho)
	return id, nil
}

// message waits for the decision of note id in channel to satisfy pred.
func (p *beekeeperProof) message(channel string, id int, what string, wait time.Duration, pred func(slackMessage) bool) (slackMessage, error) {
	tag := fmt.Sprintf("note #%d", id)
	var last slackMessage
	if !waitFor(int(wait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		for _, m := range p.fake.thread(channel, "") {
			if strings.Contains(m.shown(), tag) {
				last = m
				return pred(m)
			}
		}
		return false
	}) {
		return last, fmt.Errorf("%s: %s did not happen within %s; the message reads: %s", tag, what, wait, excerpt(last.shown(), 400))
	}
	return last, nil
}

func (p *beekeeperProof) answeredBy(channel string, id int, slackUser string, texts ...string) error {
	_, err := p.message(channel, id, "the answer's rewrite", decisionsWait, func(m slackMessage) bool {
		shown := m.shown()
		if _, open := m.action(decisionOwnWords); open || !strings.Contains(shown, decisionAnsweredBy+slackUser+">") {
			return false
		}
		for _, t := range texts {
			if !strings.Contains(shown, t) {
				return false
			}
		}
		return true
	})
	if err == nil {
		note("note #%d: answered by %s", id, slackUser)
	}
	return err
}

func (p *beekeeperProof) personByClick() error {
	id, err := p.add("asker", "Roll the release onto the lab tonight?", "2h")
	if err != nil {
		return err
	}
	dm := directChannel(p.askerSlack)
	m, err := p.message(dm, id, "the direct message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	if err := assertDecisionMessage(m); err != nil {
		return err
	}
	if err := p.driver.click(p.otherSlack, m, decisionChoose); err != nil {
		return err
	}
	if _, err := p.message(dm, id, "the refusal of a click by someone else", decisionsWait, func(m slackMessage) bool {
		_, open := m.action(decisionOwnWords)
		return open && strings.Contains(m.shown(), "<@"+p.otherSlack+">")
	}); err != nil {
		return err
	}
	note("note #%d: %s's click refused, the decision open", id, p.otherSlack)
	if err := p.driver.click(p.askerSlack, m, decisionChoose); err != nil {
		return err
	}
	return p.answeredBy(dm, id, p.askerSlack, "Roll tonight")
}

func (p *beekeeperProof) teamByThreadReply() error {
	id, err := p.add("team:"+beekeeperTeam, "Roll the release onto the lab on Monday instead?", "2h")
	if err != nil {
		return err
	}
	m, err := p.message(p.channel, id, "the team's message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	ts := p.fake.nextTS()
	if err := p.driver.event(map[string]any{
		fieldTypeKey: slackKeyMessage, slackKeyUser: p.otherSlack, slackKeyChannel: p.channel, slackKeyChanType: slackKeyChannel,
		slackKeyText: decisionThreadReply, slackKeyTS: ts, slackKeyEventTS: ts, slackKeyThreadTS: m.TS, "parent_user_id": slackFakeBotUser,
	}); err != nil {
		return err
	}
	return p.answeredBy(p.channel, id, p.otherSlack, decisionThreadReply)
}

func (p *beekeeperProof) defaulted() error {
	id, err := p.add("team:"+beekeeperTeam, "Nobody answers this one?", beekeeperDefaultDue)
	if err != nil {
		return err
	}
	open, err := p.message(p.channel, id, "the team's message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	if _, err := p.message(p.channel, id, "the default at the due time", beekeeperDefaultWait, func(m slackMessage) bool {
		_, buttons := m.action(decisionOwnWords)
		return !buttons && strings.Contains(m.shown(), decisionDefaulted) && strings.Contains(m.shown(), beekeeperDefault)
	}); err != nil {
		return err
	}
	if err := p.driver.click(p.otherSlack, open, decisionChoose); err != nil {
		return err
	}
	if !waitFor(int(decisionsWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		for _, m := range p.fake.thread(p.channel, "") {
			if m.Method == slackPostEphemeral && m.Recipient == p.otherSlack && strings.Contains(m.Text, decisionLateRefusal) {
				return true
			}
		}
		return false
	}) {
		return fmt.Errorf("a click on the defaulted note #%d was not told it was not answered in time", id)
	}
	note("note #%d: defaulted (%s), a late click refused", id, beekeeperDefault)
	return nil
}

func (p *beekeeperProof) withdrawn() error {
	id, err := p.add("team:"+beekeeperTeam, "Withdraw this one again?", "2h")
	if err != nil {
		return err
	}
	if _, err := p.message(p.channel, id, "the team's message", decisionsWait, func(slackMessage) bool { return true }); err != nil {
		return err
	}
	if _, err := p.s.callServerTool(beekeeperNoteDone, map[string]any{"note": id}); err != nil {
		return err
	}
	if _, err := p.message(p.channel, id, "the withdrawn rewrite", decisionsWait, func(m slackMessage) bool {
		_, buttons := m.action(decisionOwnWords)
		return !buttons
	}); err != nil {
		return err
	}
	note("note #%d: withdrawn, its buttons gone", id)
	return nil
}

// beekeeperOutcomes wants the Events beekeeper recorded: two answers from
// Slack, one default.
func beekeeperOutcomes() error {
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evs, err := k.clientset.CoreV1().Events(beekeeperNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=beekeeper"})
	if err != nil {
		return err
	}
	var answered, defaulted int
	for _, e := range evs.Items {
		switch {
		case e.Reason == "note.answered" && strings.Contains(e.Message, "via slack"):
			answered++
		case e.Reason == "note.defaulted" && strings.Contains(e.Message, beekeeperDefault):
			defaulted++
		}
	}
	if answered != 2 || defaulted != 1 {
		return fmt.Errorf("%s holds %d note.answered via slack and %d note.defaulted Events, want 2 and 1", beekeeperNamespace, answered, defaulted)
	}
	note("2 note.answered via slack, 1 note.defaulted")
	return nil
}
