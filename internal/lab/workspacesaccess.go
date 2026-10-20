package lab

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// A Session's start on a workspace, as the workspace proofs read it: the
// workspace-manager's grant decoded and judged, and agent-manager's
// start_session called as the person with it.

const (
	// accessDefaultSecond is the gitlab part's default person, a developer
	// of the Organization.
	accessDefaultSecond = "dev@lab.local"
	// The outcomes of authorize_workspace.
	authorizeGranted         = "granted"
	authorizeConnectRequired = "connect_required"
	authorizeRefused         = "refused"
	// accessFirstMessage is start_session's first message on the admitted
	// Session: one short turn.
	accessFirstMessage = "Reply with the single word ready."
)

// accessGrantClaims are the claims of a manager's grant the part judges.
type accessGrantClaims struct {
	Subject        string            `json:"sub"`
	Expiry         int64             `json:"exp"`
	AgentNamespace string            `json:"agent_namespace"`
	Workspace      map[string]string `json:"workspace"`
	Volume         workspaceVolume   `json:"volume"`
	Mounts         []workspaceMount  `json:"mounts"`
	Changed        []string          `json:"changed"`
}

// grantPayload decodes a grant's claims without verifying it, generically
// (to sign them again with a claim changed) and as the claims judged.
func grantPayload(grant string) (map[string]any, accessGrantClaims, error) {
	parts := strings.Split(grant, ".")
	if len(parts) != 3 {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant is no compact JWS (%d parts)", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("decoding the grant's payload: %w", err)
	}
	var generic map[string]any
	var claims accessGrantClaims
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant's payload: %w", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant's claims: %w", err)
	}
	return generic, claims, nil
}

// judgeGrant is the verdict on the admin's grant: it names the person, the
// namespace of the lab's Agents, the workspace, its volume, the workspace's mounts and exactly the changed
// repositories.
func judgeGrant(c accessGrantClaims, person, workspace string, volume workspaceVolume, changed []string) error {
	var problems []string
	if c.Subject != person {
		problems = append(problems, fmt.Sprintf("names %q, not %s", c.Subject, person))
	}
	if c.AgentNamespace != kagentNamespace {
		problems = append(problems, fmt.Sprintf("names Agent namespace %q, not %s (agent_namespace, workspace-manager 0.9.0 or newer)", c.AgentNamespace, kagentNamespace))
	}
	if c.Workspace[nameKey] != workspace {
		problems = append(problems, fmt.Sprintf("names workspace %q, not %s", c.Workspace[nameKey], workspace))
	}
	if c.Volume != volume {
		problems = append(problems, fmt.Sprintf("names volume %s %s, not the workspace's %s %s", c.Volume.Driver, c.Volume.Handle, volume.Driver, volume.Handle))
	}
	want := []workspaceMount{{SubPath: workspaceGrantSessionDirectory}, {SubPath: workspaceGrantMirrors, ReadOnly: true}}
	if !slices.Equal(c.Mounts, want) {
		problems = append(problems, fmt.Sprintf("allows mounts %v, not the Session's own directory and the mirrors read-only", c.Mounts))
	}
	got := slices.Clone(c.Changed)
	slices.Sort(got)
	if !slices.Equal(got, changed) {
		problems = append(problems, fmt.Sprintf("names %v changed since the sync, want %v", c.Changed, changed))
	}
	if len(problems) > 0 {
		return fmt.Errorf("the grant %s", strings.Join(problems, "; "))
	}
	return nil
}

// accessStarted is start_session's answer, the fields the part reads.
type accessStarted struct {
	Session struct {
		ID      string `json:"id"`
		Creator string `json:"creator"`
	} `json:"session"`
	Workspace string `json:"workspace"`
}

// startSession calls agent-manager's start_session as the person, with the
// grant when one is given.
func startSession(p *workspacePerson, grant string) (*toolEnvelope, error) {
	args := map[string]any{nameKey: workspacesHarnessAgent, namespaceKey: kagentNamespace, "message": accessFirstMessage, startSessionRequestIDArg: uuid.NewString()}
	if grant != "" {
		args[startSessionGrantArg] = grant
	}
	return p.session.callToolEnvelope(agentManagerToolPrefix+"start_session", args)
}

// accessClearSessionsScript removes every session directory from the
// workspace's volume, so its delete is not held by one.
const accessClearSessionsScript = `rm -rf /workspace/` + workspacesTestSessions + `/*
echo "sessions: $(ls -A /workspace/` + workspacesTestSessions + ` 2>/dev/null | wc -l) directories left"
`
