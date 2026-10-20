package lab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// `agentlab workspaces-test --part sync`: a workspace defined through the
// workspace-manager's tools, through muster as a signed-in member of the
// lab's Organization, and its mirrors proven on its volume over the lab's
// GitHub (the provider instance fake, which the part drives as an owner
// would: a push, a force-push, a topic removed). The steps:
//
//   - preview_workspace and create_workspace for a filter workspace (a
//     language and a topic filter over two owners, selecting two
//     repositories, the private one only where the App is installed on its
//     owner) and a list workspace (two repositories by name); get_workspace
//     and list_workspaces show both; a viewer, outside the Organization, sees
//     neither;
//   - the first cycle, which the create starts: a bare mirror of each
//     repository at mirrors/<owner>/<name>.git, every head the provider's,
//     the manifest naming each;
//   - sync_workspace with nothing changed: no fetch (the request log holds
//     listings and no git request) and a manifest byte for byte the same;
//   - a push to one repository, then a cycle fetching only that one;
//   - a session directory with a clone borrowing the mirror (git
//     alternates), a force-push, then a cycle after which the clone still
//     reads every object, the overwritten commit kept unreachable in the
//     mirror;
//   - the topic removed: the repository leaves the selection at the next
//     cycle and its mirror stays while the session directory borrows from
//     it; the session directory removed, the next cycle deletes the mirror;
//   - update_workspace re-resolving the list workspace; delete_workspace,
//     after which the volume goes, no session directory being left.
//
// A requested cycle (sync_workspace) lists every owner anew (workspace-manager
// 0.8.1 and newer), so the part requests each one right after the change it
// must see. The first cycle of a new workspace is not requested and may read
// the manager's minute-long listing cache: the part starts once a minute has
// passed since a previous run's last change at the provider. Everything the part creates is removed on every path, and the
// moving repository's topics and head are put back.

// WorkspacesPartSync is the sync part of workspaces-test.
const WorkspacesPartSync = "sync"

// WorkspacesParts are the parts `workspaces-test --part` takes.
var WorkspacesParts = []string{WorkspacesPartSync, WorkspacesPartAccess, WorkspacesPartGitLab, WorkspacesPartSessions, WorkspacesPartCallerCredential, WorkspacesPartWrite, WorkspacesPartRefresh}

const (
	syncFilterWorkspace = "agentlab-sync-filter"
	syncListWorkspace   = "agentlab-sync-list"
	syncOrg             = "agentlab-org"
	syncOSS             = "agentlab-oss"
	// syncPrivateRepo is the private repository the filter selects.
	syncPrivateRepo = syncOrg + "/platform-infra"
	// syncMovingRepo is the repository the part pushes to, force-pushes and
	// takes the topic from that selects it.
	syncMovingRepo  = syncOSS + "/docs"
	syncMovingTopic = "docs"
	syncListRepoA   = syncOrg + "/platform-api"
	syncListRepoB   = syncOSS + "/kagent"
	// syncSession is the session directory the part lays out on the filter
	// workspace's volume, as a Session's would be.
	syncSession = "agentlab-sync-proof"
	// syncListingFreshness is the workspace-manager's listing cache for a
	// cycle that is not requested, a minute, and a margin.
	syncListingFreshness = time.Minute + 5*time.Second
	// syncWorkspaceLabel labels a workspace's claim with its name.
	syncWorkspaceLabel = "workspace-manager.giantswarm.io/workspace"
	syncManifestKey    = "manifest.json"
	syncResultFailed   = "Failed"
	syncResultNoChange = "NoChange"
	syncResultSucceed  = "Succeeded"
	syncMainBranch     = "main"
	syncPushedFile     = "sync-proof.md"
	// The workspace-manager tools' argument names.
	wmWorkspaceArg    = "workspace"
	wmOrganizationArg = "organization"
	wmOwnerArg        = "owner"
	wmSourcesArg      = "sources"
	wmProviderArg     = "provider"
	gitAdd            = "add"
	// repositoriesKey is the "repositories" key of a workspace source and of
	// GitHub's installation repository listing the fake serves.
	repositoriesKey = "repositories"
	// gitOrigin is a clone's remote.
	gitOrigin = "origin"
	// neverRead says a wait never read what it waited for.
	neverRead = "never read"
	// shExitOnError runs a probe's script to its first failure.
	shExitOnError = "-ec"
)

// syncFilters are the filter workspace's filters, the same on each owner: a
// language and a topic, each the private repository's or the moving one's.
var syncFilters = map[string]any{
	"languages": []string{"HCL", "MDX"},
	"topics":    map[string]any{"names": []string{"infrastructure", syncMovingTopic}},
}

// syncFilterSources are the filter workspace's sources: both owners, or the
// moving repository's alone where the App is not installed on the private
// repository's owner.
func syncFilterSources(withPrivate bool) []map[string]any {
	var out []map[string]any
	for _, owner := range []string{syncOrg, syncOSS} {
		if owner == syncOrg && !withPrivate {
			continue
		}
		out = append(out, map[string]any{wmProviderArg: config.WorkspaceProviderFake, wmOwnerArg: owner, "filters": syncFilters})
	}
	return out
}

// syncNamedSources select repositories by name, one source per owner.
func syncNamedSources(repos ...string) []map[string]any {
	byOwner := map[string][]string{}
	var owners []string
	for _, r := range repos {
		owner, name, _ := strings.Cut(r, "/")
		if _, ok := byOwner[owner]; !ok {
			owners = append(owners, owner)
		}
		byOwner[owner] = append(byOwner[owner], name)
	}
	out := make([]map[string]any, 0, len(owners))
	for _, owner := range owners {
		out = append(out, map[string]any{wmProviderArg: config.WorkspaceProviderFake, wmOwnerArg: owner, repositoriesKey: byOwner[owner]})
	}
	return out
}

// The workspace-manager's answers, as far as the part reads them.
type syncLastSync struct {
	Time    time.Time `json:"time"`
	Result  string    `json:"result"`
	Trigger string    `json:"trigger"`
	Job     string    `json:"job"`
	Message string    `json:"message"`
}

type syncWorkspace struct {
	Name         string        `json:"name"`
	Organization string        `json:"organization"`
	Deleting     bool          `json:"deleting"`
	LastSync     *syncLastSync `json:"lastSync"`
	ActiveSync   *struct {
		Job string `json:"job"`
	} `json:"activeSync"`
	SyncRequest *struct {
		RequestedAt time.Time `json:"requestedAt"`
	} `json:"syncRequest"`
	Repositories int `json:"repositories"`
	Volume       *struct {
		ClaimName string `json:"claimName"`
		Handle    string `json:"handle"`
		Pending   string `json:"pending"`
	} `json:"volume"`
	Sessions int `json:"sessions"`
}

type syncMirror struct {
	Repository string     `json:"repository"`
	Head       string     `json:"head"`
	PushedAt   time.Time  `json:"pushedAt"`
	SizeBytes  int64      `json:"sizeBytes"`
	DroppedAt  *time.Time `json:"droppedAt"`
}

