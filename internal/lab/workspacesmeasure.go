package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-measure`: the numbers behind the workspaces design's
// defaults (W3, W4, W8), measured on a cluster with the platform, through
// muster as the person: two workspaces, a small one and one with a large
// repository, created, measured and deleted once per run. Per run and
// workspace:
//
//   - the first sync (create_workspace to the cycle's result), a no-op cycle
//     (sync_workspace with nothing pushed, once the manager's one-minute
//     listing cache has let go) and a changed cycle (after a push to one
//     repository);
//   - one online expansion of the small workspace's volume in the first run,
//     where the StorageClass provisions a size: update_workspace raising its
//     minimum, the manager growing the claim, the driver resizing it;
//   - authorize_workspace, start_session with the grant and the setup's
//     record of the session directory (from start_session, and from the
//     first turn), after a push, so the setup fetches;
//   - the on-demand fetch, the setup's own `git fetch` timed in the
//     Session's sandbox after another push;
//   - a first checkout, `git status` and a build in the Session's clone on
//     the workspace volume, and the same on the node's local disk, from a
//     pod with the Session's mounts; the session directory's size;
//   - the standing storage: the claim's capacity, the volume's use, each
//     mirror's measured size against the provider's (W8's factor 1.5), the
//     headroom max(4 GiB, 50 %).
//
// A figure the cluster cannot report is listed as not available with the
// reason, never estimated. The report goes to the run directory as JSON and
// Markdown and is printed. Everything the command creates (the workspaces,
// their volumes, the Sessions, the Agent, the probe pods, the pushes, a
// generated repository) is removed on every path. The lab is the default
// target; --kube-context and --muster-url measure another cluster with the
// platform, as the person signed in there (`muster auth login`).

// DefaultWorkspacesMeasureRuns is how many runs the measurement makes.
const DefaultWorkspacesMeasureRuns = 5

const (
	measureSmall = "small"
	measureLarge = "large"
	// The workspaces the measurement creates, removed on every path.
	measureSmallWorkspace = "agentlab-measure-small"
	measureLargeWorkspace = "agentlab-measure-large"
	// measureAgent is the lab's Agent the Sessions run on.
	measureAgent = "agentlab-workspaces-measure"
	// measureLabLargeRepo is the lab GitHub's large fixture.
	measureLabLargeRepo = syncOrg + "/monorepo"
	// measureLabBranch is the default branch of every fixture repository.
	measureLabBranch = "main"
	// measurePoll is how often a wait looks: the figures are wall-clock
	// times from a call to the first read that sees its end.
	measurePoll = 500 * time.Millisecond
	// The report's files in the run directory.
	measureReportJSON     = "workspaces-measure.json"
	measureReportMarkdown = "workspaces-measure.md"
	// measureBuildDir is the build's cache and output beside the clone,
	// removed after it.
	measureBuildDir = ".agentlab-measure-build"
	// measureLocalVolume is the git probe's emptyDir, the node's local disk,
	// mounted at its name.
	measureLocalVolume = "local"
	// measureNoBuild is the git probe's word for a repository without a Go
	// module.
	measureNoBuild = "none"
)

// The lab's small workspace: two repositories of one owner, the docs
// repository the pushes go to and a Go module the build runs in.
var measureLabSmallRepos = []string{syncOSS + "/docs", syncOSS + "/kagent"}

// WorkspacesMeasureOptions tune the measurement.
type WorkspacesMeasureOptions struct {
	// Email is the lab user the measurement works as, a member of the lab's
	// Organization (default the admin); another cluster's person is the one
	// signed in to its muster.
	Email string
	// Runs is how many times each figure is measured; zero is
	// DefaultWorkspacesMeasureRuns.
	Runs int
	// LargeRepoMiB adds a repository of this many MiB to the lab's GitHub
	// for the large workspace instead of its fixture's; zero keeps the
	// fixture's.
	LargeRepoMiB int
	// ReadyTimeout bounds each wait: a cycle, a Session, a probe pod.
	ReadyTimeout time.Duration
	// RunDir is where the report is written; "" is a directory of its own
	// under the state directory.
	RunDir string

	// Another cluster with the platform: the context of the person's
	// kubeconfig and the muster URL (both or neither), the Organization,
	// the provider instance and the repositories of the two workspaces.
	KubeContext  string
	MusterURL    string
	Organization string
	Provider     string
	SmallRepos   []string
	LargeRepo    string
	// AgentgatewayURL and Agent (namespace/name, an Agent whose Sessions may
	// reach the provider) measure the Sessions there; without them the
	// Session figures are not available.
	AgentgatewayURL string
	Agent           string
}

// remote says whether the options name another cluster.
func (o WorkspacesMeasureOptions) remote() bool { return o.KubeContext != "" || o.MusterURL != "" }

// validate refuses options of one target given to the other.
func (o WorkspacesMeasureOptions) validate() error {
	switch {
	case o.Runs < 0:
		return fmt.Errorf("--runs %d: at least 1", o.Runs)
	case o.LargeRepoMiB < 0 || o.LargeRepoMiB > githubFakeMaxGeneratedMiB:
		return fmt.Errorf("--large-repo-mib %d: 1 to %d, or 0 for the fixture's", o.LargeRepoMiB, githubFakeMaxGeneratedMiB)
	}
	labOnly := map[string]bool{"[email]": o.Email != "", "--large-repo-mib": o.LargeRepoMiB != 0}
	remoteOnly := map[string]bool{"--organization": o.Organization != "", "--provider": o.Provider != "", "--small-repos": len(o.SmallRepos) > 0,
		"--large-repo": o.LargeRepo != "", "--agentgateway-url": o.AgentgatewayURL != "", "--agent": o.Agent != ""}
	if !o.remote() {
		for _, name := range slices.Sorted(maps.Keys(remoteOnly)) {
			if remoteOnly[name] {
				return fmt.Errorf("%s names another cluster's setting: give it with --kube-context and --muster-url (the lab's are its own)", name)
			}
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(labOnly)) {
		if labOnly[name] {
			return fmt.Errorf("%s is the lab's: on another cluster the person is the one signed in to its muster and the repositories are named (--small-repos, --large-repo)", name)
		}
	}
	missing := []string{}
	for name, empty := range map[string]bool{"--kube-context": o.KubeContext == "", "--muster-url": o.MusterURL == "", "--organization": o.Organization == "",
		"--provider": o.Provider == "", "--small-repos": len(o.SmallRepos) == 0, "--large-repo": o.LargeRepo == ""} {
		if empty {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("another cluster needs %s as well", strings.Join(missing, ", "))
	}
	for _, r := range append(slices.Clone(o.SmallRepos), o.LargeRepo) {
		if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("repository %q: owner/name", r)
		}
	}
	if (o.AgentgatewayURL == "") != (o.Agent == "") {
		return fmt.Errorf("--agentgateway-url and --agent go together: the Sessions start on the Agent and their turns run through the agentgateway")
	}
	if ns, name, ok := strings.Cut(o.Agent, "/"); o.Agent != "" && (!ok || ns == "" || name == "") {
		return fmt.Errorf("--agent %q: namespace/name", o.Agent)
	}
	return nil
}

// measureWorkspace is one of the two workspaces and its samples.
type measureWorkspace struct {
	name, kind string
	repos      []string
	// pushed is the repository pushes go to ("" where the measurement
	// cannot push); worked the one the checkout, status and build run in.
	pushed, worked string
	branch         string
	volume         syncVolume
	lastSync       *syncLastSync
	providerKiB    map[string]int64
	samples        *measureSamples
	storage        measureStorage
}

func (w *measureWorkspace) owners() []string {
	var out []string
	for _, r := range w.repos {
		owner, _, _ := strings.Cut(r, "/")
		if !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	return out
}

// measurePusher pushes commits to a repository of the lab's GitHub from a
// shallow clone on this host, and puts its head back at the end.
type measurePusher struct {
	repo, dir, token, start string
	pushes                  int
}

// measureDetail is get_workspace's answer, as far as the measurement reads
// it.
type measureDetail struct {
	Workspace struct {
		syncWorkspace
		NeededSize string `json:"neededSize"`
		VolumeSize string `json:"volumeSize"`
	} `json:"workspace"`
	Mirrors []syncMirror `json:"mirrors"`
	Storage struct {
		MirrorsBytes  int64  `json:"mirrorsBytes"`
		SessionsBytes int64  `json:"sessionsBytes"`
		NeededSize    string `json:"neededSize"`
		VolumeSize    string `json:"volumeSize"`
	} `json:"storage"`
}

func (d *measureDetail) mirror(repo string) *syncMirror {
	for i := range d.Mirrors {
		if d.Mirrors[i].Repository == repo {
			return &d.Mirrors[i]
		}
	}
	return nil
}

// measurement is the command's state.
type measurement struct {
	opts      WorkspacesMeasureOptions
	cfg       *config.Config // nil on another cluster
	timeout   time.Duration
	provider  string
	org       string
	person    *musterSession
	signedIn  *workspacePerson // the lab's person, connected to the provider
	api       *kagentAPI
	set       *sessionSet
	agentNS   string
	agentName string
	sessions  string // why the Session figures are not available, "" when they are
	applied   bool   // the lab's Agent is the measurement's
	gh        *labGitHubClient
	pushers   map[string]*measurePusher
	generated string
	listedAt  map[string]time.Time // owner → the manager's last listing of it
	ws        []*measureWorkspace
	created   []string
	report    measureReport
	runDir    string
}

// WorkspacesMeasure runs the measurement and writes its report.
func WorkspacesMeasure(cfg *config.Config, opts WorkspacesMeasureOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if opts.Runs == 0 {
		opts.Runs = DefaultWorkspacesMeasureRuns
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultWorkspacesReadyTimeout
	}
	m := &measurement{opts: opts, timeout: opts.ReadyTimeout, pushers: map[string]*measurePusher{}, listedAt: map[string]time.Time{}}
	m.report = measureReport{Started: time.Now().UTC(), Runs: opts.Runs}
	if opts.remote() {
		useKubeContext(opts.KubeContext)
		m.provider, m.org = opts.Provider, opts.Organization
		m.report.Target = fmt.Sprintf("kube context %s, muster %s", opts.KubeContext, opts.MusterURL)
	} else {
		if cfg == nil || !cfg.WorkspacesEnabled() {
			return fmt.Errorf("platform.workspaces is off in %s — `agentlab configure --workspaces` turns it on, then `agentlab platform`", config.File)
		}
		if err := useClusterKubeconfig(cfg); err != nil {
			return err
		}
		m.cfg, m.provider, m.org = cfg, config.WorkspaceProviderFake, config.WorkspaceOrganization
		m.report.Target = "the lab " + cfg.ClusterName
	}
	var err error
	if m.runDir, err = measureRunDir(opts.RunDir, m.report.Started); err != nil {
		return err
	}
	p := newProofStages()
	err = m.run(p)
	m.cleanup()
	m.report.Ended = time.Now().UTC()
	if err != nil {
		m.report.Error = err.Error()
	}
	if werr := m.writeReport(); werr != nil {
		err = errors.Join(err, werr)
	}
	return p.close(err)
}

// measureRunDir is the run directory: the one given, or a new one under the
// state directory.
func measureRunDir(dir string, started time.Time) (string, error) {
	if dir == "" {
		dir = filepath.Join(StateDir, "workspaces-measure-"+started.Format("20060102-150405"))
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("the run directory %s: %w", dir, err)
	}
	return dir, nil
}

func (m *measurement) run(p *proofStages) error {
	p.begin("measure: preconditions", "The measurement's preconditions on %s: the workspace-manager through muster, the person signed in, the repositories, the Agent", m.report.Target)
	if err := m.preconditions(); err != nil {
		return err
	}
	for run := 1; run <= m.opts.Runs; run++ {
		p.begin(fmt.Sprintf("run %d: first sync", run), "Run %d of %d: create_workspace %s, each first cycle timed", run, m.opts.Runs, strings.Join(m.names(), " and "))
		for _, w := range m.ws {
			if err := m.firstSync(w, run == 1); err != nil {
				return err
			}
		}
		p.begin(fmt.Sprintf("run %d: no-op", run), "Run %d: a cycle with nothing pushed on each, once the listing cache let go", run)
		for _, w := range m.ws {
			if err := m.noOp(w); err != nil {
				return err
			}
		}
		p.begin(fmt.Sprintf("run %d: changed", run), "Run %d: a push to one repository of each, then a cycle", run)
		if err := m.changed(); err != nil {
			return err
		}
		if run == 1 {
			p.begin("run 1: expansion", "One online expansion of %s's volume, where its StorageClass provisions a size", m.ws[0].name)
			if err := m.expand(m.ws[0]); err != nil {
				return err
			}
		}
		p.begin(fmt.Sprintf("run %d: sessions", run), "Run %d: a Session on each workspace through authorize_workspace and start_session, the setup, the on-demand fetch, a checkout, git status and a build on the volume and on a local disk", run)
		for _, w := range m.ws {
			if err := m.session(w, run == m.opts.Runs); err != nil {
				return err
			}
		}
		p.begin(fmt.Sprintf("run %d: delete", run), "Run %d: the Sessions, the session directories and the workspaces deleted, each volume gone", run)
		if err := m.deleteAll(); err != nil {
			return err
		}
	}
	p.begin("measure: report", "The report, written to %s", m.runDir)
	return nil
}

func (m *measurement) names() []string {
	out := make([]string, 0, len(m.ws))
	for _, w := range m.ws {
		out = append(out, w.name)
	}
	return out
}

// preconditions signs the person in, readies the repositories and the Agent
// and removes what an interrupted run left.
func (m *measurement) preconditions() error {
	if m.opts.remote() {
		if err := m.remotePreconditions(); err != nil {
			return err
		}
	} else if err := m.labPreconditions(); err != nil {
		return err
	}
	for _, w := range m.ws {
		w.samples = newMeasureSamples()
		w.providerKiB = map[string]int64{}
		if w.pushed == "" {
			w.samples.notAvailable(figChangedCycle, "the measurement pushes only to the lab's GitHub")
			w.samples.notAvailable(figOnDemandFetch, "the measurement pushes only to the lab's GitHub")
		}
		if m.sessions != "" {
			for _, key := range []string{figSessionStart, figSessionSetup, figOnDemandFetch, figCheckoutVolume, figCheckoutLocal, figStatusVolume, figStatusLocal, figBuildVolume, figBuildLocal, figSessionBytes} {
				w.samples.notAvailable(key, m.sessions)
			}
		}
		gone, err := m.gone(w.name)
		if err != nil {
			return err
		}
		if !gone {
			note("removing the leftover workspace %s of an interrupted run", w.name)
			m.created = append(m.created, w.name)
			if err := m.deleteWorkspace(w); err != nil {
				return err
			}
		}
	}
	m.report.WorkspaceManager = workspaceManagerImage()
	if m.report.WorkspaceManager != "" {
		note("workspace-manager %s", m.report.WorkspaceManager)
	}
	return nil
}

// labPreconditions is the lab's side: the person signed in and connected to
// the lab's GitHub, the large repository (generated when asked), a shallow
// clone of each pushed repository on this host, the measurement's Agent.
func (m *measurement) labPreconditions() error {
	cfg := m.cfg
	if reason := workspaceManagerSkip(cfg); reason != "" {
		return fmt.Errorf("no workspace-manager to define a workspace with: %s", reason)
	}
	if !cfg.WorkspaceFakeProvider() {
		return fmt.Errorf("the measurement's workspaces are the lab GitHub's, the provider instance %s, and platform.workspaces.provider %q does not configure it: `agentlab configure --workspaces-provider %s`, then `agentlab platform`",
			config.WorkspaceProviderFake, cfg.Platform.Workspaces.Provider, config.WorkspaceProviderFake)
	}
	if err := waitMCPServerState(workspaceManagerMCPServer, mcpServerStateConnected); err != nil {
		return fmt.Errorf("the workspace-manager is not connected to muster: %w", err)
	}
	email := m.opts.Email
	if email == "" {
		if admin := cfg.AdminUser(); admin != nil {
			email = admin.Email
		}
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %s in %s", email, config.File)
	}
	if !slices.ContainsFunc(config.WorkspaceOrganizationGroups, user.HasGroup) {
		return fmt.Errorf("%s is no member of the Organization %s (groups %s): the measurement creates workspaces as a member", email, config.WorkspaceOrganization, strings.Join(config.WorkspaceOrganizationGroups, ", "))
	}
	var err error
	if m.signedIn, err = openWorkspacePerson(cfg, user); err != nil {
		return err
	}
	m.person = m.signedIn.session
	hops, err := connectWorkspaceProvider(cfg, m.signedIn, config.WorkspaceProviderFake)
	if err != nil {
		return err
	}
	note("%s signed in to muster and connected to %s: %s", user.Email, config.WorkspaceProviderFake, hops)
	if m.gh, err = openLabGitHub(cfg); err != nil {
		return err
	}
	large := measureLabLargeRepo
	if m.opts.LargeRepoMiB > 0 {
		large = fmt.Sprintf("%s/monorepo-%dmib", syncOrg, m.opts.LargeRepoMiB)
		note("adding %s, %d MiB generated, to the lab's GitHub (seeding a large one takes minutes)", large, m.opts.LargeRepoMiB)
		m.generated = large
		size, err := m.gh.addGeneratedRepo(large, m.opts.LargeRepoMiB, "Go")
		if err != nil {
			return err
		}
		note("%s on the lab's GitHub: %s as GitHub's size reports it", large, formatBytes(size*1024))
	}
	m.ws = []*measureWorkspace{
		{name: measureSmallWorkspace, kind: measureSmall, repos: measureLabSmallRepos, pushed: syncMovingRepo, worked: syncListRepoB, branch: measureLabBranch},
		{name: measureLargeWorkspace, kind: measureLarge, repos: []string{large}, pushed: large, worked: large, branch: measureLabBranch},
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	for _, w := range m.ws {
		if err := m.clonePusher(w.pushed); err != nil {
			return err
		}
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the probes' git image (%s)", res.d)
	}
	return m.labAgent(user)
}

// labAgent readies the measurement's Agent on the claude Harness and the
// kagent API as the person.
func (m *measurement) labAgent(user *config.User) error {
	cfg := m.cfg
	ctx := context.Background()
	if err := waitHarnessReady(ctx, claudeHarness, m.timeout); err != nil {
		return err
	}
	if agentExists(measureAgent) {
		if err := removeAgent(measureAgent); err != nil {
			return err
		}
	}
	if _, err := applyManifests(ctx, []byte(sessionsAgentManifest(measureAgent, claudeHarness, cfg.GitHubFakeURL()))); err != nil {
		return fmt.Errorf("applying the Agent %s: %w", measureAgent, err)
	}
	m.applied = true
	boot, err := waitAgentReadyOn(measureAgent, claudeHarness, m.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", measureAgent, claudeHarness, boot.elapsed.Round(time.Second), boot.reason)
	}
	m.agentNS, m.agentName, m.report.Harness = kagentNamespace, measureAgent, claudeHarness
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if m.api, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	m.set = &sessionSet{api: m.api}
	note("the Agent %s Ready on Harness %s in %s", measureAgent, claudeHarness, boot.elapsed.Round(time.Second))
	return nil
}

// remotePreconditions is another cluster's side: the person's tokens from
// their muster sign-in, the named repositories.
func (m *measurement) remotePreconditions() error {
	o := m.opts
	access, err := musterCLIToken(o.MusterURL, false)
	if err != nil {
		return err
	}
	if m.person, err = openMusterSessionAt(o.MusterURL, access, "workspaces-measure"); err != nil {
		return err
	}
	note("signed in to %s with the person's muster session; Kubernetes reads through context %s", o.MusterURL, o.KubeContext)
	m.ws = []*measureWorkspace{
		{name: measureSmallWorkspace, kind: measureSmall, repos: o.SmallRepos, worked: o.SmallRepos[0], branch: ""},
		{name: measureLargeWorkspace, kind: measureLarge, repos: []string{o.LargeRepo}, worked: o.LargeRepo, branch: ""},
	}
	if o.Agent == "" {
		m.sessions = "no --agent and --agentgateway-url to start a Session on this cluster with"
		return nil
	}
	id, err := musterCLIToken(o.MusterURL, true)
	if err != nil {
		return err
	}
	if m.api, err = dialKagentAPIAt(o.AgentgatewayURL, id); err != nil {
		return err
	}
	m.set = &sessionSet{api: m.api}
	m.agentNS, m.agentName, _ = strings.Cut(o.Agent, "/")
	m.report.Harness = "Agent " + o.Agent + "'s"
	return nil
}

// musterCLIToken is the person's token of their muster sign-in at url: the
// access token muster takes, or with id the OIDC ID token kagent takes. The
// token is handed to the measurement's clients only, never printed.
func musterCLIToken(url string, id bool) (string, error) {
	args := append(strings.Fields("auth token --endpoint"), url)
	if id {
		args = append(args, "--id")
	}
	out, err := exec.Command("muster", args...).Output() // #nosec G204 -- the muster CLI with the command's own URL
	token := strings.TrimSpace(string(out))
	if err != nil || token == "" {
		return "", fmt.Errorf("no muster sign-in for %s (`muster auth login --endpoint %s` signs in): %v", url, url, err)
	}
	return token, nil
}

// workspaceManagerImage is the workspace-manager's image tag in the platform
// namespace, "" where it cannot be read.
func workspaceManagerImage() string {
	k, err := labKube()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, workspaceManagerMCPServer, metav1.GetOptions{})
	if err != nil || len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	image := d.Spec.Template.Spec.Containers[0].Image
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return image
}

// clonePusher makes a shallow clone of repo on this host, without a working
// tree, to push from.
func (m *measurement) clonePusher(repo string) error {
	owner, _, _ := strings.Cut(repo, "/")
	token, err := m.gh.installationToken(owner)
	if err != nil {
		return fmt.Errorf("the measurement pushes to %s as the App's installation on %s: %w", repo, owner, err)
	}
	dir, err := os.MkdirTemp(StateDir, "workspaces-measure-push-")
	if err != nil {
		return err
	}
	// git runs in the clone: every path it is handed is absolute.
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	pu := &measurePusher{repo: repo, dir: filepath.Join(dir, "clone"), token: token}
	m.pushers[repo] = pu
	if _, err := m.gh.git(dir, token, "clone", "-q", "--depth", "1", "--no-checkout", m.gh.remote(repo), "clone"); err != nil {
		return err
	}
	if pu.start, err = m.gh.git(pu.dir, "", "rev-parse", "HEAD"); err != nil {
		return err
	}
	note("%s cloned on this host at %s, shallow, to push to", repo, shortSHA(pu.start))
	return nil
}

// push commits a small file on the default branch and pushes it, returning
// the new head: the index read from HEAD, so no working tree is needed.
func (m *measurement) push(repo string) (string, error) {
	pu := m.pushers[repo]
	pu.pushes++
	file := filepath.Join(filepath.Dir(pu.dir), "pushed.md")
	content := fmt.Sprintf("# pushed by agentlab workspaces-measure\n\npush %d at %s\n", pu.pushes, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		return "", err
	}
	blob, err := m.gh.git(pu.dir, "", "hash-object", "-w", file)
	if err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"read-tree", "HEAD"},
		{"update-index", "--add", "--cacheinfo", "100644," + blob + ",agentlab-measure.md"},
		{gitCommit, "-q", "-m", fmt.Sprintf("workspaces-measure: push %d", pu.pushes)},
		{githubPush, "-q", gitOrigin, "HEAD:refs/heads/" + measureLabBranch},
	} {
		if _, err := m.gh.git(pu.dir, pu.token, args...); err != nil {
			return "", err
		}
	}
	return m.gh.git(pu.dir, "", "rev-parse", "HEAD")
}

// get is get_workspace's answer.
func (m *measurement) get(name string) (*measureDetail, error) {
	var d measureDetail
	if err := m.person.callServerStructured(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// gone says whether get_workspace answers not found.
func (m *measurement) gone(name string) (bool, error) {
	env, err := m.person.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name})
	if err != nil {
		return false, err
	}
	return env.IsError && len(env.Content) > 0 && strings.Contains(env.Content[0].Text, "not found"), nil
}

// sources are the workspace's sources: its repositories by name, one source
// per owner.
func (m *measurement) sources(w *measureWorkspace) []map[string]any {
	out := syncNamedSources(w.repos...)
	for _, s := range out {
		s[wmProviderArg] = m.provider
	}
	return out
}

// listed records that the manager just listed the workspace's owners.
func (m *measurement) listed(w *measureWorkspace) {
	for _, owner := range w.owners() {
		m.listedAt[owner] = time.Now()
	}
}

// waitListingFresh waits until the manager's listing cache of the
// workspace's owners has let go, so the next cycle lists the provider.
func (m *measurement) waitListingFresh(w *measureWorkspace) {
	var last time.Time
	for _, owner := range w.owners() {
		if t := m.listedAt[owner]; t.After(last) {
			last = t
		}
	}
	if wait := time.Until(last.Add(syncListingFreshness)); wait > 0 {
		note("waiting %s: the manager answers a listing of %s from its cache for a minute", wait.Round(time.Second), strings.Join(w.owners(), ", "))
		time.Sleep(wait)
	}
}

// waitCycle waits for a cycle of the workspace that ends after prev (nil:
// the first) and returns the workspace and when its end was first seen.
func (m *measurement) waitCycle(name string, prev *syncLastSync) (*measureDetail, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	var last *measureDetail
	var lastErr error
	var seen time.Time
	err := wait.PollUntilContextTimeout(ctx, measurePoll, m.timeout, true, func(context.Context) (bool, error) {
		d, err := m.get(name)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last, seen = d, time.Now()
		w := d.Workspace
		return w.ActiveSync == nil && w.SyncRequest == nil && w.LastSync != nil && (prev == nil || w.LastSync.Time.After(prev.Time)), nil
	})
	if err != nil {
		state := neverRead
		if last != nil {
			state = fmt.Sprintf("last sync %+v, active %v, request %v", last.Workspace.LastSync, last.Workspace.ActiveSync != nil, last.Workspace.SyncRequest != nil)
		}
		return nil, seen, fmt.Errorf("no cycle of %s ended within %s (%s; last error %v): the workspace-manager's log, its sync Jobs", name, m.timeout, state, lastErr)
	}
	if r := last.Workspace.LastSync; r.Result == syncResultFailed {
		return nil, seen, fmt.Errorf("the cycle of %s failed (trigger %s, job %s): %s", name, r.Trigger, r.Job, r.Message)
	}
	return last, seen, nil
}

// firstSync creates the workspace and times its first cycle; the first run
// also reads the provider's sizes through preview_workspace.
func (m *measurement) firstSync(w *measureWorkspace, first bool) error {
	if first {
		var preview struct {
			Repositories []struct {
				Repository string `json:"repository"`
				SizeKiB    int64  `json:"sizeKiB"`
			} `json:"repositories"`
		}
		if err := m.person.callServerStructured(workspaceManagerToolPrefix+"preview_workspace", map[string]any{
			wmOrganizationArg: m.org, wmSourcesArg: m.sources(w),
		}, &preview); err != nil {
			return err
		}
		for _, r := range preview.Repositories {
			w.providerKiB[r.Repository] = r.SizeKiB
		}
		if len(preview.Repositories) != len(w.repos) {
			return fmt.Errorf("preview_workspace selects %d of %s's repositories %v: the person reads them, the provider instance %s lists them", len(preview.Repositories), w.name, w.repos, m.provider)
		}
	}
	start := time.Now()
	var created syncWorkspace
	if err := m.person.callServerStructured(workspaceManagerToolPrefix+"create_workspace", map[string]any{
		nameKey: w.name, wmOrganizationArg: m.org, wmSourcesArg: m.sources(w),
	}, &created); err != nil {
		return err
	}
	if !slices.Contains(m.created, w.name) {
		m.created = append(m.created, w.name)
	}
	d, seen, err := m.waitCycle(w.name, nil)
	if err != nil {
		return err
	}
	m.listed(w)
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", w.name, r.Result, r.Message)
	}
	w.samples.since(figFirstSync, start, seen)
	w.lastSync = d.Workspace.LastSync
	v, err := m.volume(w.name)
	if err != nil {
		return err
	}
	w.volume = v
	note("%s: first sync %s (job %s), %d repositories, claim %s/%s", w.name, seen.Sub(start).Round(10*time.Millisecond), d.Workspace.LastSync.Job, d.Workspace.Repositories, v.namespace, v.claim)
	return nil
}

// volume finds the workspace's claim by its label.
func (m *measurement) volume(name string) (syncVolume, error) {
	k, err := labKube()
	if err != nil {
		return syncVolume{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	claims, err := k.clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{LabelSelector: syncWorkspaceLabel + "=" + name})
	if err != nil {
		return syncVolume{}, fmt.Errorf("listing the claim of workspace %s: %w", name, err)
	}
	if len(claims.Items) != 1 || claims.Items[0].Spec.VolumeName == "" {
		return syncVolume{}, fmt.Errorf("workspace %s has %d claims labelled %s=%s, want one bound", name, len(claims.Items), syncWorkspaceLabel, name)
	}
	c := claims.Items[0]
	return syncVolume{namespace: c.Namespace, claim: c.Name, pv: c.Spec.VolumeName}, nil
}

// sync requests a cycle once the listing cache let go and times it.
func (m *measurement) sync(w *measureWorkspace) (*measureDetail, time.Duration, error) {
	m.waitListingFresh(w)
	start := time.Now()
	var requested struct {
		RequestedAt time.Time `json:"requestedAt"`
	}
	if err := m.person.callServerStructured(workspaceManagerToolPrefix+"sync_workspace", map[string]any{wmWorkspaceArg: w.name}, &requested); err != nil {
		return nil, 0, err
	}
	d, seen, err := m.waitCycle(w.name, w.lastSync)
	if err != nil {
		return nil, 0, err
	}
	m.listed(w)
	w.lastSync = d.Workspace.LastSync
	return d, seen.Sub(start), nil
}

// noOp times a cycle with nothing pushed: it must end NoChange.
func (m *measurement) noOp(w *measureWorkspace) error {
	d, took, err := m.sync(w)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultNoChange {
		w.samples.notAvailable(figNoOpCycle, fmt.Sprintf("a cycle with nothing pushed ended %s (trigger %s), not %s", r.Result, r.Trigger, syncResultNoChange))
		warn("%s: a cycle with nothing pushed ended %s, not %s: not counted as a no-op", w.name, r.Result, syncResultNoChange)
		return nil
	}
	w.samples.add(figNoOpCycle, took.Seconds())
	note("%s: no-op cycle %s", w.name, took.Round(10*time.Millisecond))
	return nil
}

// changed pushes to each workspace's pushed repository, then times a cycle
// on each, which must mirror the pushed head.
func (m *measurement) changed() error {
	heads := map[string]string{}
	for _, w := range m.ws {
		if w.pushed == "" {
			continue
		}
		head, err := m.push(w.pushed)
		if err != nil {
			return err
		}
		heads[w.name] = head
	}
	for _, w := range m.ws {
		head, ok := heads[w.name]
		if !ok {
			continue
		}
		d, took, err := m.sync(w)
		if err != nil {
			return err
		}
		mirror := d.mirror(w.pushed)
		if mirror == nil || mirror.Head != head {
			return fmt.Errorf("after the changed cycle of %s the manifest names %v for %s, want the pushed head %s", w.name, mirror, w.pushed, head)
		}
		w.samples.add(figChangedCycle, took.Seconds())
		note("%s: changed cycle %s, %s at %s", w.name, took.Round(10*time.Millisecond), w.pushed, shortSHA(head))
	}
	return nil
}

// claimAndClass reads the workspace's claim and its StorageClass.
func (m *measurement) claimAndClass(w *measureWorkspace) (*corev1.PersistentVolumeClaim, *bool, string, error) {
	k, err := labKube()
	if err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	claim, err := k.clientset.CoreV1().PersistentVolumeClaims(w.volume.namespace).Get(ctx, w.volume.claim, metav1.GetOptions{})
	if err != nil {
		return nil, nil, "", fmt.Errorf("reading the claim %s/%s: %w", w.volume.namespace, w.volume.claim, err)
	}
	if claim.Spec.StorageClassName == nil {
		return claim, nil, "", nil
	}
	class, err := k.clientset.StorageV1().StorageClasses().Get(ctx, *claim.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, "", fmt.Errorf("reading the StorageClass %s: %w", *claim.Spec.StorageClassName, err)
	}
	expands := class.AllowVolumeExpansion != nil && *class.AllowVolumeExpansion
	return claim, &expands, class.Provisioner, nil
}

// expand times one online expansion: update_workspace raises the minimum a
// GiB above the claim's request, the manager raises the claim's request,
// the driver resizes the volume.
func (m *measurement) expand(w *measureWorkspace) error {
	claim, expands, _, err := m.claimAndClass(w)
	if err != nil {
		return err
	}
	e := &measureExpansion{}
	w.storage.Expansion = e
	if expands == nil || !*expands {
		e.NotAvailable = fmt.Sprintf("the StorageClass %s does not allow volume expansion: an elastic class, nothing is sized (W8)", ptrOr(claim.Spec.StorageClassName, "(none)"))
		note("%s", e.NotAvailable)
		return nil
	}
	current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	target := resource.NewQuantity((current.Value()+measureGiB+measureGiB-1)/measureGiB*measureGiB, resource.BinarySI)
	e.From, e.To = current.String(), target.String()
	start := time.Now()
	var updated syncWorkspace
	if err := m.person.callServerStructured(workspaceManagerToolPrefix+"update_workspace", map[string]any{
		nameKey: w.name, wmSourcesArg: m.sources(w), "sizing": map[string]any{"minimum": target.String()},
	}, &updated); err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	var requested, resized time.Time
	var last *corev1.PersistentVolumeClaim
	err = wait.PollUntilContextTimeout(ctx, measurePoll, m.timeout, true, func(ctx context.Context) (bool, error) {
		c, err := k.clientset.CoreV1().PersistentVolumeClaims(w.volume.namespace).Get(ctx, w.volume.claim, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = c
		now := time.Now()
		if requested.IsZero() && c.Spec.Resources.Requests.Storage().Cmp(*target) >= 0 {
			requested = now
		}
		if !requested.IsZero() && c.Status.Capacity.Storage().Cmp(*target) >= 0 {
			resized = now
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		state := neverRead
		if last != nil {
			state = fmt.Sprintf("request %s, capacity %s, conditions %v", last.Spec.Resources.Requests.Storage(), last.Status.Capacity.Storage(), last.Status.Conditions)
		}
		return fmt.Errorf("the volume of %s did not grow from %s to %s within %s (%s): the workspace-manager's log, the driver's resizer", w.name, e.From, e.To, m.timeout, state)
	}
	req, res := requested.Sub(start).Seconds(), resized.Sub(requested).Seconds()
	e.RequestedSeconds, e.ResizedSeconds = &req, &res
	note("%s: %s → %s, the claim raised %.2fs after update_workspace, resized %.2fs later", w.name, e.From, e.To, req, res)
	// The update starts a cycle of its own: the next steps begin after it.
	d, _, err := m.waitCycle(w.name, w.lastSync)
	if err != nil {
		return err
	}
	m.listed(w)
	w.lastSync = d.Workspace.LastSync
	return nil
}

func ptrOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// session starts a Session on the workspace with the person's grant and
// measures its setup, the on-demand fetch, the git work and the storage.
func (m *measurement) session(w *measureWorkspace, last bool) error {
	grant, err := m.grant(w)
	if err != nil {
		return err
	}
	if m.sessions != "" {
		return m.storage(w, last)
	}
	startCalled := time.Now()
	env, err := m.person.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: m.agentName, namespaceKey: m.agentNS, startSessionRequestIDArg: uuid.NewString(), "workspaceGrant": grant,
	})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("start_session on %s refused: %.400s", w.name, env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	id := started.Session.ID
	m.set.add(id)
	ctx, cancel := context.WithTimeout(context.Background(), 3*m.timeout)
	defer cancel()
	ks, err := m.api.getSession(ctx, id)
	if err != nil {
		return err
	}
	token := randomHex(8)
	reply, sent, err := measureTurn(ctx, m.api, ks, sessionsFirstTurnCommand(token))
	if err != nil {
		return err
	}
	first, err := parseSessionsTurn(reply)
	if err != nil {
		return err
	}
	record, err := m.setupRecord(w, id)
	if err != nil {
		return err
	}
	w.samples.since(figSessionStart, startCalled, record.SetUp)
	w.samples.since(figSessionSetup, sent, record.SetUp)
	note("%s: Session %s works in %s (%s); start_session to the directory set up %s, the first turn sent to set up %s; cloned %s, fetched %s",
		w.name, id, first.pwd, headsLine(first.heads), record.SetUp.Sub(startCalled).Round(10*time.Millisecond), record.SetUp.Sub(sent).Round(10*time.Millisecond),
		strings.Join(record.Cloned, ", "), cmpOr(strings.Join(record.Fetched, ", "), "nothing"))
	for _, problem := range record.Problems {
		warn("%s: the setup's problem: %s: %s", w.name, problem.Repository, excerpt(problem.Reason, 300))
	}
	if w.pushed != "" {
		if err := m.onDemandFetch(ctx, w, ks); err != nil {
			return err
		}
	}
	if err := m.gitWork(w, id); err != nil {
		return err
	}
	if err := m.storage(w, last); err != nil {
		return err
	}
	if err := m.api.deleteSession(ctx, id); err != nil {
		return err
	}
	m.set.ids = slices.DeleteFunc(m.set.ids, func(s string) bool { return s == id })
	return nil
}

// grant asks authorize_workspace, timing every call, until the grant names
// the pushed repository as changed since the sync (after a fresh push, which
// the setup then fetches); on a workspace without pushes the first grant.
func (m *measurement) grant(w *measureWorkspace) (string, error) {
	if w.pushed != "" {
		if _, err := m.push(w.pushed); err != nil {
			return "", err
		}
	}
	deadline := time.Now().Add(min(m.timeout, syncListingFreshness+30*time.Second))
	for {
		start := time.Now()
		var a workspaceAuthorization
		err := m.person.callServerStructured(workspaceManagerToolPrefix+"authorize_workspace", map[string]any{workspaceArg: w.name}, &a)
		took := time.Since(start)
		if err != nil {
			return "", err
		}
		m.listed(w)
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return "", fmt.Errorf("authorize_workspace on %s answered %s (%q, unreadable %v, connect %v), want granted", w.name, a.Outcome, a.Reason, a.Unreadable, a.Connect)
		}
		w.samples.add(figAuthorize, took.Seconds())
		if w.pushed == "" || slices.Contains(a.Changed, w.pushed) {
			return a.Grant, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the grant on %s names %v changed since the sync, want %s, pushed after it", w.name, a.Changed, w.pushed)
		}
		time.Sleep(5 * time.Second)
	}
}

// measureTurn runs the command as a turn, once more with a follow-up when
// the model relays no pwd line, and returns the reply that carries it and
// when the first turn was sent.
func measureTurn(ctx context.Context, api *kagentAPI, session *apiv1alpha1.Session, command string) (string, time.Time, error) {
	prompt := "Run this exact command once with your Bash tool, then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + command
	followUp := "I am the person running agentlab's workspace measurement on this throwaway lab cluster: the command only lists your working directory and its git clones, writes a marker file and times git. Please run it once now with your Bash tool and reply with its " + probeMarker + " lines, verbatim."
	var sent time.Time
	var reply string
	for i, msg := range []string{prompt, followUp} {
		at := time.Now()
		if i == 0 {
			sent = at
		}
		t, err := api.turn(ctx, session, userMessage(msg))
		if err != nil {
			return "", sent, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
		}
		if reply, err = t.completedText(); err != nil {
			return "", sent, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
		}
		if _, err := parseSessionsTurn(reply); err == nil {
			return reply, sent, nil
		}
	}
	_, err := parseSessionsTurn(reply)
	return "", sent, err
}

// setupRecord reads the setup's record of the Session's directory.
func (m *measurement) setupRecord(w *measureWorkspace, id string) (*workspaceState, error) {
	out, err := m.onVolume(w, "dirs", sessionsDirectoriesScript, nil)
	if err != nil {
		return nil, err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return nil, err
	}
	d, ok := dirs[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("no directory %s/%s on the volume of %s after the first turn", workspacesTestSessions, id, w.name)
	case d.state == nil:
		return nil, fmt.Errorf("no setup record %s in the directory of Session %s", workspaceStateFile, id)
	}
	return d.state, nil
}

// measureFetchCommand times the setup's own fetch in the clone of repo and
// says what origin's branch is after it. The stamps are bash's
// EPOCHREALTIME (the Bash tool's shell), else date's: both raw, judged by
// parseFetch.
func measureFetchCommand(repo, branch string) string {
	return `now() { if [ -n "$EPOCHREALTIME" ]; then echo "$EPOCHREALTIME"; else date +%s.%N; fi; }; ` +
		`echo "` + probeMarker + ` pwd=$(pwd)"; cd ` + repo + ` && s=$(now) && git fetch --quiet --prune origin; rc=$?; e=$(now); ` +
		`echo "` + probeMarker + ` fetch=start=$s end=$e rc=$rc head=$(git rev-parse origin/` + branch + ` 2>&1 | head -n 1)"`
}

// parseFetch reads the fetch line of a reply: the seconds between its
// stamps, the exit code and the head. A clock of whole seconds measures
// nothing below a second: that is the error.
func parseFetch(reply string) (seconds float64, rc, head string, err error) {
	for _, mm := range probeLine.FindAllStringSubmatch(reply, -1) {
		if mm[1] != "fetch" {
			continue
		}
		value := strings.Trim(strings.TrimSpace(mm[2]), "`")
		start, end := probeValue(value, "start"), probeValue(value, "end")
		if !subSecondStamp(start) || !subSecondStamp(end) {
			return 0, "", "", fmt.Errorf("the Session's sandbox stamps the fetch %q and %q: no clock below a second (neither bash's EPOCHREALTIME nor date's %%N)", start, end)
		}
		s, _ := strconv.ParseFloat(start, 64)
		e, _ := strconv.ParseFloat(end, 64)
		return e - s, probeValue(value, "rc"), probeValue(value, probeHeadKey), nil
	}
	return 0, "", "", fmt.Errorf("the reply carries no %s fetch line: %s", probeMarker, excerpt(reply, 300))
}

