package lab

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
// at its due time, and a withdrawn one loses its buttons. The person's guide
// converses with them: its message opens a direct message, their reply in
// its thread reaches the guide's mailbox through muster as them, and the
// guide's answer lands in the same thread; another person cannot reach the
// guide, and with the guide off the roster the thread says the reply was
// not delivered.

// BeekeeperDecisionsVersionDefault is the first beekeeper release that puts
// decisions to people through klaus-gateway and lets their guide converse
// with them there.
const BeekeeperDecisionsVersionDefault = "v0.79.0-rc.1"

// The proof's names: the MCPServer muster aggregates beekeeper under (its
// tools are x_beekeeper_*), the team the lab users decide as, and the due
// time of the decision left to its default.
const (
	beekeeperServer        = "beekeeper"
	beekeeperTeam          = "agentlab"
	beekeeperNamespace     = "beekeeper-" + beekeeperTeam
	beekeeperPostgresImage = "gsoci.azurecr.io/giantswarm/postgres:18.6-alpine"
	beekeeperTokenFile     = "klaus-gateway-token"
	beekeeperImage         = "gsoci.azurecr.io/giantswarm/beekeeper"
	// beekeeperMount is where the run directory is mounted into serve's
	// container, beside its configuration, kubeconfig and the lab CA.
	beekeeperMount      = "/run/agentlab"
	beekeeperConfigFile = "beekeeper.yaml"
	beekeeperKubeconfig = "kubeconfig"
	beekeeperCAFile     = "ca.crt"
	// The note tools' argument names the proof sets.
	beekeeperKeyKind    = "kind"
	beekeeperKeyDefault = "default"
	beekeeperKeyNote    = "note"
	// musterNodePort is muster's plain listener in the node's network.
	musterNodePort      = 8090
	beekeeperDefaultDue = "1m"
	// beekeeperDefaultWait covers the due time and serve's default loop,
	// which looks every 30 seconds.
	beekeeperDefaultWait = 3 * time.Minute
	beekeeperDefault     = "the lab keeps its lane"
	beekeeperAnswerTool  = "x_beekeeper_note_answer"
	beekeeperNoteAdd     = "x_beekeeper_note_add"
	beekeeperNoteDone    = "x_beekeeper_note_done"
	beekeeperNoteList    = "x_beekeeper_note_list"
	beekeeperRegister    = "x_beekeeper_agents_register"
	beekeeperConverse    = "x_beekeeper_converse"
	beekeeperSend        = "x_beekeeper_send_message"
	beekeeperReceive     = "x_beekeeper_receive_messages"
	beekeeperAck         = "x_beekeeper_ack_messages"
	beekeeperSendTool    = beekeeperSend
)

// beekeeperCRDs are the CustomResourceDefinitions serve's store needs, as
// the release tags them under config/crd.
var beekeeperCRDs = []string{"environments", "holds", "mergelanes", "notes", "rosterentries"}