type syncSessionDirectory struct {
	Session string `json:"session"`
	Unknown bool   `json:"unknown"`
}

type syncDetail struct {
	Workspace          syncWorkspace          `json:"workspace"`
	Mirrors            []syncMirror           `json:"mirrors"`
	SessionDirectories []syncSessionDirectory `json:"sessionDirectories"`
}

// mirror is the manifest's entry of repo, nil without one.
func (d *syncDetail) mirror(repo string) *syncMirror {
	for i := range d.Mirrors {
		if d.Mirrors[i].Repository == repo {
			return &d.Mirrors[i]
		}
	}
	return nil
}

// live are the manifest's repositories that have not left the selection,
// sorted.
func (d *syncDetail) live() []string {
	var out []string
	for _, m := range d.Mirrors {
		if m.DroppedAt == nil {
			out = append(out, m.Repository)
		}
	}
	slices.Sort(out)
	return out
}

type syncPreview struct {
	Total        int `json:"total"`
	SkippedForks int `json:"skippedForks"`
	Repositories []struct {
		Repository string   `json:"repository"`
		Why        []string `json:"why"`
	} `json:"repositories"`
}

// syncMirrorLines word a manifest as a person reads it: each repository,
// its head, last push and mirror size, and when it left the selection.
func syncMirrorLines(mirrors []syncMirror) []string {
	sorted := slices.Clone(mirrors)
	slices.SortFunc(sorted, func(a, b syncMirror) int { return strings.Compare(a.Repository, b.Repository) })
	out := make([]string, 0, len(sorted))
	for _, m := range sorted {
		line := fmt.Sprintf("%s  head %s  pushed %s  %d KiB", m.Repository, shortSHA(m.Head), m.PushedAt.UTC().Format(time.RFC3339), (m.SizeBytes+1023)/1024)
		if m.DroppedAt != nil {
			line += "  dropped " + m.DroppedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, line)
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// syncListingPath is a repository listing of the GitHub API: an owner's, the
// installation's or the user's.
var syncListingPath = regexp.MustCompile(`^` + githubFakeAPIPath + `/((orgs|users)/[^/]+/repos|installation/repositories|user/repos)$`)

// gitRequestRepository is the repository of a git smart HTTP request of the
// log (/<owner>/<repo>[.git]/info/refs, git-upload-pack, git-receive-pack),
// "" for any other path.
func gitRequestRepository(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] == "api" || parts[0] == "_fake" {
		return ""
	}
	switch parts[2] {
	case "info/refs", gitUploadPack, gitReceivePack:
		return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	}
	return ""
}

// syncWindow is what a cycle asked of the provider, from the request log:
// the repositories git fetched, the listings and every other API call.
type syncWindow struct {
	fetched  []string
	listings int
	other    int
}

func classifyRequests(reqs []githubRequest) syncWindow {
	var w syncWindow
	for _, r := range reqs {
		switch repo := gitRequestRepository(r.Path); {
		case repo != "":
			if !slices.Contains(w.fetched, repo) {
				w.fetched = append(w.fetched, repo)
			}
		case r.Method == http.MethodGet && syncListingPath.MatchString(r.Path):
			w.listings++
		default:
			w.other++
		}
	}
	slices.Sort(w.fetched)
	return w
}

func (w syncWindow) String() string {
	fetched := "no git request"
	if len(w.fetched) > 0 {
		fetched = "git fetched " + strings.Join(w.fetched, ", ")
	}
	return fmt.Sprintf("%s; %d listings, %d other API calls", fetched, w.listings, w.other)
}

// lastProviderChange is when the request log last saw a repository change:
// a push that went through, or topics replaced. Zero for none.
func lastProviderChange(reqs []githubRequest) time.Time {
	var last time.Time
	for _, r := range reqs {
		push := r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/"+gitReceivePack)
		topics := r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/topics")
		if (push || topics) && r.Status/100 == 2 && r.Time.After(last) {
			last = r.Time
		}
	}
	return last
}

// judgeNoOp is the verdict on a cycle with nothing changed: the provider was
// listed and nothing fetched, and the manifest is byte for byte the one
// before.
func judgeNoOp(w syncWindow, before, after string) error {
	switch {
	case len(w.fetched) > 0:
		return fmt.Errorf("a cycle with nothing changed fetched %s: the request log holds their git requests", strings.Join(w.fetched, ", "))
	case w.listings == 0:
		return fmt.Errorf("the request log holds no listing: the cycle did not ask the provider, so it proves nothing about a no-op")
	case before != after:
		return fmt.Errorf("the manifest changed in a cycle with nothing changed:\nbefore: %s\nafter:  %s", excerpt(before, 400), excerpt(after, 400))
	}
	return nil
}

// judgeFetched is the verdict on the repositories a cycle fetched: exactly
// want.
func judgeFetched(w syncWindow, want ...string) error {
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(w.fetched, want) {
		if len(want) == 0 {
			return fmt.Errorf("the cycle fetched %s, want no fetch", strings.Join(w.fetched, ", "))
		}
		return fmt.Errorf("the cycle fetched [%s], want only %s", strings.Join(w.fetched, ", "), strings.Join(want, ", "))
	}
	return nil
}

// parseMirrorProbe reads the volume probe's lines, `<owner>/<name> k=v ...`,
// into each repository's values.
func parseMirrorProbe(out string) map[string]map[string]string {
	probe := map[string]map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(fields[0], "/") || !strings.Contains(fields[1], "=") {
			continue
		}
		values := map[string]string{}
		for _, f := range fields[1:] {
			if k, v, ok := strings.Cut(f, "="); ok {
				values[k] = v
			}
		}
		probe[fields[0]] = values
	}
	return probe
}

// syncMirrorsScript reports each repository's mirror on the volume: bare,
// its HEAD, its branches and tags, and the configuration that keeps its
// objects to the sync's own garbage collection.
func syncMirrorsScript(repos []string) string {
	var b strings.Builder
	b.WriteString(workspacesGitSetup)
	for _, r := range repos {
		fmt.Fprintf(&b, `m=/workspace/%s/%s.git
test "$(git -C $m rev-parse --is-bare-repository)" = true
echo "%s bare=true head=$(git -C $m rev-parse HEAD) branches=$(git -C $m for-each-ref refs/heads | wc -l) tags=$(git -C $m for-each-ref refs/tags | wc -l) gc.auto=$(git -C $m config gc.auto) gc.pruneExpire=$(git -C $m config gc.pruneExpire)"
`, workspacesTestMirrors, r, r)
	}
	return b.String()
}

// syncSessionDir is the session directory on the volume, syncSessionClone
// its clone of the moving repository, syncMirrorDir that repository's
// mirror.
const (
	syncSessionDir   = "/workspace/" + workspacesTestSessions + "/" + syncSession
	syncSessionClone = syncSessionDir + "/docs"
	syncMirrorDir    = "/workspace/" + workspacesTestMirrors + "/" + syncMovingRepo + ".git"
)