// subSecondStamp says whether a stamp is seconds with a fraction that is not
// all zeros.
func subSecondStamp(stamp string) bool {
	whole, frac, ok := strings.Cut(stamp, ".")
	if _, err := strconv.ParseFloat(stamp, 64); err != nil || !ok || whole == "" || frac == "" {
		return false
	}
	return strings.Trim(frac, "0") != ""
}

// onDemandFetch pushes once more and times the setup's fetch command in the
// Session's sandbox, which must bring the pushed head.
func (m *measurement) onDemandFetch(ctx context.Context, w *measureWorkspace, ks *apiv1alpha1.Session) error {
	head, err := m.push(w.pushed)
	if err != nil {
		return err
	}
	reply, _, err := measureTurn(ctx, m.api, ks, measureFetchCommand(w.pushed, w.branch))
	if err != nil {
		return err
	}
	seconds, rc, got, err := parseFetch(reply)
	switch {
	case err != nil:
		w.samples.notAvailable(figOnDemandFetch, err.Error())
		warn("%s: %v", w.name, err)
	case rc != "0" || got != head:
		w.samples.notAvailable(figOnDemandFetch, fmt.Sprintf("the fetch in the Session's sandbox ended %s at %s, not at the pushed %s", rc, got, shortSHA(head)))
		warn("%s: the fetch in the Session's sandbox ended %s at %q, not at the pushed %s", w.name, rc, got, shortSHA(head))
	default:
		w.samples.add(figOnDemandFetch, seconds)
		note("%s: on-demand fetch of %s %s", w.name, w.pushed, formatMeasure(seconds, unitSeconds))
	}
	return nil
}

