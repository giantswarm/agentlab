package lab

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// `agentlab workspaces-test --part sync [--provider github|gitlab]`: a
// workspace defined through the workspace-manager's tools, through muster as
// the lab's admin, a member of the lab's Organization, over the provider
// instance's two proof repositories on the real provider
// (workspacesproofrepos.go), and its mirrors proven on its volume through
// every kind of cycle:
//
//   - the first cycle, which the create starts: a bare mirror of each
//     repository at mirrors/<owner>/<name>.git, its default branch at the
//     provider's head, its unreachable objects kept for the sync's own
//     garbage collection (gc.pruneExpire never), the manifest naming each;
//   - sync_workspace with nothing changed: no sync Job (NoChange), no
//     mirror fetched (no FETCH_HEAD written), the manifest byte for byte the
//     same;
//   - a push to a scratch branch of the first repository, then cycles until
//     one fetches it: only that mirror fetched, the branch at the pushed
//     commit in it, the other mirror untouched;
//   - a session directory with a clone borrowing that mirror (git
//     alternates), a force-push over the pushed commit, then a cycle after
//     which the clone still reads every object, the overwritten commit kept
//     unreachable in the mirror;
//   - update_workspace dropping the first repository: it leaves the
//     selection and its mirror stays while the session directory borrows
//     from it; the session directory removed, the next cycle deletes the
//     mirror;
//   - update_workspace selecting both again: the mirror cloned anew at the
//     provider's head;
//   - delete_workspace, after which the claim and the volume go.
//
// A fetch is read on the volume, never from the provider: git fetch writes a
// mirror's FETCH_HEAD, which a clone and an unchanged mirror never touch.
// The scratch branch, the workspace and the host's clone are removed on every
// path; the default branches are never written.

// WorkspacesPartSync is the sync part of workspaces-test.
const WorkspacesPartSync = "sync"

const (
	syncWorkspaceName = "agentlab-sync"
	// syncSession is the session directory the part lays out on the volume,
	// as a Session's would be.
	syncSession = "agentlab-sync-proof"
	// syncFetchTries bounds the cycles the part requests after a push until
	// one fetches it: the provider's listing may report the push a little
	// after git accepted it.
	syncFetchTries = 5
	syncFetchPause = 15 * time.Second
	// The volume probe's words.
	probeFetchedKey = "fetched"
	probeBranchKey  = "branch"
	probeNone       = "none"
)

// syncPart is the part's state.
type syncPart struct {
	sp     *syncProof
	pr     *proofRepos
	admin  *workspacePerson
	detail *syncDetail
	// stamps are each mirror's probe values after the last cycle.
	stamps map[string]map[string]string
}

// workspacesSyncPart runs the part in stages on the instance.
func workspacesSyncPart(cfg *config.Config, instance string, timeout time.Duration) error {
	p := newProofStages()
	sp := &syncPart{sp: newSyncProof(cfg, timeout, syncWorkspaceName)}
	err := sp.run(instance, p)
	sp.cleanup()
	if err == nil && sp.pr != nil {
		fmt.Printf("\nWorkspaces sync part passed on %s: a workspace over %s and %s defined through the workspace-manager's tools, its mirrors written once, a cycle with nothing changed fetching nothing, a push and a force-push fetched alone with the borrowed objects kept, a dropped repository's mirror kept while borrowed and deleted after, the update re-resolved and the volume gone with the workspace.\n", instance, sp.pr.pushed, sp.pr.still)
	}
	return p.close(err)
}

// syncStages are the part's stages after its preconditions, skipped
// together without the instance's proof repositories.
var syncStages = []string{"sync: first cycle", "sync: no-op", "sync: push", "sync: force-push", "sync: dropped", "sync: borrower gone", "sync: update", "sync: delete"}

