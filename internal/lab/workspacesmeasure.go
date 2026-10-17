package lab

import (
	"cmp"
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
// platform, as the person signed in there (`muster auth login`), its pushes
// going to a scratch branch on the provider with the person's own git
// credentials (the manager notices a push to any branch and mirrors every
// ref), the branch deleted afterwards. The person's token is renewed before it expires (personToken), so a
// measurement of many runs outlives it and its cleanup deletes every Session
// it started.

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
	// measureScratchPrefix starts the scratch branch the pushes go to on
	// another cluster's provider, followed by the run's start time.
	measureScratchPrefix = "agentlab-workspaces-measure-"
	// measureGitURL is the provider's git base URL on another cluster.
	measureGitURL = "https://github.com"
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
	// measureVendored is its word for a module that vendors its
	// dependencies: nothing to download.
	measureVendored = "vendored"
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
	// GoProxy is the module proxy the build's download goes through; "" is
	// the git image's (Go's default, proxy.golang.org).
	GoProxy string
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
	// GitURL is the provider's git base URL the pushes go to,
	// <GitURL>/<owner>/<name>.git; "" is https://github.com.
	GitURL string
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
	case strings.ContainsAny(o.GoProxy, "'\\ \t\n"):
		return fmt.Errorf("--goproxy %q: a GOPROXY list, comma- or pipe-separated, without quotes or spaces", o.GoProxy)
	}
	labOnly := map[string]bool{"[email]": o.Email != "", "--large-repo-mib": o.LargeRepoMiB != 0}
	remoteOnly := map[string]bool{"--organization": o.Organization != "", "--provider": o.Provider != "", "--small-repos": len(o.SmallRepos) > 0,
		"--large-repo": o.LargeRepo != "", "--agentgateway-url": o.AgentgatewayURL != "", "--agent": o.Agent != "", "--git-url": o.GitURL != ""}
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
			return fmt.Errorf("%s is the lab's only: drop it with --kube-context and --muster-url, where the person is the one signed in to its muster and the repositories are named (--small-repos, --large-repo)", name)
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
	// cannot push, noPush says why); worked the one the checkout, status
	// and build run in.
	pushed, noPush, worked string
	branch                 string
	volume                 syncVolume
	lastSync               *syncLastSync
	providerKiB            map[string]int64
	samples                *measureSamples
	storage                measureStorage
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

// measurePusher pushes commits to a repository from a shallow clone on this
// host: in the lab to its default branch, put back at the start at the end;
// on another cluster to a scratch branch, deleted at the end.
type measurePusher struct {
	repo, dir, start string
	// branch is the branch the pushes go to.
	branch  string
	scratch bool
	git     func(dir string, args ...string) (string, error)
	pushes  int
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
	token     *personToken     // the person's, renewed for every call to muster and kagent
	signedIn  *workspacePerson // the lab's person, connected to the provider
	api       *kagentAPI
	set       *sessionSet
	started   int // the Sessions start_session started
	agentNS   string
	agentName string
	sessions  string // why the Session figures are not available, "" when they are
	applied   bool   // the lab's Agent is the measurement's
	gh        *labGitHubClient
	pushers   map[string]*measurePusher
	generated string
	listedAt  map[string]time.Time // owner → the manager's last listing of it
	ws        []*measureWorkspace
	runNo     int // the run being measured, for a failed probe's record
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
		end, err := startWorkspaceRun()
		defer end()
		if err != nil {
			return err
		}
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
		m.runNo = run
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
			w.samples.notAvailable(figChangedCycle, w.noPush)
			w.samples.notAvailable(figOnDemandFetch, w.noPush)
		}
		if m.sessions != "" {
			for _, key := range []string{figSessionStart, figSessionSetup, figOnDemandFetch, figSessionBytes} {
				w.samples.notAvailable(key, m.sessions)
			}
			for _, f := range measureGitFigures {
				w.samples.notAvailable(f.fig, m.sessions)
			}
		}
		if err := m.leftover(w); err != nil {
			return err
		}
	}
	m.report.WorkspaceManager = workspaceManagerImage()
	if m.report.WorkspaceManager != "" {
		note("workspace-manager %s", m.report.WorkspaceManager)
	}
	return nil
}

