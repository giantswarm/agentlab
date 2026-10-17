package lab

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// The state the workspace proofs leave on a lab, and the check that a run
// starts from none of it. Every proof creates its Workspaces through
// createLabWorkspace, which labels them as the lab's fixture of this run;
// every workspace-manager grant a proof uses goes into the run's grant
// ledger and is dropped when the run ends (releaseWorkspaceGrants), whatever
// the outcome. A run's start removes what an earlier run left
// (sweepWorkspaceLeftovers): the lab's labelled Workspaces of another run,
// and the Dev Portal's Playwright fixtures (`e2e-ws-<run>-…`, unlabelled,
// made through the portal) of a run that started more than
// e2eWorkspaceLeftoverAge ago — the portal suite's own rule, so a suite
// running at the same time keeps its own.

const (
	// workspaceFixtureValue is fixtureLabel's value on a Workspace a proof
	// created; workspaceRunLabel names the run that created it.
	workspaceFixtureValue = "workspace"
	workspaceRunLabel     = "agentlab.giantswarm.io/run"
	// e2eWorkspacePrefix names the Dev Portal suite's fixture Workspaces,
	// e2eWorkspaceLeftoverAge its LEFTOVER_AGE (e2e/agentlab/lab.ts).
	e2eWorkspacePrefix      = "e2e-ws-"
	e2eWorkspaceLeftoverAge = 3 * time.Hour
	// workspaceSyncRequested is the manager's sync request annotation: on a
	// deleted Workspace it runs the deletion cycle at once, as a second
	// delete_workspace does.
	workspaceSyncRequested   = "workspace-manager.giantswarm.io/sync-requested"
	workspaceSyncRequestedBy = "workspace-manager.giantswarm.io/sync-requested-by"
	// workspaceLeftoverWait bounds the wait for the leftovers to go.
	workspaceLeftoverWait = time.Minute
)

// labRunID is this run's id, its start in base-36 milliseconds: the shape
// the portal suite names its runs in, so either reads the other's age.
var labRunID = strconv.FormatInt(time.Now().UnixMilli(), 36)

// createLabWorkspace is create_workspace as the session's person, the
// Workspace then labelled as this run's fixture: the manager takes no
// labels, so the label is the lab's own metadata on its object.
func (s *musterSession) createLabWorkspace(args map[string]any, into any) error {
	if err := s.callServerStructured(workspaceManagerToolPrefix+"create_workspace", args, into); err != nil {
		return err
	}
	name, _ := args[nameKey].(string)
	return labelLabWorkspace(context.Background(), name)
}