// measureGitScript times, in the Session's clone of repo on the volume and
// then in a clone of a copy of its mirror on the node's local disk, the
// first checkout (the Harness's `git switch` of the default branch), `git
// status` and a cold `go build ./...`; it reports the session directory's
// size after the checkout. Its lines are `measure key=value`.
func measureGitScript(repo, branch string) string {
	return workspacesGitSetup + `ms() { date +%s%3N; }
c=/workspace/` + repo + `
test -d "$c/.git" || { echo "no clone $c in the session directory"; exit 1; }
alt=$(head -n 1 "$c/.git/objects/info/alternates")
test -d "$alt" || { echo "the clone borrows from $alt, which this pod does not mount"; exit 1; }
b=` + branch + `
if [ -z "$b" ]; then b=$(git -C "$c" rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||'); fi
if [ -z "$b" ] || [ "$b" = HEAD ]; then b=$(git -C "$c" for-each-ref --format='%(refname:lstrip=3)' refs/remotes/origin | grep -v '^HEAD$' | head -n 1); fi
echo "measure branch=$b workers=$(git -C "$c" config checkout.workers) untracked-cache=$(git -C "$c" config core.untrackedCache) many-files=$(git -C "$c" config feature.manyFiles)"
build() { if [ ! -f go.mod ]; then echo none; return; fi; mkdir -p "$1/out"; a=$(ms); GOCACHE="$1/cache" GOPATH="$1/gopath" GOFLAGS=-mod=mod GOPROXY=off GOTOOLCHAIN=local go build -o "$1/out/" ./... >&2; z=$(ms); echo $((z-a)); }
a=$(ms); git -C "$c" switch -q "$b"; z=$(ms); echo "measure checkout-volume=$((z-a))"
a=$(ms); git -C "$c" status --porcelain >/dev/null; z=$(ms); echo "measure status-volume=$((z-a))"
echo "measure session-bytes=$(du -sb /workspace | cut -f1)"
echo "measure build-volume=$(cd "$c" && build /workspace/` + measureBuildDir + `)"
# NFS keeps a file another process still holds as .nfs*: the directory goes
# once it is let go, or with the workspace.
for i in 1 2 3; do rm -rf /workspace/` + measureBuildDir + ` 2>/dev/null && break; sleep 1; done
cp -a "$(dirname "$alt")" /local/mirror.git
git clone -q --shared --no-checkout --config checkout.workers=0 --config core.untrackedCache=true --config feature.manyFiles=true /local/mirror.git /local/clone
git -C /local/clone update-ref -d "$(git -C /local/clone symbolic-ref -q HEAD)"
a=$(ms); git -C /local/clone switch -q "$b"; z=$(ms); echo "measure checkout-local=$((z-a))"
a=$(ms); git -C /local/clone status --porcelain >/dev/null; z=$(ms); echo "measure status-local=$((z-a))"
echo "measure build-local=$(cd /local/clone && build /local/build)"
`
}