// BeekeeperDecisionsTestOptions tunes the beekeeper decisions proof.
type BeekeeperDecisionsTestOptions struct {
	// Version is the beekeeper release whose image and CRDs run (default
	// BeekeeperDecisionsVersionDefault); Binary, a local linux build mounted
	// into that image, takes precedence — the proof of a branch.
	Version string
	Binary  string
	// The gateway: Port is its endpoints' in the node's network, Port+1
	// its admin endpoints, Port+3 beekeeper serve's, Port+4 its Postgres'.
	// Default 18090. GatewayBinary is not used: the gateway runs in the
	// node's network, as a container.
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

	node := cfg.ControlPlaneNode()
	nodeIP, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, node)
	if err != nil || strings.TrimSpace(nodeIP) == "" {
		return fmt.Errorf("the address of node %s on network %s: %v", node, kindDockerNetwork, err)
	}
	nodeIP = strings.TrimSpace(nodeIP)
	// Everything beekeeper talks to and everything that talks to it runs in
	// the node's network namespace, where muster listens (hostNetwork):
	// the host's firewall may drop what pods send to the host.
	inNode := "container:" + node

	run := strings.ToUpper(randomSuffix())
	askerSlack, otherSlack := slackUserPrefix+run+"A", slackUserPrefix+run+"B"
	channel := "CAGENTLAB" + run
	self, err := os.Executable()
	if err != nil {
		return err
	}
	step("Starting the fake Slack Web API in a container on the kind network")
	fake, err := startSlackFakeContainer(cfg, self, map[string]string{askerSlack: asker.Email, otherSlack: other.Email})
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
	if err := os.WriteFile(filepath.Join(runDir, beekeeperTokenFile), []byte(saToken), 0o600); err != nil {
		return err
	}
	kubeconfig, err := nodeKubeconfig(runDir)
	if err != nil {
		return err
	}
	caFile, err := filepath.Abs(trustBundleFile(cfg))
	if err != nil {
		return err
	}
	if err := copyFile(caFile, filepath.Join(runDir, beekeeperCAFile)); err != nil {
		return err
	}

	musterURL := "http://127.0.0.1:" + strconv.Itoa(musterNodePort)
	slackAPI := "http://" + net.JoinHostPort(fake.podIP, strconv.Itoa(fakeContainerPort)) + slackAPIPath
	gw := newGatewayProcess(KlausGatewayTestOptions{GatewayImage: opts.GatewayImage, Port: opts.Port}, runDir, caFile, "grpcs://127.0.0.1:1", slackAPI, musterURL)
	gw.extraArgs = []string{"--reviews-enabled=true", "--reviews-allowed-callers=" + caller,
		"--listen-address=" + net.JoinHostPort("0.0.0.0", strconv.Itoa(opts.Port)),
		"--admin-address=" + net.JoinHostPort("0.0.0.0", strconv.Itoa(opts.Port+1))}
	gw.kubeconfig = kubeconfig
	gw.network, gw.host = inNode, nodeIP
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s in node %s's network, Slack on the fake at %s, muster at %s", gw.describe(), node, slackAPI, musterURL)
	if err := gw.start(); err != nil {
		return err
	}
	note("ready: %s", gw.version())

	step("Applying beekeeper %s's CRDs and the team namespace %s", opts.Version, beekeeperNamespace)
	removeCRDs, err := applyBeekeeperCRDs(opts.Version)
	if err != nil {
		return err
	}
	defer removeCRDs()
	removeNS, err := ensureBeekeeperNamespace()
	if err != nil {
		return err
	}
	defer removeNS()

	step("Starting Postgres for serve's mailboxes in node %s's network on :%d (%s)", node, pgPort, beekeeperPostgresImage)
	stopPG, err := startBeekeeperPostgres(run, inNode, pgPort)
	if err != nil {
		return err
	}
	defer stopPG()

	serveCfg := beekeeperServeConfig(cfg, group, map[string]string{"asker": asker.Email, "other": other.Email}, channel,
		"http://127.0.0.1:"+strconv.Itoa(opts.Port), beekeeperMount+"/"+beekeeperTokenFile)
	image := beekeeperImage + ":" + strings.TrimPrefix(opts.Version, "v")
	step("Starting beekeeper serve (%s) in node %s's network on :%d: issuer %s, team %s of group %s", cmp.Or(opts.Binary, image), node, servePort, cfg.Issuer(), beekeeperTeam, group)
	stopServe, err := startBeekeeperServe(image, opts.Binary, run, inNode, runDir, serveCfg, nodeIP, servePort, pgPort)
	if err != nil {
		return err
	}
	defer stopServe()

	url := "http://127.0.0.1:" + strconv.Itoa(servePort) + "/mcp"
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
	otherS, err := openMusterSession(cfg, tokens[other.Email], "agentlab-beekeeper-decisions-other")
	if err != nil {
		return err
	}
	p := &beekeeperProof{s: askerS, otherS: otherS, fake: fake, driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
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
	step("6. The guide in Slack: %s's guide writes to them, their reply in the thread reaches the guide's mailbox from Slack, the guide's answer lands in the same thread", asker.Email)
	r, err := p.guideConversation()
	if err != nil {
		return err
	}
	step("7. Another person's message to %s's guide is refused", asker.Email)
	if err := p.otherRefused(); err != nil {
		return err
	}
	step("8. With the guide off the roster, a reply in its thread is not delivered and the thread says why")
	if err := p.guideNotRunning(r); err != nil {
		return err
	}
	note("all beekeeper decisions and the guide's conversation proven")
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
	fmt.Fprintf(&b, "  channels: {%q: %q}\n  gateway:\n    url: %q\n    tokenFile: %q\n    answerTool: %q\n    sendTool: %q\n",
		beekeeperTeam, channel, gateway, tokenFile, beekeeperAnswerTool, beekeeperSendTool)
	return b.String()
}