// labelLabWorkspace marks the Workspace as this run's fixture.
func labelLabWorkspace(ctx context.Context, name string) error {
	gvr, err := gvrFor(workspaceResource)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q,%q:%q}}}`, fixtureLabel, workspaceFixtureValue, workspaceRunLabel, labRunID)
	return patchObject(ctx, gvr, kagentNamespace, name, types.MergePatchType, []byte(patch))
}

// workspaceLeftover is a Workspace an earlier run left, and why it counts.
type workspaceLeftover struct {
	name, why string
}

// workspaceLeftovers picks the Workspaces an earlier run left: the lab's
// fixtures of another run, and the portal suite's of a run older than
// e2eWorkspaceLeftoverAge. Everything else — a person's own Workspace, a
// fixture of this run, a portal run going on — stays.
func workspaceLeftovers(items []unstructured.Unstructured, run string, now time.Time) []workspaceLeftover {
	var out []workspaceLeftover
	for _, ws := range items {
		labels := ws.GetLabels()
		switch {
		case labels[fixtureLabel] == workspaceFixtureValue:
			if labels[workspaceRunLabel] != run {
				out = append(out, workspaceLeftover{ws.GetName(), "the lab's fixture of run " + cmp.Or(labels[workspaceRunLabel], "(none)")})
			}
		case strings.HasPrefix(ws.GetName(), e2eWorkspacePrefix):
			started := e2eRunStart(ws.GetName(), ws.GetCreationTimestamp().Time, now)
			if age := now.Sub(started); age > e2eWorkspaceLeftoverAge {
				out = append(out, workspaceLeftover{ws.GetName(), fmt.Sprintf("the portal suite's fixture of a run started %s ago", age.Round(time.Minute))})
			}
		}
	}
	slices.SortFunc(out, func(a, b workspaceLeftover) int { return strings.Compare(a.name, b.name) })
	return out
}

// e2eRunStart reads when the portal run that named the Workspace started:
// its run id where that reads as a time between 2020 and an hour from now
// (the suite's runStart), else the Workspace's creation.
func e2eRunStart(name string, created, now time.Time) time.Time {
	run, _, _ := strings.Cut(strings.TrimPrefix(name, e2eWorkspacePrefix), "-")
	if ms, err := strconv.ParseInt(run, 36, 64); err == nil {
		t := time.UnixMilli(ms)
		if t.After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) && t.Before(now.Add(time.Hour)) {
			return t
		}
	}
	return created
}

// sweepWorkspaceLeftovers is a run's start on the workspaces: what an
// earlier run left is deleted and its deletion cycle requested at once, then
// waited for; one a Session's directory still holds stays deleting, which no
// prompt or picker offers. A lab without the workspace-manager has nothing to
// sweep.
func sweepWorkspaceLeftovers(ctx context.Context) error {
	gvr, err := gvrFor(workspaceResource)
	if err != nil {
		return nil //nolint:nilerr // no Workspace kind: no workspace-manager, nothing left
	}
	items, err := listObjects(ctx, gvr, kagentNamespace, "")
	if err != nil {
		return err
	}
	left := workspaceLeftovers(items, labRunID, time.Now())
	if len(left) == 0 {
		return nil
	}
	request := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q,%q:"agentlab sweep"}}}`,
		workspaceSyncRequested, time.Now().UTC().Format(time.RFC3339), workspaceSyncRequestedBy))
	names := make([]string, 0, len(left))
	for _, l := range left {
		note("removing the leftover Workspace %s/%s: %s", kagentNamespace, l.name, l.why)
		if err := deleteObject(ctx, gvr, kagentNamespace, l.name, 0); err != nil {
			return err
		}
		if err := patchObject(ctx, gvr, kagentNamespace, l.name, types.MergePatchType, request); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		names = append(names, l.name)
	}
	var stuck []string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, workspaceLeftoverWait, true, func(ctx context.Context) (bool, error) {
		stuck = stuck[:0]
		for _, name := range names {
			if there, err := objectExists(ctx, gvr, kagentNamespace, name); err != nil {
				return false, err
			} else if there {
				stuck = append(stuck, name)
			}
		}
		return len(stuck) == 0, nil
	})
	if wait.Interrupted(err) {
		// Deleting, a Workspace is offered to no one (the gateway's prompt,
		// the portal's picker) and starts no Session; its manager removes it
		// once no Session holds a directory on its volume.
		note("still deleting after %s, offered to no one until its manager removes it: %s", workspaceLeftoverWait, strings.Join(stuck, ", "))
		return nil
	}
	return err
}

// workspaceGrantLedger holds the workspace-manager grants a run used, each
// person once, to drop when the run ends.
type workspaceGrantLedger struct {
	mu     sync.Mutex
	emails []string
	drops  map[string]func()
}

// runWorkspaceGrants is this run's ledger.
var runWorkspaceGrants = &workspaceGrantLedger{}

// record notes a grant the person holds for the run; the latest drop of a
// person wins (it rides their newest session).
func (l *workspaceGrantLedger) record(email string, drop func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.drops == nil {
		l.drops = map[string]func(){}
	}
	if _, ok := l.drops[email]; !ok {
		l.emails = append(l.emails, email)
	}
	l.drops[email] = drop
}

// release drops every recorded grant, in the order they were first
// recorded, and empties the ledger; it returns whose it dropped.
func (l *workspaceGrantLedger) release() []string {
	l.mu.Lock()
	emails, drops := l.emails, l.drops
	l.emails, l.drops = nil, nil
	l.mu.Unlock()
	for _, email := range emails {
		drops[email]()
	}
	return emails
}

// recordWorkspaceGrant puts the person's grant into the run's ledger.
func recordWorkspaceGrant(p *workspacePerson, instance string) {
	runWorkspaceGrants.record(p.user.Email, func() { p.disconnect(instance) })
}

// startWorkspaceRun is the start of a command that works on the
// workspaces: the leftovers of earlier runs swept, and the end that drops
// the run's grants, for the caller to defer — on the sweep's failure too,
// which leaves no grant to drop but keeps the call sites alike.
func startWorkspaceRun() (end func(), err error) {
	step("Starting from a clean lab: no Workspace an earlier run left (namespace %s)", kagentNamespace)
	return releaseWorkspaceGrants, sweepWorkspaceLeftovers(context.Background())
}

// releaseWorkspaceGrants ends a run on the workspaces: every grant of the
// workspace-manager's App a proof of the run used is dropped in muster, so
// the lab's people leave the run as they hold no grant — the portal's and
// the gateway's proofs start from the same state on a used lab as on a
// fresh one. Deferred by every command that grants.
func releaseWorkspaceGrants() {
	if dropped := runWorkspaceGrants.release(); len(dropped) > 0 {
		note("dropped the workspace-manager grant of %s in muster: a run leaves no grant behind", strings.Join(dropped, ", "))
	}
}