// gitWork runs measureGitScript in a pod with the Session's mounts (its
// directory read-write at the working directory, the mirrors read-only) and
// an emptyDir as the local disk.
func (m *measurement) gitWork(w *measureWorkspace, id string) error {
	mounts := []corev1.VolumeMount{
		{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, SubPath: workspacesTestSessions + "/" + id},
		{Name: workspacesTestVolume, MountPath: workspacesActorMirrorsPath, SubPath: workspacesTestMirrors, ReadOnly: true},
		{Name: measureLocalVolume, MountPath: "/" + measureLocalVolume},
	}
	out, err := m.onVolume(w, "git", measureGitScript(w.worked, w.branch), mounts)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "measure "); ok {
			for _, f := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(f, "="); ok {
					values[k] = v
				}
			}
		}
	}
	for _, f := range []struct{ key, fig string }{
		{"checkout-volume", figCheckoutVolume}, {"checkout-local", figCheckoutLocal},
		{"status-volume", figStatusVolume}, {"status-local", figStatusLocal},
		{"build-volume", figBuildVolume}, {"build-local", figBuildLocal},
	} {
		raw := values[f.key]
		if raw == measureNoBuild {
			w.samples.notAvailable(f.fig, w.worked+" defines no Go module to build")
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("the git probe on %s reported %s=%q:\n%s", w.name, f.key, raw, out)
		}
		w.samples.add(f.fig, v/1000)
	}
	bytes, err := strconv.ParseFloat(values["session-bytes"], 64)
	if err != nil {
		return fmt.Errorf("the git probe on %s reported session-bytes=%q:\n%s", w.name, values["session-bytes"], out)
	}
	w.samples.add(figSessionBytes, bytes)
	note("%s: in %s (branch %s): checkout %s ms on the volume, %s ms local; status %s / %s ms; build %s / %s ms; the session directory %s",
		w.name, w.worked, values["branch"], values["checkout-volume"], values["checkout-local"], values["status-volume"], values["status-local"],
		values["build-volume"], values["build-local"], formatBytes(int64(bytes)))
	return nil
}