// leftover removes the workspace an interrupted run left under w's name. One
// still deleting waits on its manager's next deletion cycle, which nothing
// brings forward: its session directories are cleared, so that cycle lets
// it go, and the run measures under a fresh name.
func (m *measurement) leftover(w *measureWorkspace) error {
	gone, err := m.gone(w.name)
	if err != nil || gone {
		return err
	}
	d, err := m.get(w.name)
	if err != nil {
		return err
	}
	if !d.Workspace.Deleting {
		note("removing the leftover workspace %s of an interrupted run", w.name)
		m.created = append(m.created, w.name)
		return m.deleteWorkspace(w)
	}
	if _, err := m.clearSessions(w); err != nil {
		return err
	}
	fresh := measureFreshName(w.name, m.report.Started)
	if gone, err := m.gone(fresh); err != nil || !gone {
		return cmp.Or(err, fmt.Errorf("the fresh workspace name %s for the leftover %s is taken", fresh, w.name))
	}
	note("the leftover workspace %s of an interrupted run is still deleting: its session directories cleared, its manager deletes it at its next deletion cycle; this run measures as %s", w.name, fresh)
	m.report.Leftovers = append(m.report.Leftovers, measureLeftover{Name: w.name, MeasuredAs: fresh})
	w.name, w.volume = fresh, syncVolume{}
	return nil
}

// measureFreshName is the name a run measures under while the workspace of
// an interrupted run is still deleting.
func measureFreshName(name string, started time.Time) string {
	return name + "-" + started.UTC().Format("20060102-150405")
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
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
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
	m.token = labPersonToken(func() (string, error) {
		return passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	})
	m.person = m.signedIn.session
	m.person.renew = m.token.accessToken
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
		owner, _, _ := strings.Cut(w.pushed, "/")
		token, err := m.gh.installationToken(owner)
		if err != nil {
			return fmt.Errorf("the measurement pushes to %s as the App's installation on %s: %w", w.pushed, owner, err)
		}
		pu := &measurePusher{repo: w.pushed, branch: measureLabBranch, git: func(dir string, args ...string) (string, error) { return m.gh.git(dir, token, args...) }}
		if err := m.clonePusher(pu, m.gh.remote(w.pushed)); err != nil {
			return err
		}
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the probes' git image (%s)", res.d)
	}
	return m.labAgent()
}

// labAgent readies the measurement's Agent on the claude Harness and the
// kagent API as the person.
func (m *measurement) labAgent() error {
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
	if m.api, err = dialKagentAPIAs(cfg.AgentgatewayBaseURL(), m.token); err != nil {
		return err
	}
	m.set = &sessionSet{api: m.api, manager: m.person}
	note("the Agent %s Ready on Harness %s in %s", measureAgent, claudeHarness, boot.elapsed.Round(time.Second))
	return nil
}