func (sp *syncPart) run(instance string, p *proofStages) error {
	s, cfg := sp.sp, sp.sp.cfg
	p.begin("sync: preconditions", "The sync part's preconditions on %s: its proof repositories, the workspace-manager through muster, the admin signed in holding the instance's grant", instance)
	if skip := proofReposSkip(cfg, instance); skip != "" {
		p.skip("%s", skip)
		p.leaveOut(skip, syncStages...)
		return nil
	}
	if err := sp.preconditions(instance); err != nil {
		return err
	}
	pr := sp.pr

	p.begin("sync: first cycle", "create_workspace %s over %s and %s as %s; its first cycle: a bare mirror per repository at the provider's head, the manifest naming each", syncWorkspaceName, pr.pushed, pr.still, s.memberEmail)
	if err := sp.firstCycle(); err != nil {
		return err
	}

	p.begin("sync: no-op", "sync_workspace %s with nothing changed: no sync Job, no mirror fetched, the manifest unchanged", syncWorkspaceName)
	if err := sp.noOp(); err != nil {
		return err
	}

	p.begin("sync: push", "A push to %s's %s, then a cycle that fetches only it", pr.pushed, proofBranch)
	pushed, err := pr.push(pr.start, false)
	if err != nil {
		return err
	}
	if err := sp.fetchedAlone(pushed); err != nil {
		return err
	}

	p.begin("sync: force-push", "A session directory borrowing %s's mirror, a force-push over %s, then a cycle after which its clone reads every object", pr.pushed, shortSHA(pushed))
	if err := sp.forcePush(pushed); err != nil {
		return err
	}

	p.begin("sync: dropped", "update_workspace %s selecting %s alone: %s leaves the selection, its mirror stays while %s borrows from it", syncWorkspaceName, pr.still, pr.pushed, syncSession)
	if err := sp.dropped(); err != nil {
		return err
	}

	p.begin("sync: borrower gone", "The session directory removed: the next cycle deletes the dropped mirror of %s", pr.pushed)
	if err := sp.borrowerGone(); err != nil {
		return err
	}

	p.begin("sync: update", "update_workspace %s selecting %s and %s again: the mirror of %s cloned anew", syncWorkspaceName, pr.pushed, pr.still, pr.pushed)
	if err := sp.reselect(); err != nil {
		return err
	}

	p.begin("sync: delete", "delete_workspace %s: no session directory left, the claim and the volume go; the scratch branch deleted", syncWorkspaceName)
	if err := s.delete(syncWorkspaceName); err != nil {
		return err
	}
	return pr.restore()
}