// measureStorageScript reads the volume's use and its filesystem's size.
const measureStorageScript = `m=$(du -sb /workspace/` + workspacesTestMirrors + ` 2>/dev/null | cut -f1)
s=$(du -sb /workspace/` + workspacesTestSessions + ` 2>/dev/null | cut -f1)
echo "measure mirrors=${m:-0} sessions=${s:-0} $(df -B1 /workspace | tail -n 1 | awk '{print "fs-size=" $2 " fs-avail=" $4}')"
`

// storage reads the workspace's standing storage after the Session's work;
// the last run's is the report's.
func (m *measurement) storage(w *measureWorkspace, last bool) error {
	if !last {
		return nil
	}
	out, err := m.onVolume(w, "storage", measureStorageScript, nil)
	if err != nil {
		return err
	}
	num := func(key string) int64 {
		v, _ := strconv.ParseInt(probeValue(out, key), 10, 64)
		return v
	}
	s := &w.storage
	s.MirrorsBytes, s.SessionsBytes, s.FilesystemBytes, s.FilesystemAvailBytes = num("mirrors"), num("sessions"), num("fs-size"), num("fs-avail")
	claim, expands, provisioner, err := m.claimAndClass(w)
	if err != nil {
		return err
	}
	s.StorageClass = ptrOr(claim.Spec.StorageClassName, "")
	s.ProvisionsSize = expands != nil && *expands
	s.Driver = provisioner
	if driver := m.pvDriver(w); driver != "" {
		s.Driver = driver
	}
	if c, ok := claim.Status.Capacity[corev1.ResourceStorage]; ok {
		v := c.Value()
		s.CapacityBytes = &v
	} else {
		s.CapacityNotAvailable = "the claim reports no capacity"
	}
	why := s.CapacityNotAvailable
	if !s.ProvisionsSize {
		why = "the StorageClass provisions no size (elastic): its capacity is nominal, the headroom is not sized (W8)"
		s.CapacityBytes = nil
		s.CapacityNotAvailable = why
	}
	d, err := m.get(w.name)
	if err != nil {
		return err
	}
	s.NeededSize, s.VolumeSize = cmpOr(d.Storage.NeededSize, d.Workspace.NeededSize), cmpOr(d.Storage.VolumeSize, d.Workspace.VolumeSize)
	s.Mirrors = nil
	for _, repo := range w.repos {
		var mirrorBytes int64
		if mm := d.mirror(repo); mm != nil {
			mirrorBytes = mm.SizeBytes
		}
		s.Mirrors = append(s.Mirrors, judgeMirror(repo, w.providerKiB[repo], mirrorBytes))
	}
	s.Headroom = judgeHeadroom(s.MirrorsBytes+s.SessionsBytes, s.CapacityBytes, why)
	note("%s: StorageClass %s (%s), capacity %v, mirrors %s, session directories %s", w.name, s.StorageClass, s.Driver, capacityLine(s), formatBytes(s.MirrorsBytes), formatBytes(s.SessionsBytes))
	return nil
}