// syncBorrowScript lays out the session directory the way a Session's clone
// is: `git clone --shared` of the mirror, borrowing its objects through git
// alternates.
const syncBorrowScript = workspacesGitSetup + `mkdir -p ` + syncSessionDir + `
git clone -q --shared ` + syncMirrorDir + ` ` + syncSessionClone + `
echo "alternates=$(cat ` + syncSessionClone + `/.git/objects/info/alternates)"
echo "head=$(git -C ` + syncSessionClone + ` rev-parse HEAD)"
echo "own-objects=$(find ` + syncSessionClone + `/.git/objects -type f ! -path '*/info/*' | wc -l)"
`

// syncBorrowedScript proves the session directory's clone reads every object
// after the force-push overwrote overwritten in the mirror: the mirror's
// branch at its new head, the overwritten commit still in the mirror and no
// longer on its branch, and every object the clone reaches readable.
func syncBorrowedScript(overwritten string) string {
	return workspacesGitSetup + `c=` + syncSessionClone + `
m=` + syncMirrorDir + `
echo "mirror-main=$(git -C $m rev-parse refs/heads/` + syncMainBranch + `)"
git -C $m cat-file -e ` + overwritten + `^{commit}
if git -C $m merge-base --is-ancestor ` + overwritten + ` refs/heads/` + syncMainBranch + `; then echo "the overwritten commit is still on the branch: no force-push"; exit 1; fi
echo "mirror-keeps=` + overwritten + `"
missing=$(git -C $c rev-list --objects --all | cut -d' ' -f1 | git -C $c cat-file --batch-check | grep -c ' missing$' || true)
echo "objects=$(git -C $c rev-list --objects --all | wc -l) missing=$missing"
test "$missing" = 0
git -C $c fsck --connectivity-only --no-dangling
git -C $c show ` + overwritten + `:` + syncPushedFile + ` >/dev/null
echo "clone-head=$(git -C $c rev-parse HEAD) reads=` + syncPushedFile + `"
`
}

// syncMirrorPresenceScript says whether the moving repository's mirror is on
// the volume and which session directories borrow from it.
const syncMirrorPresenceScript = `if test -d ` + syncMirrorDir + `; then echo "mirror=present"; else echo "mirror=absent"; fi
for a in /workspace/` + workspacesTestSessions + `/*/*/.git/objects/info/alternates; do test -f "$a" && echo "borrower=$(echo "$a" | cut -d/ -f4) alternates=$(cat "$a")"; done
true
`

// syncRemoveSessionScript removes the session directory, as its Session's
// end would.
const syncRemoveSessionScript = `rm -rf ` + syncSessionDir + `
echo "sessions: $(ls -A /workspace/` + workspacesTestSessions + ` 2>/dev/null | wc -l) directories left"
`

