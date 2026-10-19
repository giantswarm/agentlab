package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// workspaceSessionRig starts Sessions of an Agent through agent-manager's
// start_session as a person: in a workspace of one repository on the
// provider instance fake-bearer, with the person's grant for it, or without
// a workspace. The workspace and the Sessions are removed by cleanup.
type workspaceSessionRig struct {
	sp      *syncProof
	timeout time.Duration
	person  *workspacePerson
	api     *kagentAPI
	set     *sessionSet
	apiHost string
	// workspace and repo are the rig's workspace and its one repository.
	workspace, repo string
	// grantJudged says the grant must name the API host as a Bearer
	// provider of fake-bearer's audience.
	grantJudged bool
	// started is when the workspace Session's start_session was called:
	// the start of muster's log a proof reads.
	started time.Time
}

func newWorkspaceSessionRig(cfg *config.Config, timeout time.Duration, workspace, repo string, grantJudged bool) *workspaceSessionRig {
	return &workspaceSessionRig{sp: newSyncProof(cfg, timeout, workspace), timeout: timeout,
		apiHost: config.GitHubFakeBearerAPIHost(cfg.Platform.Domain), workspace: workspace, repo: repo, grantJudged: grantJudged}
}

// preconditions checks the Session admission and the workspace-manager's
// provider instance fake-bearer, and signs the person in; Sessions are read
// through api. The workspaces' own preconditions (sp.preconditions) are the
// caller's.
func (r *workspaceSessionRig) preconditions(user *config.User, api *kagentAPI) error {
	cfg := r.sp.cfg
	ctx := context.Background()
	r.api = api
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): a Session on a workspace volume needs it")
	}
	key, err := readWorkspaceGrantKey()
	if err != nil {
		return err
	}
	kid, err := proveWorkspaceManagerPublishesGrantKey(ctx, key)
	if err != nil {
		return err
	}
	if err := proveKagentAdmissionTrusts(ctx, rig); err != nil {
		return err
	}
	note("the manager publishes the lab's grant key as %s; kagent's admission trusts %s", kid, rig.JWKSURL)
	if r.person == nil {
		if r.person, err = openWorkspacePerson(cfg, user); err != nil {
			return err
		}
	}
	if _, err := r.person.providerStatus(config.WorkspaceProviderFakeBearer); err != nil {
		return fmt.Errorf("the workspace-manager has no provider instance %s: %w (`agentlab platform` renders it)", config.WorkspaceProviderFakeBearer, err)
	}
	r.set = &sessionSet{api: api, manager: r.person.session}
	return nil
}

// createWorkspace connects the person to fake-bearer and defines the
// workspace with its one repository, waiting for its first cycle.
func (r *workspaceSessionRig) createWorkspace() error {
	hops, err := connectWorkspaceProvider(r.sp.cfg, r.person, config.WorkspaceProviderFakeBearer)
	if err != nil {
		return err
	}
	note("%s connected to %s: %s", r.person.user.Email, config.WorkspaceProviderFakeBearer, hops)
	return r.defineWorkspace()
}

// defineWorkspace defines the workspace on fake-bearer with its one
// repository, waiting for its first cycle.
func (r *workspaceSessionRig) defineWorkspace() error {
	s := r.sp
	owner, name, _ := strings.Cut(r.repo, "/")
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: r.workspace, wmOrganizationArg: config.WorkspaceOrganization,
		wmSourcesArg: []map[string]any{{wmProviderArg: config.WorkspaceProviderFakeBearer, wmOwnerArg: owner, repositoriesKey: []string{name}}},
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(r.workspace, nil)
	if err != nil {
		return err
	}
	if res := d.Workspace.LastSync; res.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", r.workspace, res.Result, res.Message)
	}
	note("created %s of Organization %s on %s; first cycle %s", r.workspace, created.Organization, config.WorkspaceProviderFakeBearer, d.Workspace.LastSync.Result)
	return nil
}

// startSession starts a Session of the Agent through start_session, with
// the person's grant for the workspace or without a workspace, and returns
// it as kagent records it.
func (r *workspaceSessionRig) startSession(agent string, inWorkspace bool) (*apiv1alpha1.Session, error) {
	args := map[string]any{nameKey: agent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString()}
	if inWorkspace {
		a, err := r.person.authorizeWorkspace(r.workspace)
		if err != nil {
			return nil, err
		}
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return nil, fmt.Errorf("authorize_workspace %s answered %s (%q, unreadable %v, connect %v), want granted", r.workspace, a.Outcome, a.Reason, a.Unreadable, a.Connect)
		}
		var claims struct {
			Providers []grantProvider `json:"providers"`
		}
		parts := strings.Split(a.Grant, ".")
		if len(parts) != 3 || decodeJWTPart(parts[1], &claims) != nil {
			return nil, fmt.Errorf("the grant of authorize_workspace %s is no JWT", r.workspace)
		}
		if err := judgeGrantProviders(claims.Providers, r.apiHost, config.WorkspaceProviderFakeBearer); err != nil && r.grantJudged {
			return nil, fmt.Errorf("the grant for %s: %w", r.workspace, err)
		}
		note("the grant names the providers %s", grantProvidersLine(claims.Providers))
		args[startSessionGrantArg] = a.Grant
		r.started = time.Now()
	}
	env, err := r.person.session.callToolEnvelope(agentManagerToolPrefix+"start_session", args)
	if err != nil {
		return nil, err
	}
	if env.IsError {
		return nil, fmt.Errorf("start_session refused: %.400s", env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return nil, fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	r.set.add(started.Session.ID)
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	session, err := r.api.getSession(ctx, started.Session.ID)
	if err != nil {
		return nil, err
	}
	if !inWorkspace && session.GetVolumeSource() != nil {
		return nil, fmt.Errorf("the Session %s started without a workspace carries volume %q", session.GetId(), session.GetVolumeSource().GetVolume().GetVolumeHandle())
	}
	return session, nil
}

// musterLog reads muster's log since the workspace Session's start.
func (r *workspaceSessionRig) musterLog() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, time.Since(r.started)+time.Minute)
	if err != nil {
		return "", fmt.Errorf("reading muster's log: %w", err)
	}
	return logs, nil
}

// cleanup removes the Sessions, the Sessions' directories on the
// workspace's volume and the workspace, and puts the lab's GitHub back; it
// runs once.
func (r *workspaceSessionRig) cleanup() error {
	if r.sp.restored {
		return nil
	}
	var errs []error
	if r.set != nil {
		errs = append(errs, r.set.removeAllNow())
	}
	if r.sp.member != nil {
		if gone, err := r.sp.gone(r.workspace); err == nil && !gone {
			if _, err := r.sp.volume(r.workspace); err == nil {
				out, err := r.sp.onVolume(r.workspace, "sessions-clear", accessClearSessionsScript)
				errs = append(errs, err)
				if err == nil {
					note("%s", indent(out, "  "))
				}
			}
		}
	}
	errs = append(errs, r.sp.restore())
	return errors.Join(errs...)
}