func capacityLine(s *measureStorage) string {
	if s.CapacityBytes == nil {
		return "not available"
	}
	return formatBytes(*s.CapacityBytes)
}

// pvDriver is the CSI driver of the workspace's PersistentVolume.
func (m *measurement) pvDriver(w *measureWorkspace) string {
	k, err := labKube()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pv, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, w.volume.pv, metav1.GetOptions{})
	if err != nil || pv.Spec.CSI == nil {
		return ""
	}
	return pv.Spec.CSI.Driver
}

// onVolume runs a probe on the git image with the workspace's volume:
// mounted whole at /workspace without mounts, else as they say (an
// emptyDir "local" is added when they name it).
func (m *measurement) onVolume(w *measureWorkspace, probe, script string, mounts []corev1.VolumeMount) (string, error) {
	pod := probePod("agentlab-measure-"+probe, workspacesTestGitImage, []string{"sh", shExitOnError, script})
	pod.Spec.Volumes = []corev1.Volume{{Name: workspacesTestVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: w.volume.claim},
	}}}
	if mounts == nil {
		mounts = []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath}}
	}
	if slices.ContainsFunc(mounts, func(v corev1.VolumeMount) bool { return v.Name == measureLocalVolume }) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: measureLocalVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	pod.Spec.Containers[0].VolumeMounts = mounts
	out, err := runPod(context.Background(), w.volume.namespace, pod, 3*m.timeout)
	if err != nil {
		return "", fmt.Errorf("the volume probe %s on workspace %s: %w\n%s", probe, w.name, err, out)
	}
	return strings.TrimSpace(out), nil
}