// probeValue is key's value in a probe's `key=value` output, "" without
// one.
func probeValue(out, key string) string {
	for _, f := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

// syncVolume is a workspace's claim as the part finds it: its namespace
// (the workspace-manager's), name and PersistentVolume.
type syncVolume struct {
	namespace, claim, pv string
}

// syncProof is the part's state.
type syncProof struct {
	cfg              *config.Config
	timeout          time.Duration
	member, outsider *musterSession
	memberEmail      string
	outsiderEmail    string
	gh               *labGitHubClient
	tokens           map[string]string // owner → installation token
	work             string            // this host's clone of the moving repository
	startHead        string
	startTopics      []string
	private          bool
	volumes          map[string]syncVolume
	restored         bool
	// names are the workspaces the part creates, removed on every path.
	names []string
}

// newSyncProof is a part's state on the workspaces it creates.
func newSyncProof(cfg *config.Config, timeout time.Duration, names ...string) *syncProof {
	return &syncProof{cfg: cfg, timeout: timeout, tokens: map[string]string{}, volumes: map[string]syncVolume{}, names: names}
}

// workspacesSyncPart runs the part in stages.
func workspacesSyncPart(cfg *config.Config, email string, timeout time.Duration) error {
	p := newProofStages()
	sp := newSyncProof(cfg, timeout, syncFilterWorkspace, syncListWorkspace)
	err := sp.run(email, p)
	if !sp.restored {
		if rerr := sp.restore(); rerr != nil {
			warn("removing what the part left: %v", rerr)
		}
	}
	if err == nil {
		fmt.Printf("\nWorkspaces sync part passed: two workspaces defined through the workspace-manager's tools, their mirrors written once, a cycle with nothing changed fetching nothing, a push and a force-push fetched alone with the borrowed objects kept, a repository leaving with its mirror kept while borrowed, the update re-resolved and the volumes gone with the workspaces.\n")
	}
	return p.close(err)
}

func (sp *syncProof) run(email string, p *proofStages) error {
	p.begin("sync: preconditions", "The sync part's preconditions: the workspace-manager through muster, the lab's GitHub, %s signed in", email)
	if err := sp.preconditions(email); err != nil {
		return err
	}

	p.begin("sync: private repository", "The private repository %s: the App's installation on %s", syncPrivateRepo, syncOrg)
	token, err := sp.gh.installationToken(syncOrg)
	switch {
	case errors.Is(err, errNoInstallation):
		p.skip("the App of the lab's GitHub is not installed on %s: the filter workspace selects the public %s alone", syncOrg, syncMovingRepo)
	case err != nil:
		return err
	default:
		sp.private, sp.tokens[syncOrg] = true, token
		note("the App is installed on %s: the filter selects %s through it", syncOrg, syncPrivateRepo)
	}
	wantFilter := []string{syncMovingRepo}
	if sp.private {
		wantFilter = append(wantFilter, syncPrivateRepo)
	}
	slices.Sort(wantFilter)
	wantList := []string{syncListRepoA, syncListRepoB}

	p.begin("sync: preview", "preview_workspace as %s: the filter draft (languages HCL, MDX and topics infrastructure, %s over %s) and the list draft (%s)", sp.memberEmail, syncMovingTopic, strings.Join(sourceOwners(syncFilterSources(sp.private)), ", "), strings.Join(wantList, ", "))
	if err := sp.preview(syncFilterSources(sp.private), wantFilter); err != nil {
		return err
	}
	if err := sp.preview(syncNamedSources(wantList...), wantList); err != nil {
		return err
	}

	p.begin("sync: create", "create_workspace %s and %s as %s; get_workspace and list_workspaces show both", syncFilterWorkspace, syncListWorkspace, sp.memberEmail)
	firstMark, err := sp.mark()
	if err != nil {
		return err
	}
	for name, sources := range map[string][]map[string]any{syncFilterWorkspace: syncFilterSources(sp.private), syncListWorkspace: syncNamedSources(wantList...)} {
		var created syncWorkspace
		if err := sp.member.createLabWorkspace(map[string]any{
			nameKey: name, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: sources,
		}, &created); err != nil {
			return err
		}
		note("created %s of Organization %s", created.Name, created.Organization)
	}
	if err := sp.bothListed(); err != nil {
		return err
	}

	p.begin("sync: outside the Organization", "%s, outside the Organization %s, sees neither workspace", sp.outsiderEmail, config.WorkspaceOrganization)
	if err := sp.outsiderSeesNothing(); err != nil {
		return err
	}

	p.begin("sync: first cycle", "The first cycle, which the create starts: a bare mirror per repository, every head the provider's, the manifest naming each")
	filter, err := sp.waitCycle(syncFilterWorkspace, nil)
	if err != nil {
		return err
	}
	list, err := sp.waitCycle(syncListWorkspace, nil)
	if err != nil {
		return err
	}
	if err := sp.windowFetched(firstMark, append(slices.Clone(wantFilter), wantList...)...); err != nil {
		return err
	}
	if err := sp.firstCycle(filter, wantFilter); err != nil {
		return err
	}
	if err := sp.firstCycle(list, wantList); err != nil {
		return err
	}

	p.begin("sync: no-op", "sync_workspace %s with nothing changed: no fetch, the manifest unchanged", syncFilterWorkspace)
	before, err := sp.manifest(syncFilterWorkspace)
	if err != nil {
		return err
	}
	noop, window, err := sp.sync(syncFilterWorkspace, filter)
	if err != nil {
		return err
	}
	after, err := sp.manifest(syncFilterWorkspace)
	if err != nil {
		return err
	}
	if err := judgeNoOp(window, before, after); err != nil {
		return err
	}
	if r := noop.Workspace.LastSync; r.Result != syncResultNoChange || r.Job != "" {
		return fmt.Errorf("the cycle with nothing changed ended %s with job %q, want %s and no sync job", r.Result, r.Job, syncResultNoChange)
	}
	note("result %s (trigger %s, no sync job); the request log: %s; the manifest byte for byte the same (%d bytes)", noop.Workspace.LastSync.Result, noop.Workspace.LastSync.Trigger, window, len(after))

	p.begin("sync: push", "A push to %s, then a cycle that fetches only it", syncMovingRepo)
	pushed, err := sp.push()
	if err != nil {
		return err
	}
	afterPush, window, err := sp.sync(syncFilterWorkspace, noop)
	if err != nil {
		return err
	}
	if err := judgeFetched(window, syncMovingRepo); err != nil {
		return err
	}
	if err := sp.heads(afterPush, map[string]string{syncMovingRepo: pushed}, noop, syncMovingRepo); err != nil {
		return err
	}
	note("result %s; the request log: %s", afterPush.Workspace.LastSync.Result, window)
	sp.printManifest(afterPush)

	p.begin("sync: force-push", "A session directory borrowing %s's mirror, a force-push, then a cycle after which its clone reads every object", syncMovingRepo)
	borrowed, err := sp.onVolume(syncFilterWorkspace, "borrow", syncBorrowScript)
	if err != nil {
		return err
	}
	if h := probeValue(borrowed, probeHeadKey); h != pushed {
		return fmt.Errorf("the session directory's clone is at %s, want the mirror's head %s:\n%s", h, pushed, borrowed)
	}
	note("%s", indent(borrowed, "  "))
	forced, err := sp.forcePush()
	if err != nil {
		return err
	}
	afterForce, window, err := sp.sync(syncFilterWorkspace, afterPush)
	if err != nil {
		return err
	}
	if err := judgeFetched(window, syncMovingRepo); err != nil {
		return err
	}
	if err := sp.heads(afterForce, map[string]string{syncMovingRepo: forced}, afterPush, syncMovingRepo); err != nil {
		return err
	}
	if !slices.ContainsFunc(afterForce.SessionDirectories, func(s syncSessionDirectory) bool { return s.Session == syncSession }) {
		return fmt.Errorf("get_workspace lists no session directory %s after the cycle", syncSession)
	}
	out, err := sp.onVolume(syncFilterWorkspace, "borrowed", syncBorrowedScript(pushed))
	if err != nil {
		return err
	}
	if probeValue(out, "mirror-main") != forced {
		return fmt.Errorf("the mirror's %s is not the force-pushed %s:\n%s", syncMainBranch, forced, out)
	}
	note("result %s; the request log: %s; get_workspace lists the session directory %s", afterForce.Workspace.LastSync.Result, window, syncSession)
	note("%s", indent(out, "  "))
	sp.printManifest(afterForce)

	p.begin("sync: topic removed", "The topic %s removed from %s: it leaves the selection, its mirror stays while %s borrows from it", syncMovingTopic, syncMovingRepo, syncSession)
	if err := sp.gh.setTopics(syncMovingRepo, sp.tokens[syncOSS], withoutTopic(sp.startTopics, syncMovingTopic)); err != nil {
		return err
	}
	afterTopic, window, err := sp.sync(syncFilterWorkspace, afterForce)
	if err != nil {
		return err
	}
	if err := judgeFetched(window); err != nil {
		return err
	}
	if m := afterTopic.mirror(syncMovingRepo); m == nil || m.DroppedAt == nil {
		return fmt.Errorf("the manifest after the topic's removal does not mark %s dropped: %v", syncMovingRepo, syncMirrorLines(afterTopic.Mirrors))
	}
	if live := afterTopic.live(); slices.Contains(live, syncMovingRepo) || afterTopic.Workspace.Repositories != len(wantFilter)-1 {
		return fmt.Errorf("the workspace still selects %s after its topic went: %v (%d repositories)", syncMovingRepo, live, afterTopic.Workspace.Repositories)
	}
	out, err = sp.onVolume(syncFilterWorkspace, "dropped", syncMirrorPresenceScript)
	if err != nil {
		return err
	}
	if probeValue(out, "mirror") != "present" || !strings.Contains(out, "borrower="+syncSession) {
		return fmt.Errorf("the dropped mirror of %s is not on the volume with its borrower %s:\n%s", syncMovingRepo, syncSession, out)
	}
	note("result %s; the request log: %s; %d repositories selected", afterTopic.Workspace.LastSync.Result, window, afterTopic.Workspace.Repositories)
	note("%s", indent(out, "  "))
	sp.printManifest(afterTopic)

	p.begin("sync: borrower gone", "The session directory removed: the next cycle deletes the dropped mirror")
	if out, err = sp.onVolume(syncFilterWorkspace, "unborrow", syncRemoveSessionScript); err != nil {
		return err
	}
	note("%s", indent(out, "  "))
	afterGone, window, err := sp.sync(syncFilterWorkspace, afterTopic)
	if err != nil {
		return err
	}
	if err := judgeFetched(window); err != nil {
		return err
	}
	if afterGone.mirror(syncMovingRepo) != nil {
		return fmt.Errorf("the manifest still records %s after its last borrower went: %v", syncMovingRepo, syncMirrorLines(afterGone.Mirrors))
	}
	if out, err = sp.onVolume(syncFilterWorkspace, "unborrowed", syncMirrorPresenceScript); err != nil {
		return err
	}
	if probeValue(out, "mirror") != probeAbsent {
		return fmt.Errorf("the mirror of %s is still on the volume after its last borrower went:\n%s", syncMovingRepo, out)
	}
	note("result %s (trigger %s, sync job %s looked after the volume); the request log: %s; the mirror of %s gone", afterGone.Workspace.LastSync.Result, afterGone.Workspace.LastSync.Trigger, afterGone.Workspace.LastSync.Job, window, syncMovingRepo)
	sp.printManifest(afterGone)

	p.begin("sync: update", "update_workspace %s: %s replaced by %s by name", syncListWorkspace, syncListRepoB, syncMovingRepo)
	var updated syncWorkspace
	wantUpdated := []string{syncListRepoA, syncMovingRepo}
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"update_workspace", map[string]any{
		nameKey: syncListWorkspace, wmSourcesArg: syncNamedSources(wantUpdated...),
	}, &updated); err != nil {
		return err
	}
	afterUpdate, err := sp.waitCycle(syncListWorkspace, list.Workspace.LastSync)
	if err != nil {
		return err
	}
	if live := afterUpdate.live(); !slices.Equal(live, wantUpdated) || afterUpdate.mirror(syncListRepoB) != nil {
		return fmt.Errorf("after the update the manifest holds %v, want %v and no %s (no session directory borrows it)", syncMirrorLines(afterUpdate.Mirrors), wantUpdated, syncListRepoB)
	}
	note("result %s (trigger %s)", afterUpdate.Workspace.LastSync.Result, afterUpdate.Workspace.LastSync.Trigger)
	sp.printManifest(afterUpdate)

	p.begin("sync: delete", "delete_workspace %s and %s: no session directory left, each volume goes", syncFilterWorkspace, syncListWorkspace)
	for _, name := range []string{syncFilterWorkspace, syncListWorkspace} {
		if err := sp.delete(name); err != nil {
			return err
		}
	}

	p.begin("sync: restore", "%s back to topics %v and head %s", syncMovingRepo, sp.startTopics, shortSHA(sp.startHead))
	return sp.restore()
}