// preconditions fails fast without what the part needs: the
// workspace-manager registered with muster and reachable, the admin signed
// in and holding the instance's grant, the pushed repository cloned on this
// host; then it removes what an interrupted run left.
func (sp *syncPart) preconditions(instance string) error {
	s, cfg := sp.sp, sp.sp.cfg
	if reason, err := workspaceManagerSkip(cfg); err != nil {
		return err
	} else if reason != "" {
		return fmt.Errorf("no workspace-manager to define a workspace with: %s", reason)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
	}
	admin := cfg.AdminUser()
	if admin == nil {
		return fmt.Errorf("the sync part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	var err error
	if sp.admin, err = openWorkspacePerson(cfg, admin); err != nil {
		return err
	}
	s.member, s.memberEmail = sp.admin.session, admin.Email
	if err := grantProofProvider(cfg, sp.admin, instance, s.timeout); err != nil {
		return err
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the volume probes' git image (%s)", res.d)
	}
	if err := s.removeLeftovers(); err != nil {
		return err
	}
	if sp.pr, err = newProofRepos(cfg, instance); err != nil {
		return err
	}
	if _, err := sp.pr.clone(); err != nil {
		return err
	}
	note("%s cloned on this host at %s, to push to its scratch branch %s; %s stays as it is", sp.pr.pushed, shortSHA(sp.pr.start), proofBranch, sp.pr.still)
	return nil
}

// firstCycle creates the workspace, waits for its first cycle and judges
// the mirrors against the provider; a requested cycle then settles the
// listing the first one may have read from the manager's cache.
func (sp *syncPart) firstCycle() error {
	s, pr := sp.sp, sp.pr
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: syncWorkspaceName, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: pr.sources(pr.repos()...),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(syncWorkspaceName, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s, want %s: %s", syncWorkspaceName, r.Result, syncResultSucceed, r.Message)
	}
	if live := d.live(); !slices.Equal(live, pr.repos()) {
		return fmt.Errorf("the first cycle of %s mirrored %v, want %v", syncWorkspaceName, live, pr.repos())
	}
	stamps, err := sp.probe()
	if err != nil {
		return err
	}
	for _, repo := range pr.repos() {
		head, err := pr.defaultHead(repo)
		if err != nil {
			return err
		}
		m, got := d.mirror(repo), stamps[repo]
		switch {
		case m.Head != head:
			return fmt.Errorf("the manifest names %s for %s, the provider's default branch is at %s", shortSHA(m.Head), repo, shortSHA(head))
		case got[probeHeadKey] != head:
			return fmt.Errorf("the mirror of %s on the volume is at %s, the provider's default branch at %s", repo, got[probeHeadKey], shortSHA(head))
		case got["gc.pruneExpire"] != "never":
			return fmt.Errorf("the mirror of %s has gc.pruneExpire %q, want never: its unreachable objects are not kept for the sync", repo, got["gc.pruneExpire"])
		}
	}
	v, err := s.volume(syncWorkspaceName)
	if err != nil {
		return err
	}
	note("created %s of Organization %s: result %s (trigger %s, sync job %s), claim %s/%s on %s", syncWorkspaceName, created.Organization, d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, d.Workspace.LastSync.Job, v.namespace, v.claim, v.pv)
	note("%s", indent(mirrorStampLines(stamps), "  "))
	s.printManifest(d)
	if sp.detail, err = sp.sync(d); err != nil {
		return err
	}
	sp.stamps, err = sp.probe()
	if err == nil {
		note("a requested cycle settles the listing: result %s", sp.detail.Workspace.LastSync.Result)
	}
	return err
}

// noOp requests a cycle with nothing changed.
func (sp *syncPart) noOp() error {
	s := sp.sp
	before, err := sp.manifest()
	if err != nil {
		return err
	}
	d, err := sp.sync(sp.detail)
	if err != nil {
		return err
	}
	after, err := sp.manifest()
	if err != nil {
		return err
	}
	stamps, err := sp.probe()
	if err != nil {
		return err
	}
	if err := judgeNoOp(d.Workspace.LastSync, before, after, fetchedSince(sp.stamps, stamps)); err != nil {
		return err
	}
	sp.detail, sp.stamps = d, stamps
	note("result %s (trigger %s, no sync job); no mirror fetched; the manifest byte for byte the same (%d bytes)", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, len(after))
	s.printManifest(d)
	return nil
}

// judgeNoOp is the verdict on a cycle with nothing changed: NoChange with no
// sync Job, no mirror fetched, the manifest byte for byte the one before.
func judgeNoOp(last *syncLastSync, before, after string, fetched []string) error {
	switch {
	case last.Result != syncResultNoChange || last.Job != "":
		return fmt.Errorf("the cycle with nothing changed ended %s with sync job %q, want %s and no sync job", last.Result, last.Job, syncResultNoChange)
	case len(fetched) > 0:
		return fmt.Errorf("a cycle with nothing changed fetched %s: their FETCH_HEAD moved", strings.Join(fetched, ", "))
	case before != after:
		return fmt.Errorf("the manifest changed in a cycle with nothing changed:\nbefore: %s\nafter:  %s", excerpt(before, 400), excerpt(after, 400))
	}
	return nil
}

// fetchedAlone requests cycles until one fetches the pushed repository with
// the scratch branch at commit, and judges that no cycle fetched the other.
func (sp *syncPart) fetchedAlone(commit string) error {
	s, pr := sp.sp, sp.pr
	for try := 1; ; try++ {
		d, err := sp.sync(sp.detail)
		if err != nil {
			return err
		}
		stamps, err := sp.probe()
		if err != nil {
			return err
		}
		fetched := fetchedSince(sp.stamps, stamps)
		prev := sp.detail
		sp.detail, sp.stamps = d, stamps
		if slices.Contains(fetched, pr.still) {
			return fmt.Errorf("the cycle after the push to %s fetched %s too, which did not change (manifest last push %s, was %s)", pr.pushed, pr.still, pushedAt(d, pr.still), pushedAt(prev, pr.still))
		}
		if slices.Contains(fetched, pr.pushed) {
			if got := stamps[pr.pushed][probeBranchKey]; got != commit {
				return fmt.Errorf("the mirror of %s fetched, its %s at %s, want the pushed %s", pr.pushed, proofBranch, got, shortSHA(commit))
			}
			if err := sp.headsKept(prev, d); err != nil {
				return err
			}
			note("cycle %d after the push: result %s (sync job %s); fetched %s alone, its %s at %s; %s untouched", try, d.Workspace.LastSync.Result, d.Workspace.LastSync.Job, pr.pushed, proofBranch, shortSHA(commit), pr.still)
			note("%s", indent(mirrorStampLines(stamps), "  "))
			s.printManifest(d)
			return nil
		}
		if try == syncFetchTries {
			return fmt.Errorf("%d cycles after the push to %s's %s fetched no mirror (last result %s; manifest last push %s): the provider's listing never reported the push", try, pr.pushed, proofBranch, d.Workspace.LastSync.Result, pushedAt(d, pr.pushed))
		}
		note("cycle %d after the push: result %s, nothing fetched yet (the provider's listing may lag the push); again in %s", try, d.Workspace.LastSync.Result, syncFetchPause)
		time.Sleep(syncFetchPause)
	}
}

// headsKept checks no default branch moved between two cycles: the pushes
// go to the scratch branch alone.
func (sp *syncPart) headsKept(prev, d *syncDetail) error {
	for _, m := range prev.Mirrors {
		if now := d.mirror(m.Repository); now == nil || now.Head != m.Head {
			return fmt.Errorf("the default branch of %s moved from %s to %v in a cycle after a push to %s alone", m.Repository, shortSHA(m.Head), now, proofBranch)
		}
	}
	return nil
}

func pushedAt(d *syncDetail, repo string) string {
	if m := d.mirror(repo); m != nil {
		return m.PushedAt.UTC().Format(time.RFC3339)
	}
	return probeNone
}

// forcePush lays out the session directory borrowing the pushed mirror,
// force-pushes over overwritten and judges the clone after the cycle.
func (sp *syncPart) forcePush(overwritten string) error {
	s, pr := sp.sp, sp.pr
	borrowed, err := s.onVolume(syncWorkspaceName, "borrow", syncBorrowScript(pr.pushed))
	if err != nil {
		return err
	}
	if h := probeValue(borrowed, probeBranchKey); h != overwritten {
		return fmt.Errorf("the session directory's clone has %s at %s, want the mirror's %s:\n%s", proofBranch, h, shortSHA(overwritten), borrowed)
	}
	note("%s", indent(borrowed, "  "))
	forced, err := pr.push(pr.start, true)
	if err != nil {
		return err
	}
	if err := sp.fetchedAlone(forced); err != nil {
		return err
	}
	if !slices.ContainsFunc(sp.detail.SessionDirectories, func(d syncSessionDirectory) bool { return d.Session == syncSession }) {
		return fmt.Errorf("get_workspace lists no session directory %s after the cycle", syncSession)
	}
	out, err := s.onVolume(syncWorkspaceName, "borrowed", syncBorrowedScript(pr.pushed, overwritten))
	if err != nil {
		return err
	}
	if probeValue(out, "mirror-branch") != forced {
		return fmt.Errorf("the mirror's %s is not the force-pushed %s:\n%s", proofBranch, shortSHA(forced), out)
	}
	note("get_workspace lists the session directory %s", syncSession)
	note("%s", indent(out, "  "))
	return nil
}

// dropped updates the workspace to the other repository alone and judges
// the dropped mirror kept while the session directory borrows from it.
func (sp *syncPart) dropped() error {
	s, pr := sp.sp, sp.pr
	d, err := sp.update(pr.still)
	if err != nil {
		return err
	}
	if m := d.mirror(pr.pushed); m == nil || m.DroppedAt == nil {
		return fmt.Errorf("the manifest after the update does not mark %s dropped: %v", pr.pushed, syncMirrorLines(d.Mirrors))
	}
	if live := d.live(); !slices.Equal(live, []string{pr.still}) || d.Workspace.Repositories != 1 {
		return fmt.Errorf("after the update the workspace selects %v (%d repositories), want %s alone", live, d.Workspace.Repositories, pr.still)
	}
	out, err := s.onVolume(syncWorkspaceName, "dropped", syncMirrorPresenceScript(pr.pushed))
	if err != nil {
		return err
	}
	if probeValue(out, "mirror") != "present" || !strings.Contains(out, "borrower="+syncSession) {
		return fmt.Errorf("the dropped mirror of %s is not on the volume with its borrower %s:\n%s", pr.pushed, syncSession, out)
	}
	note("result %s (trigger %s); %d repository selected, %s dropped and kept", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, d.Workspace.Repositories, pr.pushed)
	note("%s", indent(out, "  "))
	s.printManifest(d)
	return nil
}

// borrowerGone removes the session directory and judges the next cycle
// deletes the dropped mirror.
func (sp *syncPart) borrowerGone() error {
	s, pr := sp.sp, sp.pr
	out, err := s.onVolume(syncWorkspaceName, "unborrow", syncRemoveSessionScript)
	if err != nil {
		return err
	}
	note("%s", indent(out, "  "))
	d, err := sp.sync(sp.detail)
	if err != nil {
		return err
	}
	sp.detail = d
	if d.mirror(pr.pushed) != nil {
		return fmt.Errorf("the manifest still records %s after its last borrower went: %v", pr.pushed, syncMirrorLines(d.Mirrors))
	}
	if out, err = s.onVolume(syncWorkspaceName, "unborrowed", syncMirrorPresenceScript(pr.pushed)); err != nil {
		return err
	}
	if probeValue(out, "mirror") != probeAbsent {
		return fmt.Errorf("the mirror of %s is still on the volume after its last borrower went:\n%s", pr.pushed, out)
	}
	note("result %s (trigger %s, sync job %s); the mirror of %s gone", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, cmp.Or(d.Workspace.LastSync.Job, probeNone), pr.pushed)
	s.printManifest(d)
	return nil
}

// reselect updates the workspace to both repositories again and judges the
// pushed one's mirror cloned anew at the provider's head.
func (sp *syncPart) reselect() error {
	s, pr := sp.sp, sp.pr
	d, err := sp.update(pr.repos()...)
	if err != nil {
		return err
	}
	if live := d.live(); !slices.Equal(live, pr.repos()) {
		return fmt.Errorf("after the update the manifest holds %v, want %v", syncMirrorLines(d.Mirrors), pr.repos())
	}
	stamps, err := sp.probe()
	if err != nil {
		return err
	}
	head, err := pr.defaultHead(pr.pushed)
	if err != nil {
		return err
	}
	got := stamps[pr.pushed]
	switch {
	case got[probeHeadKey] != head:
		return fmt.Errorf("the mirror of %s cloned anew is at %s, the provider's default branch at %s", pr.pushed, got[probeHeadKey], shortSHA(head))
	case got[probeFetchedKey] != "0":
		return fmt.Errorf("the mirror of %s carries a FETCH_HEAD: the update fetched into the old mirror instead of cloning anew", pr.pushed)
	}
	note("result %s (trigger %s); %s cloned anew at %s", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, pr.pushed, shortSHA(head))
	s.printManifest(d)
	return nil
}

// update replaces the workspace's sources with repos and waits for the
// cycle the update starts.
func (sp *syncPart) update(repos ...string) (*syncDetail, error) {
	s := sp.sp
	var updated syncWorkspace
	if err := s.member.callServerStructured(workspaceManagerToolPrefix+"update_workspace", map[string]any{
		nameKey: syncWorkspaceName, wmSourcesArg: sp.pr.sources(repos...),
	}, &updated); err != nil {
		return nil, err
	}
	d, err := s.waitCycle(syncWorkspaceName, sp.detail.Workspace.LastSync)
	if err != nil {
		return nil, err
	}
	sp.detail = d
	return d, nil
}

// sync requests a cycle and waits for its end; a requested cycle lists the
// provider anew.
func (sp *syncPart) sync(prev *syncDetail) (*syncDetail, error) {
	if err := requestSync(sp.sp.member, syncWorkspaceName); err != nil {
		return nil, err
	}
	return sp.sp.waitCycle(syncWorkspaceName, prev.Workspace.LastSync)
}

// requestSync is sync_workspace on the workspace as the session's person.
func requestSync(s *musterSession, workspace string) error {
	var requested struct {
		RequestedAt time.Time `json:"requestedAt"`
	}
	return s.callServerStructured(workspaceManagerToolPrefix+"sync_workspace", map[string]any{wmWorkspaceArg: workspace}, &requested)
}

// manifest is the workspace's manifest as the sync wrote it, the ConfigMap
// beside its claim.
func (sp *syncPart) manifest() (string, error) {
	v, err := sp.sp.volume(syncWorkspaceName)
	if err != nil {
		return "", err
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cm, err := k.clientset.CoreV1().ConfigMaps(v.namespace).Get(ctx, "workspace-"+syncWorkspaceName+"-manifest", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the manifest of workspace %s: %w", syncWorkspaceName, err)
	}
	return cm.Data[syncManifestKey], nil
}

// probe reads every live mirror on the volume.
func (sp *syncPart) probe() (map[string]map[string]string, error) {
	out, err := sp.sp.onVolume(syncWorkspaceName, "mirrors", syncStampScript(sp.pr.repos()))
	if err != nil {
		return nil, err
	}
	return parseMirrorProbe(out), nil
}

// syncStampScript reports each mirror present on the volume: bare, its
// default branch's head, the scratch branch, when git fetch last wrote its
// FETCH_HEAD (0 for a mirror never fetched since its clone) and the
// configuration that keeps its objects to the sync's own garbage collection.
func syncStampScript(repos []string) string {
	var b strings.Builder
	b.WriteString(workspacesGitSetup)
	for _, r := range repos {
		fmt.Fprintf(&b, `m=/workspace/%[1]s/%[2]s.git
if test -d $m; then
test "$(git -C $m rev-parse --is-bare-repository)" = true
echo "%[2]s bare=true %[3]s=$(git -C $m rev-parse HEAD) %[4]s=$(git -C $m rev-parse -q --verify refs/heads/%[5]s || echo %[6]s) %[7]s=$(test -f $m/FETCH_HEAD && date -r $m/FETCH_HEAD +%%s%%N || echo 0) gc.auto=$(git -C $m config gc.auto) gc.pruneExpire=$(git -C $m config gc.pruneExpire)"
fi
`, workspacesTestMirrors, r, probeHeadKey, probeBranchKey, proofBranch, probeNone, probeFetchedKey)
	}
	return b.String()
}

// fetchedSince are the repositories whose FETCH_HEAD moved between two
// probes, sorted; a mirror only after counts when it carries one.
func fetchedSince(before, after map[string]map[string]string) []string {
	var out []string
	for repo, a := range after {
		stamp := a[probeFetchedKey]
		if stamp == "" || stamp == "0" {
			continue
		}
		if b, ok := before[repo]; !ok || b[probeFetchedKey] != stamp {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// mirrorStampLines word a probe, one mirror a line.
func mirrorStampLines(stamps map[string]map[string]string) string {
	var out []string
	for _, repo := range slices.Sorted(maps.Keys(stamps)) {
		v := stamps[repo]
		fetched := "never fetched"
		if ns, err := strconv.ParseInt(v[probeFetchedKey], 10, 64); err == nil && ns > 0 {
			fetched = "fetched " + time.Unix(0, ns).UTC().Format(time.RFC3339)
		}
		out = append(out, fmt.Sprintf("%s: bare, default branch at %s, %s %s, %s, gc.pruneExpire=%s", repo, shortSHA(v[probeHeadKey]), proofBranch, shortSHA(v[probeBranchKey]), fetched, v["gc.pruneExpire"]))
	}
	return strings.Join(out, "\n")
}

// The session directory on the volume and its clone of the pushed
// repository.
const (
	syncSessionDir   = "/workspace/" + workspacesTestSessions + "/" + syncSession
	syncSessionClone = syncSessionDir + "/clone"
)

func syncMirrorDir(repo string) string {
	return "/workspace/" + workspacesTestMirrors + "/" + repo + ".git"
}

// syncBorrowScript lays out the session directory the way a Session's clone
// is: `git clone --shared` of the mirror, borrowing its objects through git
// alternates.
func syncBorrowScript(repo string) string {
	return workspacesGitSetup + `mkdir -p ` + syncSessionDir + `
git clone -q --shared --no-checkout ` + syncMirrorDir(repo) + ` ` + syncSessionClone + `
echo "alternates=$(cat ` + syncSessionClone + `/.git/objects/info/alternates)"
echo "` + probeBranchKey + `=$(git -C ` + syncSessionClone + ` rev-parse refs/remotes/origin/` + proofBranch + `)"
echo "own-objects=$(find ` + syncSessionClone + `/.git/objects -type f ! -path '*/info/*' | wc -l)"
`
}

// syncBorrowedScript proves the session directory's clone reads every
// object after the force-push overwrote overwritten in the mirror: the
// mirror's scratch branch at its new head, the overwritten commit still in
// the mirror and no longer on the branch, and every object the clone
// reaches readable.
func syncBorrowedScript(repo, overwritten string) string {
	return workspacesGitSetup + `c=` + syncSessionClone + `
m=` + syncMirrorDir(repo) + `
echo "mirror-branch=$(git -C $m rev-parse refs/heads/` + proofBranch + `)"
git -C $m cat-file -e ` + overwritten + `^{commit}
if git -C $m merge-base --is-ancestor ` + overwritten + ` refs/heads/` + proofBranch + `; then echo "the overwritten commit is still on the branch: no force-push"; exit 1; fi
echo "mirror-keeps=` + overwritten + `"
missing=$(git -C $c rev-list --objects --all | cut -d' ' -f1 | git -C $c cat-file --batch-check | grep -c ' missing$' || true)
echo "objects=$(git -C $c rev-list --objects --all | wc -l) missing=$missing"
test "$missing" = 0
git -C $c fsck --connectivity-only --no-dangling
git -C $c cat-file -e ` + overwritten + `^{tree}
echo "clone-reads=` + overwritten + `"
`
}

// syncMirrorPresenceScript says whether the repository's mirror is on the
// volume and which session directories borrow from it.
func syncMirrorPresenceScript(repo string) string {
	return `if test -d ` + syncMirrorDir(repo) + `; then echo "mirror=present"; else echo "mirror=absent"; fi
for a in /workspace/` + workspacesTestSessions + `/*/*/.git/objects/info/alternates; do test -f "$a" && echo "borrower=$(echo "$a" | cut -d/ -f4) alternates=$(cat "$a")"; done
true
`
}

// syncRemoveSessionScript removes the session directory, as its Session's
// end would.
const syncRemoveSessionScript = `rm -rf ` + syncSessionDir + `
echo "sessions: $(ls -A /workspace/` + workspacesTestSessions + ` 2>/dev/null | wc -l) directories left"
`

// cleanup removes what the part created, on every path.
func (sp *syncPart) cleanup() {
	var errs []string
	if err := sp.sp.restore(); err != nil {
		errs = append(errs, err.Error())
	}
	if sp.pr != nil {
		if err := sp.pr.restore(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		warn("removing what the part left: %s", strings.Join(errs, "; "))
	}
}