// deleteAll deletes the run's Sessions and workspaces.
func (m *measurement) deleteAll() error {
	if m.set != nil {
		if err := m.set.removeAllNow(); err != nil {
			return err
		}
	}
	for _, w := range m.ws {
		if err := m.deleteWorkspace(w); err != nil {
			return err
		}
	}
	return nil
}

// deleteWorkspace clears the workspace's session directories, deletes it
// through delete_workspace and waits for it, its claim and its volume to go.
func (m *measurement) deleteWorkspace(w *measureWorkspace) error {
	v, err := m.volume(w.name)
	if err == nil {
		w.volume = v
		if _, err := m.onVolume(w, "clear", accessClearSessionsScript, nil); err != nil {
			return err
		}
	}
	var res struct {
		Sessions []string `json:"sessions"`
	}
	if err := m.person.callServerStructured(workspaceManagerToolPrefix+"delete_workspace", map[string]any{wmWorkspaceArg: w.name}, &res); err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	var state string
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, m.timeout, true, func(ctx context.Context) (bool, error) {
		if gone, err := m.gone(w.name); err != nil || !gone {
			state = "the workspace is still listed"
			return false, nil
		}
		if v.claim == "" {
			return true, nil
		}
		if _, err := k.clientset.CoreV1().PersistentVolumeClaims(v.namespace).Get(ctx, v.claim, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			state = "its claim " + v.claim + " is still there"
			return false, nil
		}
		if _, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, v.pv, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			state = "its PersistentVolume " + v.pv + " is still there"
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("workspace %s is not gone %s after delete_workspace: %s", w.name, m.timeout, state)
	}
	m.created = slices.DeleteFunc(m.created, func(n string) bool { return n == w.name })
	w.lastSync = nil
	note("deleted %s: the workspace, its claim and its volume %s gone", w.name, cmpOr(v.pv, "(none claimed)"))
	return nil
}