// sourceOwners are the sources' owners.
func sourceOwners(sources []map[string]any) []string {
	var out []string
	for _, s := range sources {
		out = append(out, fmt.Sprint(s[wmOwnerArg]))
	}
	return out
}

func withoutTopic(topics []string, topic string) []string {
	return slices.DeleteFunc(slices.Clone(topics), func(t string) bool { return t == topic })
}

// preconditions fails fast without what the part needs: the
// workspace-manager registered with muster and connected, the lab's GitHub
// as its provider instance, a member and an outsider signed in; then it
// removes what an interrupted run left and clones the moving repository.
func (sp *syncProof) preconditions(email string) error {
	cfg := sp.cfg
	if reason, err := workspaceManagerSkip(cfg); err != nil {
		return err
	} else if reason != "" {
		return fmt.Errorf("no workspace-manager to define a workspace with: %s", reason)
	}
	if !cfg.WorkspaceFakeProvider() {
		return fmt.Errorf("the workspaces parts push, force-push and change topics on the lab's GitHub, the provider instance %s, and platform.workspaces.provider %q does not configure it: `agentlab configure --workspaces-provider %s` (or fake,github), then `agentlab platform`",
			config.WorkspaceProviderFake, cfg.Platform.Workspaces.Provider, config.WorkspaceProviderFake)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	registered, err := objectExists(ctx, mcpServers, platformNamespace, workspaceManagerMCPServer)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("no workspace-manager: no MCPServer %s in %s registers it with muster (`kubectl -n %s get mcpservers.muster.giantswarm.io`)", workspaceManagerMCPServer, platformNamespace, platformNamespace)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
	}

	member := cfg.FindUser(email)
	if member == nil {
		return fmt.Errorf("no user %s in %s", email, config.File)
	}
	if !slices.ContainsFunc(config.WorkspaceOrganizationGroups, member.HasGroup) {
		return fmt.Errorf("%s is no member of the Organization %s (groups %s): the part creates workspaces as a member", email, config.WorkspaceOrganization, strings.Join(config.WorkspaceOrganizationGroups, ", "))
	}
	outsider := cfg.FindUserInGroup("viewers")
	if outsider == nil {
		return fmt.Errorf("the part needs a viewers user, outside the Organization %s, in %s (`agentlab configure --defaults` writes one)", config.WorkspaceOrganization, config.File)
	}
	sp.memberEmail, sp.outsiderEmail = member.Email, outsider.Email
	for _, u := range []struct {
		user *config.User
		into **musterSession
	}{{member, &sp.member}, {outsider, &sp.outsider}} {
		p, err := openGrantedWorkspacePerson(cfg, u.user)
		if err != nil {
			return err
		}
		*u.into = p.session
	}
	note("signed in to muster as %s (member) and %s (outside the Organization), each holding the workspace-manager's App grant where muster pins one", sp.memberEmail, sp.outsiderEmail)

	if sp.gh, err = openLabGitHub(cfg); err != nil {
		return err
	}
	if sp.tokens[syncOSS], err = sp.gh.installationToken(syncOSS); err != nil {
		return fmt.Errorf("the part pushes to %s as the App's installation on %s: %w", syncMovingRepo, syncOSS, err)
	}
	note("the lab's GitHub on %s; its request log at %s%s", sp.gh.url, sp.gh.url, githubFakeRequestsPath)
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the volume probes' git image (%s)", res.d)
	}

	if err := sp.removeLeftovers(); err != nil {
		return err
	}
	fixture, err := loadGitHubFixture("")
	if err != nil {
		return err
	}
	for _, r := range fixture.Repositories {
		if r.fullName() == syncMovingRepo {
			sp.startTopics = r.Topics
		}
	}
	if current, err := sp.gh.topics(syncMovingRepo, sp.tokens[syncOSS]); err != nil {
		return err
	} else if !slices.Equal(current, sp.startTopics) {
		note("%s carries topics %v, the fixture's are %v: put back first", syncMovingRepo, current, sp.startTopics)
		if err := sp.gh.setTopics(syncMovingRepo, sp.tokens[syncOSS], sp.startTopics); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	if sp.work, err = os.MkdirTemp(StateDir, "workspaces-sync-"); err != nil {
		return err
	}
	if _, err := sp.gh.git(sp.work, sp.tokens[syncOSS], "clone", "-q", sp.gh.remote(syncMovingRepo), "docs"); err != nil {
		return err
	}
	sp.work = filepath.Join(sp.work, "docs")
	if sp.startHead, err = sp.gh.git(sp.work, "", "rev-parse", "HEAD"); err != nil {
		return err
	}
	note("%s cloned on this host at %s, topics %v", syncMovingRepo, shortSHA(sp.startHead), sp.startTopics)

	// A listing the manager cached before the provider's last change would
	// give the first cycle a stale last push, and the no-op would fetch.
	reqs, err := sp.gh.requests()
	if err != nil {
		return err
	}
	if changed := lastProviderChange(reqs); !changed.IsZero() {
		if wait := time.Until(changed.Add(syncListingFreshness)); wait > 0 {
			note("waiting %s: a repository changed at the provider at %s, and the manager may still answer a listing from before it", wait.Round(time.Second), changed.UTC().Format(time.RFC3339))
			time.Sleep(wait)
		}
	}
	return nil
}