// applyBeekeeperCRDs applies the release's CRDs from its tag; the returned
// func deletes those the lab did not have before.
func applyBeekeeperCRDs(version string) (func(), error) {
	var created []string
	remove := func() {
		for _, name := range created {
			_ = deleteObject(context.Background(), gvrCRDs, "", name, 30*time.Second)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, name := range beekeeperCRDs {
		crd := name + ".beekeeper.giantswarm.io"
		if _, err := getObject(context.Background(), gvrCRDs, "", crd); err != nil {
			created = append(created, crd)
		}
		u := fmt.Sprintf("https://raw.githubusercontent.com/giantswarm/beekeeper/%s/config/crd/beekeeper.giantswarm.io_%s.yaml", version, name)
		resp, err := client.Get(u) //nolint:gosec // a fixed GitHub URL of the release
		if err != nil {
			remove()
			return nil, fmt.Errorf("fetching %s: %w", u, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			remove()
			return nil, fmt.Errorf("fetching %s: HTTP %d %v", u, resp.StatusCode, err)
		}
		if _, err := applyManifests(context.Background(), body); err != nil {
			remove()
			return nil, err
		}
	}
	return remove, nil
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: beekeeperNamespace, Labels: map[string]string{managedByLabel: managedByAgentlabValue}}}
	if _, err := k.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, fmt.Errorf("creating namespace %s: %w", beekeeperNamespace, err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k.clientset.CoreV1().Namespaces().Delete(ctx, beekeeperNamespace, metav1.DeleteOptions{})
	}, nil
}

// nodeKubeconfig is the lab kubeconfig for a process in the node's network:
// the API server on 127.0.0.1:6443 there, not the host's published port.
func nodeKubeconfig(runDir string) (string, error) {
	raw, err := os.ReadFile(labKubeconfig()) //nolint:gosec // the lab's own kubeconfig
	if err != nil {
		return "", err
	}
	out := regexp.MustCompile(`server: https://127\.0\.0\.1:\d+`).ReplaceAll(raw, []byte("server: https://127.0.0.1:6443"))
	path := filepath.Join(runDir, beekeeperKubeconfig)
	return path, os.WriteFile(path, out, 0o600) //nolint:gosec // the run directory
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from) //nolint:gosec // the lab's own files
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600) //nolint:gosec // the run directory
}