// remotePreconditions is another cluster's side: the person's tokens from
// their muster sign-in, the named repositories.
func (m *measurement) remotePreconditions() error {
	o := m.opts
	m.token = musterPersonToken(o.MusterURL)
	access, err := m.token.accessToken()
	if err != nil {
		return err
	}
	if m.person, err = openMusterSessionAt(o.MusterURL, access, "workspaces-measure"); err != nil {
		return err
	}
	m.person.renew = m.token.accessToken
	note("signed in to %s with the person's muster session; Kubernetes reads through context %s", o.MusterURL, o.KubeContext)
	m.ws = []*measureWorkspace{
		{name: measureSmallWorkspace, kind: measureSmall, repos: o.SmallRepos, worked: o.SmallRepos[0], branch: ""},
		{name: measureLargeWorkspace, kind: measureLarge, repos: []string{o.LargeRepo}, worked: o.LargeRepo, branch: ""},
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	scratch := measureScratchPrefix + m.report.Started.Format("20060102-150405")
	gitURL := strings.TrimSuffix(cmp.Or(o.GitURL, measureGitURL), "/")
	for _, w := range m.ws {
		pu := &measurePusher{repo: w.worked, branch: scratch, scratch: true, git: personGit}
		if err := m.clonePusher(pu, gitURL+"/"+w.worked+".git"); err != nil {
			// The person may not push there: the run goes on without the
			// two figures that need a push.
			w.noPush = fmt.Sprintf("no push to %s with your git credentials: %s", w.worked, excerpt(err.Error(), 300))
			warn("%s: %s", w.name, w.noPush)
			continue
		}
		w.pushed = w.worked
	}
	if o.Agent == "" {
		m.sessions = "no --agent and --agentgateway-url to start a Session on this cluster with"
		return nil
	}
	if m.api, err = dialKagentAPIAs(o.AgentgatewayURL, m.token); err != nil {
		return err
	}
	m.set = &sessionSet{api: m.api, manager: m.person}
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

// clonePusher makes a shallow clone of the pusher's repository from url on
// this host, without a working tree or file contents, to push from.
func (m *measurement) clonePusher(pu *measurePusher, url string) error {
	dir, err := os.MkdirTemp(StateDir, "workspaces-measure-push-")
	if err != nil {
		return err
	}
	// git runs in the clone: every path it is handed is absolute.
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	pu.dir = filepath.Join(dir, "clone")
	m.pushers[pu.repo] = pu
	if _, err := pu.git(dir, "clone", "-q", "--depth", "1", "--filter=blob:none", "--no-checkout", "--single-branch", url, "clone"); err != nil {
		return err
	}
	if pu.start, err = pu.git(pu.dir, "rev-parse", "HEAD"); err != nil {
		return err
	}
	note("%s cloned on this host at %s, shallow, to push to %s", pu.repo, shortSHA(pu.start), pu.branch)
	return nil
}

// push commits on the clone's head, the tree unchanged, and pushes it to the
// pusher's branch, returning the new head.
func (m *measurement) push(repo string) (string, error) {
	pu := m.pushers[repo]
	pu.pushes++
	msg := fmt.Sprintf("agentlab workspaces-measure: push %d at %s", pu.pushes, time.Now().UTC().Format(time.RFC3339Nano))
	head, err := pu.git(pu.dir, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", msg)
	if err != nil {
		return "", err
	}
	if _, err := pu.git(pu.dir, "update-ref", "HEAD", head); err != nil {
		return "", err
	}
	if _, err := pu.git(pu.dir, githubPush, "-q", gitOrigin, "HEAD:refs/heads/"+pu.branch); err != nil {
		return "", err
	}
	return head, nil
}

// unpush removes the pushes: the scratch branch deleted, the lab's default
// branch put back at the start.
func (pu *measurePusher) unpush() error {
	if pu.start == "" || pu.pushes == 0 {
		return nil
	}
	if pu.scratch {
		_, err := pu.git(pu.dir, githubPush, "-q", gitOrigin, "--delete", "refs/heads/"+pu.branch)
		return err
	}
	_, err := pu.git(pu.dir, githubPush, "-q", "--force", gitOrigin, pu.start+":refs/heads/"+pu.branch)
	return err
}

// personGit runs git with the person's own configuration and credentials,
// never prompting.
func personGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...) // #nosec G204 -- git with the measurement's own arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	return strings.TrimSpace(string(out)), nil
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
	create := map[string]any{nameKey: w.name, wmOrganizationArg: m.org, wmSourcesArg: m.sources(w)}
	var err error
	if m.opts.remote() {
		// An installation's Workspace carries no lab label.
		err = m.person.callServerStructured(workspaceManagerToolPrefix+"create_workspace", create, &created)
	} else {
		err = m.person.createLabWorkspace(create, &created)
	}
	if err != nil {
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
	claims, err := m.claims(name)
	if err != nil {
		return syncVolume{}, err
	}
	v, ok := boundVolume(claims)
	if !ok {
		return syncVolume{}, fmt.Errorf("workspace %s has %d claims labelled %s=%s, want one bound", name, len(claims), syncWorkspaceLabel, name)
	}
	return v, nil
}

// boundVolume is the workspace's volume when its claims are one bound claim.
func boundVolume(claims []corev1.PersistentVolumeClaim) (syncVolume, bool) {
	if len(claims) != 1 || claims[0].Spec.VolumeName == "" {
		return syncVolume{}, false
	}
	c := claims[0]
	return syncVolume{namespace: c.Namespace, claim: c.Name, pv: c.Spec.VolumeName}, true
}

// claims are the claims labelled with the workspace's name.
func (m *measurement) claims(name string) ([]corev1.PersistentVolumeClaim, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	list, err := k.clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{LabelSelector: syncWorkspaceLabel + "=" + name})
	if err != nil {
		return nil, fmt.Errorf("listing the claim of workspace %s: %w", name, err)
	}
	return list.Items, nil
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
		if m.pushers[w.pushed].scratch {
			// The manifest names the default branch's head: the scratch
			// branch's push shows as a cycle that changed the mirror.
			if r := d.Workspace.LastSync; mirror == nil || r.Result != syncResultSucceed {
				return fmt.Errorf("after a push to %s the cycle of %s ended %s with mirror %v, want %s", w.pushed, w.name, r.Result, mirror, syncResultSucceed)
			}
		} else if mirror == nil || mirror.Head != head {
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
		nameKey: m.agentName, namespaceKey: m.agentNS, startSessionRequestIDArg: uuid.NewString(), startSessionGrantArg: grant,
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
	m.started++
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
	owner := m.sessionOwner(w, id)
	if record, err := m.setupRecord(w, id, owner); err != nil {
		m.probeFailed(w, "dirs", err, figSessionStart, figSessionSetup)
	} else {
		w.samples.since(figSessionStart, startCalled, record.SetUp)
		w.samples.since(figSessionSetup, sent, record.SetUp)
		note("%s: Session %s works in %s (%s); start_session to the directory set up %s, the first turn sent to set up %s; cloned %s, fetched %s",
			w.name, id, first.pwd, headsLine(first.heads), record.SetUp.Sub(startCalled).Round(10*time.Millisecond), record.SetUp.Sub(sent).Round(10*time.Millisecond),
			strings.Join(record.Cloned, ", "), cmpOr(strings.Join(record.Fetched, ", "), "nothing"))
		for _, problem := range record.Problems {
			warn("%s: the setup's problem: %s: %s", w.name, problem.Repository, excerpt(problem.Reason, 300))
		}
	}
	if w.pushed != "" {
		if err := m.onDemandFetch(ctx, w, ks); err != nil {
			return err
		}
	}
	m.gitWork(w, id, owner)
	if err := m.storage(w, last); err != nil {
		return err
	}
	return m.set.remove(ctx, id)
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

// setupRecord reads the setup's record of the Session's directory as its
// owner.
func (m *measurement) setupRecord(w *measureWorkspace, id string, owner volumeOwner) (*workspaceState, error) {
	out, err := m.onVolume(w, "dirs", sessionsDirectoriesScript, nil, owner)
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
	reply, _, err := measureTurn(ctx, m.api, ks, measureFetchCommand(w.pushed, m.pushers[w.pushed].branch))
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

// measureBuildFunction is the git probe's `build <where> <dir>`: in the
// working directory's Go module, with a cold module and build cache in dir,
// the download of every module the build needs (through goproxy, "" the
// image's) and then the compile with the proxy off, each its own figure, so
// the volume against the local disk compares the compile. A vendored module
// downloads nothing. Its lines are `measure <figure>-<where>=<ms|none|vendored|fail>`
// and, for a failure, `measure-why <figure>-<where> <the last error line>`.
// It never fails the script: a failed build is a figure not available.
func measureBuildFunction(goproxy string) string {
	proxy := ""
	if goproxy != "" {
		proxy = "export GOPROXY='" + goproxy + "'; "
	}
	return `buildfail() { echo "measure $1=fail"; tail -n 20 "$2" >&2; echo "measure-why $1 $(grep -v '^go: downloading' "$2" | grep . | tail -n 1)"; }
build() (
  w=$1; d=$2
  if [ ! -f go.mod ]; then echo "measure download-$w=` + measureNoBuild + `"; echo "measure build-$w=` + measureNoBuild + `"; exit 0; fi
  mkdir -p "$d/out"
  export GOCACHE="$d/cache" GOPATH="$d/gopath" GOTOOLCHAIN=local GOFLAGS="-mod=mod -modcacherw"
  if [ -f vendor/modules.txt ]; then
    export GOFLAGS="-mod=vendor"; echo "measure download-$w=` + measureVendored + `"
  else
    ` + proxy + `a=$(ms)
    if ! go list -deps ./... >/dev/null 2>"$d/err"; then
      buildfail "download-$w" "$d/err"; echo "measure build-$w=fail"; echo "measure-why build-$w the download failed"; exit 0
    fi
    z=$(ms); echo "measure download-$w=$((z-a))"
  fi
  export GOPROXY=off
  a=$(ms)
  if ! go build -o "$d/out/" ./... 2>"$d/err"; then buildfail "build-$w" "$d/err"; exit 0; fi
  z=$(ms); echo "measure build-$w=$((z-a))"
)
`
}

// measureGitScript times, in the Session's clone of repo on the volume and
// then in a clone of a copy of its mirror on the node's local disk, the
// first checkout (the Harness's `git switch` of the default branch), `git
// status` and a cold build (measureBuildFunction); it reports the session
// directory's size after the checkout. Its lines are `measure key=value`.
func measureGitScript(repo, branch, goproxy string) string {
	return workspacesGitSetup + `ms() { date +%s%3N; }
` + measureBuildFunction(goproxy) + `c=/workspace/` + repo + `
test -d "$c/.git" || { echo "no clone $c in the session directory"; exit 1; }
alt=$(head -n 1 "$c/.git/objects/info/alternates")
test -d "$alt" || { echo "the clone borrows from $alt, which this pod does not mount"; exit 1; }
b=` + branch + `
if [ -z "$b" ]; then b=$(git -C "$c" rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||'); fi
if [ -z "$b" ] || [ "$b" = HEAD ]; then b=$(git -C "$c" for-each-ref --format='%(refname:lstrip=3)' refs/remotes/origin | grep -v '^HEAD$' | head -n 1); fi
echo "measure branch=$b workers=$(git -C "$c" config checkout.workers) untracked-cache=$(git -C "$c" config core.untrackedCache) many-files=$(git -C "$c" config feature.manyFiles)"
a=$(ms); git -C "$c" switch -q "$b"; z=$(ms); echo "measure checkout-volume=$((z-a))"
a=$(ms); git -C "$c" status --porcelain >/dev/null; z=$(ms); echo "measure status-volume=$((z-a))"
echo "measure session-bytes=$(du -sb /workspace | cut -f1)"
(cd "$c" && build volume /workspace/` + measureBuildDir + `)
# NFS keeps a file another process still holds as .nfs*: the directory goes
# once it is let go, or with the workspace.
for i in 1 2 3; do rm -rf /workspace/` + measureBuildDir + ` 2>/dev/null && break; sleep 1; done
cp -a "$(dirname "$alt")" /local/mirror.git
git clone -q --shared --no-checkout --config checkout.workers=0 --config core.untrackedCache=true --config feature.manyFiles=true /local/mirror.git /local/clone
git -C /local/clone update-ref -d "$(git -C /local/clone symbolic-ref -q HEAD)"
a=$(ms); git -C /local/clone switch -q "$b"; z=$(ms); echo "measure checkout-local=$((z-a))"
a=$(ms); git -C /local/clone status --porcelain >/dev/null; z=$(ms); echo "measure status-local=$((z-a))"
(cd /local/clone && build local /local/build)
`
}

// measureGitFigures are the git probe's keys and their figures.
var measureGitFigures = []struct{ key, fig string }{
	{"checkout-volume", figCheckoutVolume}, {"checkout-local", figCheckoutLocal},
	{"status-volume", figStatusVolume}, {"status-local", figStatusLocal},
	{"download-volume", figDownloadVolume}, {"download-local", figDownloadLocal},
	{"build-volume", figBuildVolume}, {"build-local", figBuildLocal},
	{"session-bytes", figSessionBytes},
}

// measureGitProbe is what the git probe reported: a figure's value, or why
// it has none.
type measureGitProbe struct {
	values map[string]string
	why    map[string]string
}

// parseGitProbe reads the git probe's `measure` and `measure-why` lines.
func parseGitProbe(out string) measureGitProbe {
	p := measureGitProbe{values: map[string]string{}, why: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "measure-why "); ok {
			k, reason, _ := strings.Cut(rest, " ")
			p.why[k] = strings.TrimSpace(reason)
		} else if rest, ok := strings.CutPrefix(line, "measure "); ok {
			for _, f := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(f, "="); ok {
					p.values[k] = v
				}
			}
		}
	}
	return p
}

// value is a figure's value in its unit (the probe's milliseconds as
// seconds), or why it is not available.
func (p measureGitProbe) value(key, worked string) (float64, string) {
	raw, ok := p.values[key]
	switch {
	case !ok:
		return 0, "the git probe reported no " + key
	case raw == measureNoBuild:
		return 0, worked + " defines no Go module at its root to build"
	case raw == measureVendored:
		return 0, worked + " vendors its modules: the build downloads nothing"
	case raw == "fail":
		return 0, "the build failed: " + cmpOr(p.why[key], "no error line")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Sprintf("the git probe reported %s=%q", key, raw)
	}
	if key == "session-bytes" {
		return v, ""
	}
	return v / 1000, ""
}

// gitWork runs measureGitScript in a pod with the Session's mounts (its
// directory read-write at the working directory, the mirrors read-only) and
// an emptyDir as the local disk, as the directory's owner. Every figure the probe reports is kept; one
// it cannot report is not available with the reason, and a failed probe is
// the report's, never the run's end.
func (m *measurement) gitWork(w *measureWorkspace, id string, owner volumeOwner) {
	mounts := []corev1.VolumeMount{
		{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, SubPath: workspacesTestSessions + "/" + id},
		{Name: workspacesTestVolume, MountPath: workspacesActorMirrorsPath, SubPath: workspacesTestMirrors, ReadOnly: true},
		{Name: measureLocalVolume, MountPath: "/" + measureLocalVolume},
	}
	out, err := m.onVolume(w, "git", measureGitScript(w.worked, w.branch, m.opts.GoProxy), mounts, owner)
	if err != nil {
		// The probe's output up to its failure still carries figures.
		out = err.Error()
		m.failed(w, "git", err)
	}
	p := parseGitProbe(out)
	for _, f := range measureGitFigures {
		v, why := p.value(f.key, w.worked)
		if why != "" {
			if err != nil {
				why = "the git probe failed"
			}
			w.samples.notAvailable(f.fig, why)
			continue
		}
		w.samples.add(f.fig, v)
	}
	for _, key := range []string{"download-volume", "build-volume", "download-local", "build-local"} {
		if p.values[key] == "fail" {
			m.failed(w, "git ("+key+")", errors.New(cmpOr(p.why[key], "no error line")))
		}
	}
	note("%s: in %s (branch %s): checkout %s ms on the volume, %s ms local; status %s / %s ms; download %s / %s ms; compile %s / %s ms; the session directory %s B",
		w.name, w.worked, p.values["branch"], p.values["checkout-volume"], p.values["checkout-local"], p.values["status-volume"], p.values["status-local"],
		p.values["download-volume"], p.values["download-local"], p.values["build-volume"], p.values["build-local"], p.values["session-bytes"])
}

// failed records a failed probe in the report and warns.
func (m *measurement) failed(w *measureWorkspace, probe string, err error) {
	reason := excerpt(err.Error(), 400)
	m.report.Failed = append(m.report.Failed, measureFailedProbe{Run: m.runNo, Workspace: w.name, Probe: probe, Reason: reason})
	warn("run %d, %s: the %s probe failed, its figures of this run are not available: %s", m.runNo, w.name, probe, reason)
}

// probeFailed records a failed probe and its figures as not available.
func (m *measurement) probeFailed(w *measureWorkspace, probe string, err error, figs ...string) {
	m.failed(w, probe, err)
	for _, f := range figs {
		w.samples.notAvailable(f, "the "+probe+" probe failed")
	}
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
	out, err := m.onVolume(w, "storage", measureStorageScript, nil, workspaceVolumeOwner)
	if err != nil {
		// The standing storage stays unread: the report says so.
		m.failed(w, "storage", err)
		return nil
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

// measureVolumePod is onVolume's pod: a workspaceVolumePod running as owner
// (its fsGroup stays the sync Job's) with the claim mounted whole at
// /workspace without mounts, else as they say (an emptyDir "local" is added
// when they name it).
func measureVolumePod(claim, probe, script string, mounts []corev1.VolumeMount, owner volumeOwner) *corev1.Pod {
	pod := workspaceVolumePod("agentlab-measure-"+probe, claim, script)
	pod.Spec.SecurityContext.RunAsUser, pod.Spec.SecurityContext.RunAsGroup = &owner.uid, &owner.gid
	if mounts == nil {
		return pod
	}
	if slices.ContainsFunc(mounts, func(v corev1.VolumeMount) bool { return v.Name == measureLocalVolume }) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: measureLocalVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	pod.Spec.Containers[0].VolumeMounts = mounts
	return pod
}

// onVolume runs a probe on the git image with the workspace's volume, as
// measureVolumePod mounts it, as owner.
func (m *measurement) onVolume(w *measureWorkspace, probe, script string, mounts []corev1.VolumeMount, owner volumeOwner) (string, error) {
	pod := measureVolumePod(w.volume.claim, probe, script, mounts, owner)
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

// sessionOwners reads the owner of every session directory on the
// workspace's volume: the sessions directory itself is readable by the sync
// Job's user, a Session's directory only by its owner where the volume keeps
// the runtime's owner and mode (Azure Files over NFS).
func (m *measurement) sessionOwners(w *measureWorkspace) (map[string]volumeOwner, error) {
	out, err := m.onVolume(w, "owners", sessionOwnersScript, nil, workspaceVolumeOwner)
	if err != nil {
		return nil, err
	}
	return parseSessionOwners(out), nil
}

// sessionOwner is the user a probe entering the Session's directory runs
// as: the directory's owner, else (unread, or root, whom the restricted
// profile refuses) the sync Job's user.
func (m *measurement) sessionOwner(w *measureWorkspace, id string) volumeOwner {
	owners, err := m.sessionOwners(w)
	if err != nil {
		warn("%s: the owner of the directory of Session %s is unread, the probes run as the sync Job's user: %s", w.name, id, excerpt(err.Error(), 300))
		return workspaceVolumeOwner
	}
	return owners[id].runnable()
}

// clearSessions removes the session directories from the workspace's
// volume, each as its owner, and returns the volume; a workspace without a
// bound claim has none to clear.
func (m *measurement) clearSessions(w *measureWorkspace) (syncVolume, error) {
	claims, err := m.claims(w.name)
	if err != nil {
		return syncVolume{}, err
	}
	v, bound := boundVolume(claims)
	if !bound {
		return syncVolume{}, nil
	}
	w.volume = v
	owners, err := m.sessionOwners(w)
	if err != nil {
		return syncVolume{}, err
	}
	for _, o := range distinctOwners(owners) {
		out, err := m.onVolume(w, "clear", measureClearSessionsScript(o), nil, o.runnable())
		if err != nil {
			return syncVolume{}, err
		}
		note("%s: %s", w.name, out)
	}
	return v, nil
}

// deleteWorkspace clears the workspace's session directories, deletes it
// through delete_workspace and waits for it, its claim and its volume to go.
func (m *measurement) deleteWorkspace(w *measureWorkspace) error {
	v, err := m.clearSessions(w)
	if err != nil {
		return err
	}
	answered, err := m.requestDelete(w.name)
	if err != nil {
		return err
	}
	if err := m.awaitDeleted(w.name, v, answered); err != nil {
		return err
	}
	m.created = slices.DeleteFunc(m.created, func(n string) bool { return n == w.name })
	w.lastSync = nil
	note("deleted %s: the workspace, its claim and its volume %s gone", w.name, cmpOr(v.pv, "(none claimed)"))
	return nil
}

// requestDelete calls delete_workspace and says whether it answered. An
// empty call_tool answer is no verdict: muster can cut a long delete that
// runs on in the manager, so get_workspace decides (awaitDeleted).
func (m *measurement) requestDelete(name string) (answered bool, err error) {
	var res struct {
		Sessions []string `json:"sessions"`
	}
	err = m.person.callServerStructured(workspaceManagerToolPrefix+"delete_workspace", map[string]any{wmWorkspaceArg: name}, &res)
	if errors.Is(err, errEmptyToolAnswer) {
		warn("%s: delete_workspace answered nothing through muster; get_workspace decides within %s", name, m.timeout)
		return false, nil
	}
	return err == nil, err
}

// awaitDeleted waits up to the measurement's timeout for get_workspace to
// answer not found and, when the workspace claimed a volume, for its claim
// and volume to go.
func (m *measurement) awaitDeleted(name string, v syncVolume, answered bool) error {
	var k *kubeClients
	if v.claim != "" {
		var err error
		if k, err = labKube(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	var state string
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, m.timeout, true, func(ctx context.Context) (bool, error) {
		if gone, err := m.gone(name); err != nil || !gone {
			state = "get_workspace still answers it"
			if err != nil {
				state = "get_workspace failed: " + excerpt(err.Error(), 300)
			}
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
		call := "delete_workspace"
		if !answered {
			call = "delete_workspace (which answered nothing through muster)"
		}
		return fmt.Errorf("workspace %s is not gone %s after %s: %s", name, m.timeout, call, state)
	}
	return nil
}

// cleanup removes what the measurement created on every path, best effort
// past the first failure: the Sessions, the workspaces, the Agent, the
// person's sign-in at the lab's GitHub, the pushes and their clones, the
// generated repository.
func (m *measurement) cleanup() {
	var errs []error
	if m.set != nil {
		left := slices.Clone(m.set.ids)
		if err := m.set.removeAllNow(); err != nil {
			// The report names every Session still there, for a person to
			// delete.
			m.report.SessionsLeft = m.set.ids
			errs = append(errs, err)
		} else if len(left) > 0 {
			note("deleted the Sessions the measurement left: %s", strings.Join(left, ", "))
		}
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
		if pu.repo != m.generated {
			errs = append(errs, pu.unpush())
		}
		if pu.dir != "" {
			errs = append(errs, os.RemoveAll(filepath.Dir(pu.dir)))
		}
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
	note("removed what the measurement created: the %d Sessions it started, the workspaces and their volumes, the Agent, the pushes", m.started)
}

// writeReport writes the report's JSON and Markdown to the run directory
// and prints the Markdown.
func (m *measurement) writeReport() error {
	for _, w := range m.ws {
		// A run that ended before the preconditions set the samples up has
		// none: every figure is not available.
		samples := w.samples
		if samples == nil {
			samples = newMeasureSamples()
		}
		m.report.Workspaces = append(m.report.Workspaces, measureWorkspaceReport{
			Name: w.name, Kind: w.kind, Repositories: w.repos, Pushed: w.pushed, Worked: w.worked,
			Figures: samples.figures(), Storage: w.storage,
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

// sessionOwnerLine starts each line of sessionOwnersScript.
const sessionOwnerLine = "owner"

// sessionOwnersScript prints the owner of every session directory as
// `owner <id> <uid> <gid>`.
const sessionOwnersScript = `for d in /workspace/` + workspacesTestSessions + `/*/; do test -d "$d" || continue; ` +
	`echo "` + sessionOwnerLine + ` $(basename "$d") $(stat -c '%u %g' "$d")"; done; true
`

// parseSessionOwners reads sessionOwnersScript's lines.
func parseSessionOwners(out string) map[string]volumeOwner {
	owners := map[string]volumeOwner{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || f[0] != sessionOwnerLine {
			continue
		}
		uid, uerr := strconv.ParseInt(f[2], 10, 64)
		gid, gerr := strconv.ParseInt(f[3], 10, 64)
		if uerr != nil || gerr != nil {
			continue
		}
		owners[f[1]] = volumeOwner{uid, gid}
	}
	return owners
}

// distinctOwners is each owner of the directories once, in a stable order.
func distinctOwners(owners map[string]volumeOwner) []volumeOwner {
	var list []volumeOwner
	for _, o := range owners {
		if !slices.Contains(list, o) {
			list = append(list, o)
		}
	}
	slices.SortFunc(list, func(a, b volumeOwner) int { return cmp.Or(cmp.Compare(a.uid, b.uid), cmp.Compare(a.gid, b.gid)) })
	return list
}

// measureClearSessionsScript removes the session directories the owner's
// user owns, so its delete is not held by one, and says how many are left.
func measureClearSessionsScript(o volumeOwner) string {
	return `for d in /workspace/` + workspacesTestSessions + `/*/; do test -d "$d" || continue; ` +
		`if [ "$(stat -c %u "$d")" = "` + strconv.FormatInt(o.uid, 10) + `" ]; then rm -rf "$d"; fi; done
echo "sessions: $(ls -A /workspace/` + workspacesTestSessions + ` 2>/dev/null | wc -l) directories left"
`
}