// removeLeftovers deletes the workspaces of an interrupted run, their
// session directory first so the volume can go.
func (sp *syncProof) removeLeftovers() error {
	for _, name := range sp.names {
		gone, err := sp.gone(name)
		if err != nil {
			return err
		}
		if gone {
			continue
		}
		note("removing the leftover workspace %s of an interrupted run", name)
		if err := sp.delete(name); err != nil {
			return err
		}
	}
	return nil
}

// preview checks preview_workspace's selection of a draft against want.
func (sp *syncProof) preview(sources []map[string]any, want []string) error {
	var preview syncPreview
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"preview_workspace", map[string]any{
		wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: sources,
	}, &preview); err != nil {
		return err
	}
	var got []string
	for _, r := range preview.Repositories {
		got = append(got, r.Repository)
		note("%s  selected by %s", r.Repository, strings.Join(r.Why, ", "))
	}
	slices.Sort(got)
	if preview.Total != len(want) || !slices.Equal(got, want) {
		return fmt.Errorf("preview_workspace selects %d: %v, want %v", preview.Total, got, want)
	}
	if preview.SkippedForks > 0 {
		note("%d forks left out by the filters", preview.SkippedForks)
	}
	return nil
}

// bothListed checks get_workspace and list_workspaces show both workspaces
// to the member.
func (sp *syncProof) bothListed() error {
	var listed struct {
		Workspaces []syncWorkspace `json:"workspaces"`
	}
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"list_workspaces", map[string]any{wmOrganizationArg: config.WorkspaceOrganization}, &listed); err != nil {
		return err
	}
	for _, name := range []string{syncFilterWorkspace, syncListWorkspace} {
		if !slices.ContainsFunc(listed.Workspaces, func(w syncWorkspace) bool { return w.Name == name }) {
			return fmt.Errorf("list_workspaces for %s as %s does not list %s", config.WorkspaceOrganization, sp.memberEmail, name)
		}
		d, err := sp.get(name)
		if err != nil {
			return err
		}
		if d.Workspace.Organization != config.WorkspaceOrganization {
			return fmt.Errorf("get_workspace %s names Organization %q, want %s", name, d.Workspace.Organization, config.WorkspaceOrganization)
		}
	}
	note("list_workspaces for %s lists both (%d workspaces); get_workspace answers each", config.WorkspaceOrganization, len(listed.Workspaces))
	return nil
}

// outsiderSeesNothing checks the viewer is refused the Organization's
// listing and each workspace, and that its own listing names neither.
func (sp *syncProof) outsiderSeesNothing() error {
	env, err := sp.outsider.callToolEnvelope(workspaceManagerToolPrefix+"list_workspaces", map[string]any{wmOrganizationArg: config.WorkspaceOrganization})
	if err != nil {
		return err
	}
	if !env.IsError || !strings.Contains(env.Content[0].Text, "not a member") {
		return fmt.Errorf("list_workspaces for %s as %s answered %.300q, want a refusal as not a member", config.WorkspaceOrganization, sp.outsiderEmail, env.Content[0].Text)
	}
	note("list_workspaces for %s refused: %.160s", config.WorkspaceOrganization, env.Content[0].Text)
	var own struct {
		Workspaces []syncWorkspace `json:"workspaces"`
	}
	if err := sp.outsider.callServerStructured(workspaceManagerToolPrefix+"list_workspaces", nil, &own); err != nil {
		return err
	}
	if len(own.Workspaces) > 0 {
		var names []string
		for _, w := range own.Workspaces {
			names = append(names, w.Name)
		}
		return fmt.Errorf("list_workspaces as %s lists %v, want none: the viewer is a member of no Organization", sp.outsiderEmail, names)
	}
	note("list_workspaces of its own Organizations: none")
	for _, name := range []string{syncFilterWorkspace, syncListWorkspace} {
		env, err := sp.outsider.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name})
		if err != nil {
			return err
		}
		if !env.IsError || !strings.Contains(env.Content[0].Text, "permission denied") {
			return fmt.Errorf("get_workspace %s as %s answered %.300q, want permission denied", name, sp.outsiderEmail, env.Content[0].Text)
		}
	}
	note("get_workspace of each refused: permission denied")
	return nil
}

// firstCycle checks the cycle the create started: its mirrors on the volume
// and against the provider's heads.
func (sp *syncProof) firstCycle(d *syncDetail, want []string) error {
	name := d.Workspace.Name
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s, want %s: %s", name, r.Result, syncResultSucceed, r.Message)
	}
	if live := d.live(); !slices.Equal(live, want) {
		return fmt.Errorf("the first cycle of %s mirrored %v, want %v", name, live, want)
	}
	provider := map[string]string{}
	for _, repo := range want {
		head, err := sp.providerHead(repo)
		if err != nil {
			return err
		}
		provider[repo] = head
	}
	if err := sp.heads(d, provider, nil); err != nil {
		return err
	}
	out, err := sp.onVolume(name, "mirrors", syncMirrorsScript(want))
	if err != nil {
		return err
	}
	probe := parseMirrorProbe(out)
	for _, repo := range want {
		m, got := d.mirror(repo), probe[repo]
		switch {
		case got == nil:
			return fmt.Errorf("no mirror of %s on the volume of %s:\n%s", repo, name, out)
		case got[probeHeadKey] != m.Head:
			return fmt.Errorf("the mirror of %s on the volume is at %s, the manifest says %s", repo, got[probeHeadKey], m.Head)
		case got["gc.pruneExpire"] != "never":
			return fmt.Errorf("the mirror of %s has gc.pruneExpire %q, want never: its unreachable objects are not kept for the sync", repo, got["gc.pruneExpire"])
		}
	}
	v := sp.volumes[name]
	note("%s: result %s (trigger %s, sync job %s), claim %s/%s on %s", name, d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, d.Workspace.LastSync.Job, v.namespace, v.claim, v.pv)
	note("%s", indent(out, "  "))
	sp.printManifest(d)
	return nil
}

// windowFetched checks the request log since mark holds a fetch of exactly
// the repositories.
func (sp *syncProof) windowFetched(mark int, repos ...string) error {
	reqs, err := sp.gh.requests()
	if err != nil {
		return err
	}
	w := classifyRequests(reqs[min(mark, len(reqs)):])
	if err := judgeFetched(w, repos...); err != nil {
		return err
	}
	note("the request log since the create: %s", w)
	return nil
}

// providerHead is the head of the repository's default branch at the
// provider, as git sees it.
func (sp *syncProof) providerHead(repo string) (string, error) {
	owner, _, _ := strings.Cut(repo, "/")
	out, err := sp.gh.git(StateDir, sp.tokens[owner], "ls-remote", sp.gh.remote(repo), "refs/heads/"+syncMainBranch)
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	if sha == "" {
		return "", fmt.Errorf("%s has no branch %s at the provider", repo, syncMainBranch)
	}
	return sha, nil
}