// startBeekeeperPostgres runs the mailboxes' Postgres in network on port;
// the returned func removes it.
func startBeekeeperPostgres(run, network string, port int) (func(), error) {
	name := "agentlab-beekeeper-pg-" + strings.ToLower(run)
	p := strconv.Itoa(port)
	if err := runQuiet(dockerBin, dockerRun(name, network, "-d", "--rm", "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		beekeeperPostgresImage, "postgres", "-p", p)...); err != nil {
		return nil, fmt.Errorf("starting Postgres: %w", err)
	}
	stop := func() { _ = runQuiet(dockerBin, "rm", "-f", name) }
	if !waitFor(60, time.Second, func() bool { return runQuiet(dockerBin, "exec", name, "pg_isready", "-U", "postgres", "-p", p) == nil }) {
		stop()
		return nil, fmt.Errorf("the Postgres container %s was not ready within a minute", name)
	}
	return stop, nil
}

// startBeekeeperServe runs serve from image (or binary, mounted into it) in
// network as the caller's uid, the run directory mounted with its
// configuration, token, kubeconfig and the lab CA, and waits for its
// /healthz on nodeIP; the returned func removes the container. Its log is
// beekeeper.log in runDir.
func startBeekeeperServe(image, binary, run, network, runDir, serveCfg, nodeIP string, port, pgPort int) (func(), error) {
	if err := os.WriteFile(filepath.Join(runDir, beekeeperConfigFile), []byte(serveCfg), 0o600); err != nil {
		return nil, err
	}
	name := "agentlab-beekeeper-" + strings.ToLower(run)
	mount := func(f string) string { return beekeeperMount + "/" + f }
	args := dockerRun(name, network, "-d", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", runDir+":"+beekeeperMount,
		"-e", "BEEKEEPER_CONFIG="+mount(beekeeperConfigFile),
		"-e", fmt.Sprintf("BEEKEEPER_DATABASE_URL=postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", pgPort),
		"-e", "KUBECONFIG="+mount(beekeeperKubeconfig),
		"-e", "SSL_CERT_FILE="+mount(beekeeperCAFile),
		"-e", "HOME=/tmp", "-e", "XDG_STATE_HOME=/tmp")
	if binary != "" {
		args = append(args, "-v", binary+":/beekeeper:ro")
	}
	args = append(args, image, "serve", fmt.Sprintf("--http=:%d", port))
	if out, err := command(dockerBin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("starting beekeeper serve: %w: %s", err, excerpt(string(out), 300))
	}
	logs := func() string { out, _ := outputQuiet(dockerBin, "logs", name); return out }
	stop := func() {
		_ = os.WriteFile(filepath.Join(runDir, "beekeeper.log"), []byte(logs()), 0o600)
		_ = runQuiet(dockerBin, "rm", "-f", name)
	}
	health := fmt.Sprintf("http://%s:%d/healthz", nodeIP, port)
	client := &http.Client{Timeout: 2 * time.Second}
	if !waitFor(60, 500*time.Millisecond, func() bool {
		resp, err := client.Get(health) //nolint:noctx // the proof's own serve
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		l := logs()
		stop()
		return nil, fmt.Errorf("beekeeper serve did not answer %s within 30s: %s", health, excerptEnds(l, 600))
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
	s, otherS                              *musterSession
	fake                                   *slackFakeContainer
	driver                                 *slackDriver
	asker, askerSlack, otherSlack, channel string
}

// add files a decision through muster as the asker and returns its number.
func (p *beekeeperProof) add(forWho, question, due string) (int, error) {
	text, err := p.s.callServerTool(beekeeperNoteAdd, map[string]any{
		slackKeyText: question, "for": forWho, beekeeperKeyKind: "decision", "agent": "beekeeper-decisions-test", "host": "agentlab",
		"status_quo": "The lab's merge lane takes one release a night.",
		"why":        "Only the lab's owners pick what rolls.",
		"options":    []any{"Roll tonight: the lane clears at 22:00", "Wait for Monday: nothing rolls before Monday"},
		"recommend":  2, "due": due, beekeeperKeyDefault: beekeeperDefault,
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

// message waits for the latest decision asking question in channel to
// satisfy pred: the question names it, since serve may hand a closed note's
// number out again (giantswarm/beekeeper#352).
func (p *beekeeperProof) message(channel, question, what string, wait time.Duration, pred func(slackMessage) bool) (slackMessage, error) {
	var last slackMessage
	if !waitFor(int(wait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		msgs := p.fake.thread(channel, "")
		for i := len(msgs) - 1; i >= 0; i-- {
			if strings.Contains(msgs[i].shown(), question) {
				last = msgs[i]
				return pred(last)
			}
		}
		return false
	}) {
		return last, fmt.Errorf("%q: %s did not happen within %s; the message reads: %s", question, what, wait, excerpt(last.shown(), 400))
	}
	return last, nil
}

func (p *beekeeperProof) answeredBy(channel, question string, id int, slackUser string, texts ...string) error {
	_, err := p.message(channel, question, "the answer's rewrite", decisionsWait, func(m slackMessage) bool {
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
	const q = "Roll the release onto the lab tonight?"
	id, err := p.add("asker", q, "2h")
	if err != nil {
		return err
	}
	dm := directChannel(p.askerSlack)
	m, err := p.message(dm, q, "the direct message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	if err := assertDecisionMessage(m); err != nil {
		return err
	}
	if tag := fmt.Sprintf("note #%d", id); !strings.Contains(m.shown(), tag) || strings.Contains(m.shown(), "note #note") {
		return fmt.Errorf("the decision's context line does not name %s once: %s", tag, excerpt(m.shown(), 300))
	}
	if err := p.driver.click(p.otherSlack, m, decisionChoose); err != nil {
		return err
	}
	if _, err := p.message(dm, q, "the refusal of a click by someone else", decisionsWait, func(m slackMessage) bool {
		_, open := m.action(decisionOwnWords)
		return open && strings.Contains(m.shown(), "<@"+p.otherSlack+">")
	}); err != nil {
		return err
	}
	note("note #%d: %s's click refused, the decision open", id, p.otherSlack)
	if err := p.driver.click(p.askerSlack, m, decisionChoose); err != nil {
		return err
	}
	return p.answeredBy(dm, q, id, p.askerSlack, "Roll tonight")
}

func (p *beekeeperProof) teamByThreadReply() error {
	const q = "Roll the release onto the lab on Monday instead?"
	id, err := p.add("team:"+beekeeperTeam, q, "2h")
	if err != nil {
		return err
	}
	m, err := p.message(p.channel, q, "the team's message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	ts := p.fake.nextTS()
	if err := p.driver.event(map[string]any{
		fieldTypeKey: slackKeyMessage, slackKeyUser: p.otherSlack, slackKeyChannel: p.channel, slackKeyChanType: slackKeyChannel,
		slackKeyText: decisionThreadReply, slackKeyTS: ts, slackKeyEventTS: ts, slackKeyThreadTS: m.TS, slackKeyParentUser: slackFakeBotUser,
	}); err != nil {
		return err
	}
	return p.answeredBy(p.channel, q, id, p.otherSlack, decisionThreadReply)
}

func (p *beekeeperProof) defaulted() error {
	const q = "Nobody answers this one?"
	id, err := p.add("team:"+beekeeperTeam, q, beekeeperDefaultDue)
	if err != nil {
		return err
	}
	open, err := p.message(p.channel, q, "the team's message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	if _, err := p.message(p.channel, q, "the default at the due time", beekeeperDefaultWait, func(m slackMessage) bool {
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
	const q = "Withdraw this one again?"
	id, err := p.add("team:"+beekeeperTeam, q, "2h")
	if err != nil {
		return err
	}
	if _, err := p.message(p.channel, q, "the team's message", decisionsWait, func(slackMessage) bool { return true }); err != nil {
		return err
	}
	if _, err := p.s.callServerTool(beekeeperNoteDone, map[string]any{beekeeperKeyNote: id}); err != nil {
		return err
	}
	if _, err := p.message(p.channel, q, "the withdrawn rewrite", decisionsWait, func(m slackMessage) bool {
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
	evs, err := k.clientset.CoreV1().Events(beekeeperNamespace).List(ctx, metav1.ListOptions{LabelSelector: managedByLabel + "=beekeeper"})
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

// The guide of the conversation steps, its address and what it says.
const (
	guideName      = "Guide"
	guideHost      = "agentlab"
	guideAddress   = "local:" + guideHost + "/" + guideName
	guideQuestion  = "Shall the release roll onto the lab tonight?"
	guideReply     = "Yes, after the 21:00 backup."
	guideAnswer    = "Noted; I hand it to the supervisor."
	guideNotRunMsg = "Not delivered to *" + guideName + " on " + guideHost + "*"
)

// guideThread is the guide's conversation: the gateway's id, the direct
// message and the opening message's ts.
type guideThread struct{ id, channel, ts string }

// guide calls a beekeeper tool as the asker's guide.
func (p *beekeeperProof) guide(tool string, args map[string]any) (string, error) {
	args["agent"], args["host"] = guideName, guideHost
	return p.s.callServerTool(tool, args)
}

// replyInThread sends slackUser's reply into the guide's thread, as Slack
// delivers a direct message.
func (p *beekeeperProof) replyInThread(r guideThread, slackUser, text string) error {
	ts := p.fake.nextTS()
	return p.driver.event(map[string]any{
		fieldTypeKey: slackKeyMessage, slackKeyUser: slackUser, slackKeyChannel: r.channel, slackKeyChanType: "im",
		slackKeyText: text, slackKeyTS: ts, slackKeyEventTS: ts, slackKeyThreadTS: r.ts, slackKeyParentUser: slackFakeBotUser,
	})
}

// inThread waits until a message of the guide's thread contains text.
func (p *beekeeperProof) inThread(r guideThread, what, text string) error {
	var last []slackMessage
	if !waitFor(int(decisionsWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		last = p.fake.thread(r.channel, r.ts)
		for _, m := range last {
			if strings.Contains(m.shown(), text) {
				return true
			}
		}
		return false
	}) {
		return fmt.Errorf("conversation %s: %s did not appear within %s; the thread holds %d messages", r.id, what, decisionsWait, len(last))
	}
	return nil
}

func (p *beekeeperProof) guideConversation() (guideThread, error) {
	r := guideThread{channel: directChannel(p.askerSlack)}
	if _, err := p.guide(beekeeperRegister, map[string]any{}); err != nil {
		return r, err
	}
	text, err := p.guide(beekeeperConverse, map[string]any{slackKeyText: guideQuestion})
	if err != nil {
		return r, err
	}
	if _, err := fmt.Sscanf(text, "converse: opened conversation %s", &r.id); err != nil {
		return r, fmt.Errorf("%s answered %q, not an opened conversation", beekeeperConverse, excerpt(text, 200))
	}
	m, err := p.message(r.channel, guideQuestion, "the guide's direct message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return r, err
	}
	r.ts = m.TS
	note("conversation %s: the guide's question in %s", r.id, r.channel)

	if err := p.replyInThread(r, p.askerSlack, guideReply); err != nil {
		return r, err
	}
	var got string
	if !waitFor(int(decisionsWait/time.Second), time.Second, func() bool {
		got, err = p.guide(beekeeperReceive, map[string]any{})
		return err == nil && strings.Contains(got, "from "+p.asker+" via slack: slack-")
	}) {
		return r, fmt.Errorf("the reply did not reach the guide's mailbox from %s via slack within %s: %s %v", p.asker, decisionsWait, excerpt(got, 300), err)
	}
	var id int
	if _, err := fmt.Sscanf(got, "%d message", &id); err != nil {
		return r, fmt.Errorf("%s answered %q, no delivery id", beekeeperReceive, excerpt(got, 200))
	}
	for _, msg := range p.fake.thread(r.channel, r.ts) {
		if strings.Contains(msg.shown(), guideNotRunMsg) {
			return r, fmt.Errorf("the delivered reply got a note: %s", excerpt(msg.shown(), 200))
		}
	}
	note("the reply is in the guide's mailbox: %s", strings.TrimSpace(got))
	if _, err := p.guide(beekeeperAck, map[string]any{"ids": []any{id}}); err != nil {
		return r, err
	}

	text, err = p.guide(beekeeperConverse, map[string]any{slackKeyText: guideAnswer})
	if err != nil {
		return r, err
	}
	if !strings.Contains(text, "posted to conversation "+r.id) {
		return r, fmt.Errorf("the guide's answer went elsewhere: %s", excerpt(text, 200))
	}
	if err := p.inThread(r, "the guide's answer", guideAnswer); err != nil {
		return r, err
	}
	note("the guide's answer is in the same thread")
	return r, nil
}

func (p *beekeeperProof) otherRefused() error {
	_, err := p.otherS.callServerTool(beekeeperSend, map[string]any{"to": guideAddress,
		"message": map[string]any{"messageId": "agentlab-other-" + randomSuffix(), "role": "user", "parts": []any{map[string]any{"kind": "text", slackKeyText: "hello"}}}})
	if err == nil || !strings.Contains(err.Error(), "is not running") {
		return fmt.Errorf("another person's send_message to %s was not refused: %v", guideAddress, err)
	}
	note("refused: %s", excerpt(err.Error(), 200))
	return nil
}

// guideNotRunning takes the guide off the roster, as its local beekeeper
// does when the session ends, and replies in its thread.
func (p *beekeeperProof) guideNotRunning(r guideThread) error {
	gvr, err := gvrFor("rosterentries.beekeeper.giantswarm.io")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	entries, err := listObjects(ctx, gvr, beekeeperNamespace, "")
	if err != nil {
		return err
	}
	removed := false
	for _, e := range entries {
		if addr, _, _ := unstructured.NestedString(e.Object, "spec", "address"); addr == guideAddress {
			if err := deleteObject(ctx, gvr, beekeeperNamespace, e.GetName(), 30*time.Second); err != nil {
				return err
			}
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("no RosterEntry of %s in %s", guideAddress, beekeeperNamespace)
	}
	if err := p.replyInThread(r, p.askerSlack, "Are you still there?"); err != nil {
		return err
	}
	if err := p.inThread(r, "the not-delivered note", guideNotRunMsg); err != nil {
		return err
	}
	for _, m := range p.fake.thread(r.channel, r.ts) {
		if strings.Contains(m.shown(), guideNotRunMsg) {
			if !strings.Contains(m.shown(), "is not running") {
				return fmt.Errorf("the note does not say the guide is not running: %s", excerpt(m.shown(), 300))
			}
			note("the thread says: %s", excerpt(m.shown(), 200))
			return nil
		}
	}
	return nil
}