// cleanup removes what the measurement created on every path, best effort
// past the first failure: the Sessions, the workspaces, the Agent, the
// person's sign-in at the lab's GitHub, the pushes and their clones, the
// generated repository.
func (m *measurement) cleanup() {
	var errs []error
	if m.set != nil {
		errs = append(errs, m.set.removeAllNow())
	}
	if m.person != nil {
		for _, w := range m.ws {
			if slices.Contains(m.created, w.name) {
				errs = append(errs, m.deleteWorkspace(w))
			}
		}
	}
	if m.applied && agentExists(measureAgent) {
		errs = append(errs, removeAgent(measureAgent))
	}
	if m.signedIn != nil {
		m.signedIn.disconnect(config.WorkspaceProviderFake)
	}
	for _, pu := range m.pushers {
		if pu.start != "" && pu.pushes > 0 && pu.repo != m.generated {
			_, err := m.gh.git(pu.dir, pu.token, githubPush, "-q", "--force", gitOrigin, pu.start+":refs/heads/"+measureLabBranch)
			errs = append(errs, err)
		}
		errs = append(errs, os.RemoveAll(filepath.Dir(pu.dir)))
	}
	if m.generated != "" {
		errs = append(errs, m.gh.removeGeneratedRepo(m.generated))
	}
	if m.api != nil {
		m.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the measurement left: %v", err)
		return
	}
	note("removed what the measurement created: the Sessions, the workspaces and their volumes, the Agent, the pushes")
}

// writeReport writes the report's JSON and Markdown to the run directory
// and prints the Markdown.
func (m *measurement) writeReport() error {
	for _, w := range m.ws {
		m.report.Workspaces = append(m.report.Workspaces, measureWorkspaceReport{
			Name: w.name, Kind: w.kind, Repositories: w.repos, Pushed: w.pushed, Worked: w.worked,
			Figures: w.samples.figures(), Storage: w.storage,
		})
	}
	raw, err := json.MarshalIndent(m.report, "", "  ")
	if err != nil {
		return err
	}
	md := m.report.markdown()
	jsonPath, mdPath := filepath.Join(m.runDir, measureReportJSON), filepath.Join(m.runDir, measureReportMarkdown)
	if err := os.WriteFile(jsonPath, raw, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(mdPath, []byte(md), 0o600); err != nil {
		return err
	}
	fmt.Printf("\n%s\nReport: %s and %s\n", md, jsonPath, mdPath)
	return nil
}