// heads checks the manifest's heads: want's at the given values, every
// other repository at its head in prev (nil: no comparison).
func (sp *syncProof) heads(d *syncDetail, want map[string]string, prev *syncDetail, changed ...string) error {
	for repo, head := range want {
		m := d.mirror(repo)
		if m == nil || m.Head != head {
			return fmt.Errorf("the manifest names %v for %s, want its head %s at the provider", m, repo, head)
		}
	}
	if prev == nil {
		return nil
	}
	for _, m := range prev.Mirrors {
		if slices.Contains(changed, m.Repository) || m.DroppedAt != nil {
			continue
		}
		if now := d.mirror(m.Repository); now == nil || now.Head != m.Head {
			return fmt.Errorf("the head of %s moved from %s to %v in a cycle that changed only %v", m.Repository, m.Head, now, changed)
		}
	}
	return nil
}

func (sp *syncProof) printManifest(d *syncDetail) {
	note("manifest of %s:\n%s", d.Workspace.Name, indent(strings.Join(syncMirrorLines(d.Mirrors), "\n"), "  "))
}

// push commits a file to the moving repository's default branch and pushes
// it, returning the new head. The commit may be empty: a run interrupted
// after its push leaves the same file on the branch, and the push must still
// move the head.
func (sp *syncProof) push() (string, error) {
	if err := os.WriteFile(filepath.Join(sp.work, syncPushedFile), []byte("# pushed by agentlab workspaces-test --part sync\n"), 0o600); err != nil {
		return "", err
	}
	for _, args := range [][]string{{gitAdd, syncPushedFile}, {gitCommit, "-q", "--allow-empty", "-m", "workspaces-test: a push"}, {githubPush, "-q", gitOrigin, "HEAD:refs/heads/" + syncMainBranch}} {
		if _, err := sp.gh.git(sp.work, sp.tokens[syncOSS], args...); err != nil {
			return "", err
		}
	}
	head, err := sp.gh.git(sp.work, "", "rev-parse", "HEAD")
	if err == nil {
		note("pushed %s to %s's %s", shortSHA(head), syncMovingRepo, syncMainBranch)
	}
	return head, err
}

// forcePush overwrites the pushed commit with another on the start head and
// force-pushes it, returning the new head.
func (sp *syncProof) forcePush() (string, error) {
	if _, err := sp.gh.git(sp.work, "", "reset", "-q", "--hard", sp.startHead); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(sp.work, "force-pushed.md"), []byte("# force-pushed by agentlab workspaces-test --part sync\n"), 0o600); err != nil {
		return "", err
	}
	for _, args := range [][]string{{gitAdd, "force-pushed.md"}, {gitCommit, "-qm", "workspaces-test: a force-push"}, {githubPush, "-q", "--force", gitOrigin, "HEAD:refs/heads/" + syncMainBranch}} {
		if _, err := sp.gh.git(sp.work, sp.tokens[syncOSS], args...); err != nil {
			return "", err
		}
	}
	head, err := sp.gh.git(sp.work, "", "rev-parse", "HEAD")
	if err == nil {
		note("force-pushed %s over %s's %s", shortSHA(head), syncMovingRepo, syncMainBranch)
	}
	return head, err
}

// createNamed defines a workspace of the lab's Organization over the named
// repositories as the member, waits for its first cycle to mirror exactly
// them, and returns its volume: the one the workspace's grants name.
func (sp *syncProof) createNamed(name string, repos []string) (workspaceVolume, error) {
	var created syncWorkspace
	if err := sp.member.createLabWorkspace(map[string]any{
		nameKey: name, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: syncNamedSources(repos...),
	}, &created); err != nil {
		return workspaceVolume{}, err
	}
	d, err := sp.waitCycle(name, nil)
	if err != nil {
		return workspaceVolume{}, err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return workspaceVolume{}, fmt.Errorf("the first cycle of %s ended %s: %s", name, r.Result, r.Message)
	}
	want := slices.Sorted(slices.Values(repos))
	if live := d.live(); !slices.Equal(live, want) {
		return workspaceVolume{}, fmt.Errorf("the first cycle of %s mirrored %v, want %v", name, live, want)
	}
	v, err := sp.volume(name)
	if err != nil {
		return workspaceVolume{}, err
	}
	k, err := labKube()
	if err != nil {
		return workspaceVolume{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pv, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, v.pv, metav1.GetOptions{})
	if err != nil {
		return workspaceVolume{}, fmt.Errorf("reading the volume %s of workspace %s: %w", v.pv, name, err)
	}
	if pv.Spec.CSI == nil {
		return workspaceVolume{}, fmt.Errorf("the volume %s of workspace %s is no CSI volume", v.pv, name)
	}
	volume := workspaceVolume{Driver: pv.Spec.CSI.Driver, Handle: pv.Spec.CSI.VolumeHandle}
	note("created %s of Organization %s; first cycle %s; claim %s/%s, volume %s (%s)", name, created.Organization, d.Workspace.LastSync.Result, v.namespace, v.claim, volume.Handle, volume.Driver)
	sp.printManifest(d)
	return volume, nil
}

// get is get_workspace's answer.
func (sp *syncProof) get(name string) (*syncDetail, error) {
	var d syncDetail
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// gone says whether get_workspace answers not found.
func (sp *syncProof) gone(name string) (bool, error) {
	env, err := sp.member.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name})
	if err != nil {
		return false, err
	}
	return env.IsError && strings.Contains(env.Content[0].Text, "not found"), nil
}

// mark is the request log's length now: the start of a window.
func (sp *syncProof) mark() (int, error) {
	reqs, err := sp.gh.requests()
	return len(reqs), err
}

// sync requests a cycle and waits for its end; the requested cycle lists the
// provider anew, so it sees what changed there. It returns the workspace after
// the cycle and what the cycle asked of the provider.
func (sp *syncProof) sync(name string, prev *syncDetail) (*syncDetail, syncWindow, error) {
	mark, err := sp.mark()
	if err != nil {
		return nil, syncWindow{}, err
	}
	var requested struct {
		RequestedAt time.Time `json:"requestedAt"`
	}
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"sync_workspace", map[string]any{wmWorkspaceArg: name}, &requested); err != nil {
		return nil, syncWindow{}, err
	}
	d, err := sp.waitCycle(name, prev.Workspace.LastSync)
	if err != nil {
		return nil, syncWindow{}, err
	}
	reqs, err := sp.gh.requests()
	if err != nil {
		return nil, syncWindow{}, err
	}
	return d, classifyRequests(reqs[min(mark, len(reqs)):]), nil
}

// waitCycle waits for a cycle that ends after prev (nil: the first) with no
// request pending and no sync job running; a failed cycle is the error.
func (sp *syncProof) waitCycle(name string, prev *syncLastSync) (*syncDetail, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	var last *syncDetail
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, sp.timeout, true, func(context.Context) (bool, error) {
		d, err := sp.get(name)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last = d
		w := d.Workspace
		return w.ActiveSync == nil && w.SyncRequest == nil && w.LastSync != nil && (prev == nil || w.LastSync.Time.After(prev.Time)), nil
	})
	if err != nil {
		state := neverRead
		if last != nil {
			state = fmt.Sprintf("last sync %+v, active %v, request %v", last.Workspace.LastSync, last.Workspace.ActiveSync != nil, last.Workspace.SyncRequest != nil)
		}
		return nil, fmt.Errorf("no cycle of %s ended within %s (%s; last error %v): the workspace-manager's log, `kubectl -n kagent get jobs`", name, sp.timeout, state, lastErr)
	}
	if r := last.Workspace.LastSync; r.Result == syncResultFailed {
		return nil, fmt.Errorf("the cycle of %s failed (trigger %s, job %s): %s", name, r.Trigger, r.Job, r.Message)
	}
	return last, nil
}

// volume finds the workspace's claim by its label.
func (sp *syncProof) volume(name string) (syncVolume, error) {
	if v, ok := sp.volumes[name]; ok {
		return v, nil
	}
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
	v := syncVolume{namespace: c.Namespace, claim: c.Name, pv: c.Spec.VolumeName}
	sp.volumes[name] = v
	return v, nil
}

// manifest is the workspace's manifest as the sync wrote it, the ConfigMap
// beside its claim.
func (sp *syncProof) manifest(name string) (string, error) {
	v, err := sp.volume(name)
	if err != nil {
		return "", err
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cm, err := k.clientset.CoreV1().ConfigMaps(v.namespace).Get(ctx, "workspace-"+name+"-manifest", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the manifest of workspace %s: %w", name, err)
	}
	return cm.Data[syncManifestKey], nil
}

// workspaceVolumeUser is the user and group a workspace volume probe runs
// as, with the group as its fsGroup: the workspace-manager's sync Job's, the
// identity that lays the volume out and removes the session directories on
// it, so a probe reads, writes and removes what the Job can.
const workspaceVolumeUser int64 = 1000

// volumeOwner is the user and group a file on a workspace volume belongs to,
// and a probe that must enter it runs as.
type volumeOwner struct{ uid, gid int64 }

// workspaceVolumeOwner is the sync Job's user and group.
var workspaceVolumeOwner = volumeOwner{workspaceVolumeUser, workspaceVolumeUser}

// runnable is the owner a restricted probe can run as: root, whom
// runAsNonRoot refuses, and the zero owner (unread) are the sync Job's user.
func (o volumeOwner) runnable() volumeOwner {
	if o.uid == 0 {
		return workspaceVolumeOwner
	}
	return o
}

// workspaceVolumePod is a probe pod on the git image with the workspace's
// claim mounted whole at /workspace, as the sync Job's user and admitted
// under the PodSecurity `restricted` profile an installation enforces.
func workspaceVolumePod(name, claim, script string) *corev1.Pod {
	pod := probePod(name, workspacesTestGitImage, []string{"sh", shExitOnError, script})
	pod.Spec.Volumes = []corev1.Volume{{Name: workspacesTestVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath}}
	// The user has no home in the image: git's global config goes to /tmp,
	// as the sync Job's does.
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "HOME", Value: "/tmp"}}
	user := workspaceVolumeUser
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &user, RunAsGroup: &user, FSGroup: &user}
	restrictPodSecurity(pod)
	return pod
}

// onVolume runs a probe on the git image with the workspace's whole volume
// at /workspace and returns its output.
func (sp *syncProof) onVolume(name, probe, script string) (string, error) {
	v, err := sp.volume(name)
	if err != nil {
		return "", err
	}
	pod := workspaceVolumePod("agentlab-sync-"+probe, v.claim, script)
	out, err := runPod(context.Background(), v.namespace, pod, sp.timeout)
	if err != nil {
		return "", fmt.Errorf("the volume probe %s on workspace %s: %w\n%s", probe, name, err, out)
	}
	return strings.TrimSpace(out), nil
}

// delete removes the workspace's session directory, deletes it through
// delete_workspace and waits for it, its claim and its volume to go.
func (sp *syncProof) delete(name string) error {
	v, err := sp.volume(name)
	if err == nil {
		out, err := sp.onVolume(name, "clear", syncRemoveSessionScript)
		if err != nil {
			return err
		}
		note("%s", indent(out, "  "))
	}
	var res struct {
		Sessions []string `json:"sessions"`
	}
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"delete_workspace", map[string]any{wmWorkspaceArg: name}, &res); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return err
	}
	var state string
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, sp.timeout, true, func(ctx context.Context) (bool, error) {
		gone, err := sp.gone(name)
		if err != nil || !gone {
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
		return fmt.Errorf("workspace %s is not gone %s after delete_workspace: %s", name, sp.timeout, state)
	}
	delete(sp.volumes, name)
	if v.pv == "" {
		note("deleted %s (no volume was claimed)", name)
		return nil
	}
	node := WorkspacesNode(sp.cfg)
	out, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", "ls -A "+workspacesExportDir+" 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("reading the export on %s: %w", node, err)
	}
	if slices.Contains(strings.Fields(out), v.pv) {
		return fmt.Errorf("the export on %s still holds %s, the volume of workspace %s", node, v.pv, name)
	}
	note("deleted %s (%d Sessions with a directory at the delete): the workspace, its claim %s/%s and its volume %s gone, nothing of it under %s", name, len(res.Sessions), v.namespace, v.claim, v.pv, workspacesExportDir)
	return nil
}

// restore removes what the part left, best effort past the first failure:
// the workspaces, the moving repository's topics and head as they were, the
// host's clone. It returns the first failure.
func (sp *syncProof) restore() error {
	sp.restored = true
	var errs []error
	if sp.member != nil {
		for _, name := range sp.names {
			if gone, err := sp.gone(name); err == nil && !gone {
				errs = append(errs, sp.delete(name))
			}
		}
	}
	if sp.gh != nil && sp.tokens[syncOSS] != "" && sp.startTopics != nil {
		errs = append(errs, sp.gh.setTopics(syncMovingRepo, sp.tokens[syncOSS], sp.startTopics))
	}
	if sp.work != "" && sp.startHead != "" {
		if head, err := sp.providerHead(syncMovingRepo); err != nil || head != sp.startHead {
			_, err := sp.gh.git(sp.work, sp.tokens[syncOSS], "push", "-q", "--force", gitOrigin, sp.startHead+":refs/heads/"+syncMainBranch)
			errs = append(errs, err)
		}
	}
	if sp.work != "" {
		errs = append(errs, os.RemoveAll(filepath.Dir(sp.work)))
	}
	err := errors.Join(errs...)
	if err == nil && sp.startHead != "" {
		note("%s back to topics %v at %s; the workspaces and the host's clone removed", syncMovingRepo, sp.startTopics, shortSHA(sp.startHead))
	}
	return err
}
