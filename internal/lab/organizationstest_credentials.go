package lab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The credentials part of the organizations proof (docs/agents.md "The
// credentials part"): an organization admin adds a model without its key
// through agent-manager over muster, the key is typed only into
// agent-manager's credential API the way the portal's form posts it, the
// save lands as a pull request opened as the person with the Secret
// SOPS-encrypted, Flux applies and decrypts it through the organization's
// own Kustomization, and the organization is erased. Beside it: what muster
// answers each MCP client generation for that missing key, and a tenant
// namespace's MCP server sign-in attributed to its key.
//
// Every prerequisite is read from the live lab and never configured here;
// a missing one is a SKIP that names its fix. The key typed is a random
// value generated in this process, printed only as its SHA-256.

const (
	organizationCredentialsPart = "credentials"
	// credentialsOrganization is the part's own organization, erased at the
	// end; credentialsModel the model it adds without a key, whose key
	// agent-manager names by default.
	credentialsOrganization = "agentlab-credentials"                  // #nosec G101 -- an organization name, not a credential
	credentialsModel        = "agentlab-credentials"                  // #nosec G101 -- a ModelConfig name, not a credential
	credentialsModelSecret  = "tenant-credential-" + credentialsModel // #nosec G101 -- a Secret's NAME, not a credential
	credentialsModelKey     = "apiKey"
	// credentialsSignInServer is the tenant namespace's MCPServer that needs
	// a per-server sign-in.
	credentialsSignInServer = "agentlab-credentials-signin" // #nosec G101 -- an MCPServer name, not a credential
	// credentialsRESTPort is agent-manager's listener: REST under /api/v1
	// and MCP, one port.
	credentialsRESTPort      = 8080
	credentialOperationsPath = "/api/v1/credential-operations/" // #nosec G101 -- a path, not a credential
	// credentialStateCommitted is a credential whose pull request exists.
	credentialStateCommitted = "committed"
	credentialStatePresent   = "present"
)

// Surfaces of the credentials part.
const (
	surfaceAgentManager  = "agent-manager through muster"
	surfaceCredentialAPI = "agent-manager credential API"
	surfaceGitHub        = "GitHub"
	surfaceFlux          = "Flux"
)

// credentialsProof is one run of the part.
type credentialsProof struct {
	cfg *config.Config
	// admin is the organization's admin, other a lab user who is not.
	admin, other *config.User
	adminToken   string
	otherToken   string
	namespace    string
	uid          string
	pre          credentialPrerequisites
	mode         string
	// value is the key typed into the form; valueSum its hash.
	value    string
	valueSum [sha256.Size]byte
	// plain is the generation without URL elicitation, kept open: the
	// retry that returns the receipt is its call again.
	plain                *musterMCP
	operation, formURL   string
	receipt              *credentialAnswer
	agentManagerVersion  string
	merged               bool
	verdicts             []organizationVerdict
	retainForDiagnostics bool
}

// organizationCredentialsTest runs the credentials part and prints its
// verdicts.
func organizationCredentialsTest(cfg *config.Config, admin *config.User) error {
	other := credentialsOtherUser(cfg, admin)
	if other == nil {
		return fmt.Errorf("the credentials part needs a lab user outside %s for the refused read of another person's operation; %s has none", platformAdminsGroup, config.File)
	}
	value := randomHex(32)
	p := &credentialsProof{cfg: cfg, admin: admin, other: other, value: value, valueSum: sha256.Sum256([]byte(value))}
	defer p.cleanup()
	if err := p.run(); err != nil {
		return err
	}
	fmt.Println()
	for _, v := range p.verdicts {
		fmt.Println(v.line())
	}
	return nil
}

// credentialsOtherUser is the first lab user outside platform-admins.
func credentialsOtherUser(cfg *config.Config, admin *config.User) *config.User {
	for i := range cfg.Users {
		u := &cfg.Users[i]
		if u.Email != admin.Email && !u.HasGroup(platformAdminsGroup) {
			return u
		}
	}
	return nil
}

func (p *credentialsProof) run() error {
	if err := p.setUp(); err != nil {
		return err
	}
	if err := p.readPrerequisites(); err != nil {
		return err
	}
	for _, c := range p.checks() {
		v, err := c.verdict()
		if err != nil {
			p.retainForDiagnostics = true
			return err
		}
		p.verdicts = append(p.verdicts, v)
	}
	return nil
}

// checks are the part's verdicts in the order they run: the erasure last,
// since every other check needs the organization.
func (p *credentialsProof) checks() []organizationCheck {
	uiSkip := p.pre.uiURLSkip()
	generations := firstNonEmpty(uiSkip, p.pre.generationsSkip())
	commit := firstNonEmpty(uiSkip, p.pre.commitSkip())
	flux := firstNonEmpty(commit, p.pre.fluxSkip())
	return []organizationCheck{
		{surface: surfaceAgentManager, boundary: "a model without a key is not written: no ModelConfig, no tenant credential value, a pending credential operation", skip: uiSkip, run: p.modelWithoutKey},
		{surface: surfaceAgentManager, boundary: "an MCP 2026-07-28 client declaring URL elicitation gets input_required, its message attributed to agent-manager by muster", skip: generations, run: p.modernGeneration},
		{surface: surfaceAgentManager, boundary: "an MCP 2025-11-25 client that declared URL elicitation gets -32042 carrying the form's URL", skip: generations, run: p.legacyGeneration},
		{surface: surfaceAgentManager, boundary: "a client without URL elicitation gets a result whose credentials[].next carries the form's link", skip: uiSkip, run: p.plainGeneration},
		p.resourceLinkCheck(uiSkip),
		{surface: surfaceCredentialAPI, boundary: "the operation's confirmation is the person's alone: another person's read is refused", skip: uiSkip, run: p.otherPersonRefused},
		{surface: surfaceCredentialAPI, boundary: "the form's save: GET with a strong ETag, POST with If-Match and only the requested pair returns the receipt", skip: uiSkip, run: p.save},
		{surface: surfaceGitHub, boundary: "the save opens a pull request as the person, its Secret SOPS-encrypted", skip: commit, run: p.pullRequest},
		{surface: surfaceFlux, boundary: "the merged pull request is applied and decrypted by the organization's Kustomization: the Secret's SHA-256 matches and the ModelConfig exists", skip: flux, run: p.fluxApplied},
		p.tenantSignInCheck(),
		{surface: surfaceAgentManager, boundary: "delete_organization erases the organization after the typed confirmation: its namespace and every RoleBinding referring to it gone", run: p.erase},
	}
}

// setUp keeps the part's organization Ready from scratch, its admins the
// platform admins, and signs both people in.
func (p *credentialsProof) setUp() error {
	if err := checkProofOrganization(credentialsOrganization); err != nil {
		return err
	}
	if err := p.removeLeftover(); err != nil {
		return err
	}
	step("Organization %s/%s Ready; group %s bound to %s there", organizationsRootNamespace, credentialsOrganization, platformAdminsGroup, organizationAdminRole)
	ns, err := ensureOrganization(organizationsRootNamespace, credentialsOrganization, "Credentials proof")
	if err != nil {
		return err
	}
	p.namespace = ns
	if err := markProofOrganization(credentialsOrganization); err != nil {
		return err
	}
	if err := bindOrganizationRole(ns, organizationAdminsBinding, organizationAdminRole, []rbacv1.Subject{groupSubject(platformAdminsGroup)}); err != nil {
		return err
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	org, err := getObject(ctx, gvr, organizationsRootNamespace, credentialsOrganization)
	if err != nil {
		return err
	}
	p.uid = string(org.GetUID())
	for _, u := range []struct {
		user  *config.User
		token *string
	}{{p.admin, &p.adminToken}, {p.other, &p.otherToken}} {
		step("Logging in to Dex as %s", u.user.Email)
		if *u.token, err = passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.user.Email, u.user.Password, musterLoginScopes); err != nil {
			return err
		}
	}
	note("%s runs in namespace %s", credentialsOrganization, ns)
	return nil
}

// removeLeftover erases a previous run's organization, which would hold the
// model the part adds.
func (p *credentialsProof) removeLeftover() error {
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return fmt.Errorf("the apiserver serves no %s: %w", organizationResource, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), organizationCleanupWait+kubeReadTimeout)
	defer cancel()
	org, err := getObject(ctx, gvr, organizationsRootNamespace, credentialsOrganization)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := markProofOrganization(credentialsOrganization); err != nil {
		return err
	}
	ns, _, _ := unstructured.NestedString(org.Object, "status", "namespace")
	step("Erasing a previous run's organization %s/%s", organizationsRootNamespace, credentialsOrganization)
	op := &organizationsProof{cfg: p.cfg, admin: p.admin, names: [2]string{credentialsOrganization}, namespaces: [2]string{ns}}
	return op.deleteProofOrganization(ctx, 0)
}

// readPrerequisites reads what the part stands on from the live lab.
func (p *credentialsProof) readPrerequisites() error {
	step("The part's prerequisites from the live lab: agent-manager's and agent-platform-operator's flags, the person's GitHub grant, what muster speaks, Flux's controllers")
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return err
	}
	for _, d := range []struct {
		name  string
		flags *map[string]string
	}{{agentManagerMCPServer, &p.pre.agentManager}, {"agent-platform-operator", &p.pre.operator}} {
		deployment, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, d.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading Deployment %s/%s: %w", platformNamespace, d.name, err)
		}
		*d.flags = containerFlags(deployment.Spec.Template.Spec.Containers[0].Args)
	}
	session, err := openMusterSession(p.cfg, p.adminToken, "organizations-credentials")
	if err != nil {
		return err
	}
	info, err := readAgentManagerInfo(session)
	if err != nil {
		return err
	}
	p.agentManagerVersion = info.Version
	p.pre.grant = p.grantProblem()
	modern, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-probe", protocol: mcp.ProtocolVersion20260728})
	if err != nil {
		return err
	}
	p.pre.musterModern = modern.protocol() == mcp.ProtocolVersion20260728
	modern.close()
	if p.pre.kustomizeController, err = kustomizeControllerReady(ctx); err != nil {
		return err
	}
	p.mode = modeApply
	if p.pre.commitSkip() == "" {
		p.mode = modeCommit
	}
	note("agent-manager %s, credential UI %q, mode %s; muster speaks %s to a %s client; kustomize-controller Ready %v; operator commit mode %v",
		p.agentManagerVersion, p.pre.agentManager[flagCredentialUIURL], p.mode, modern.protocol(), mcp.ProtocolVersion20260728,
		p.pre.kustomizeController, p.pre.operator[flagFluxSourceName] != "")
	return nil
}

// grantProblem is "" while the admin holds their grant of the workspaces'
// github instance App, which agent-manager's commit runs on, else why not.
func (p *credentialsProof) grantProblem() string {
	person, err := openWorkspacePerson(p.cfg, p.admin)
	if err != nil {
		return fmt.Sprintf("%s's grant of the %s instance App cannot be read (%v): commit mode runs on it", p.admin.Email, config.WorkspaceProviderGitHub, err)
	}
	status, err := person.providerStatus(config.WorkspaceProviderGitHub)
	if err != nil {
		return fmt.Sprintf("%s's grant of the %s instance App cannot be read (%v): commit mode runs on it", p.admin.Email, config.WorkspaceProviderGitHub, err)
	}
	if !status.Connected {
		return fmt.Sprintf("%s has not granted the %s instance App in muster: open muster's sign-in link for it once (core_auth_login, or `agentlab workspaces-test --part git`), then run again", p.admin.Email, config.WorkspaceProviderGitHub)
	}
	return ""
}

// modelArgs are set_organization_model's arguments: a new model, no key.
func (p *credentialsProof) modelArgs() map[string]any {
	return map[string]any{namespaceKey: p.namespace, nameKey: credentialsModel, "provider": organizationModelProvider, modelField: p.cfg.AIModel, modeArg: p.mode}
}

// modelWithoutKey: the plain client's set_organization_model writes
// nothing and names a pending operation.
func (p *credentialsProof) modelWithoutKey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	plain, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-plain", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	p.plain = plain
	answer, result, err := p.setModel(ctx, plain)
	if err != nil {
		return "", err
	}
	if answer.result == nil {
		return "", fmt.Errorf("a client without URL elicitation was answered with %d elicitations", len(answer.urlElicitations()))
	}
	if p.operation, p.formURL, err = pendingCredential(*result, credentialsModelSecret, credentialsModelKey); err != nil {
		return "", err
	}
	mc, err := modelConfigGVR()
	if err != nil {
		return "", err
	}
	if exists, err := objectExists(ctx, mc, p.namespace, credentialsModel); err != nil || exists {
		return "", fmt.Errorf("ModelConfig %s/%s exists before its credential was saved (err %v)", p.namespace, credentialsModel, err)
	}
	values, err := tenantCredentialValues(ctx, p.namespace)
	if err != nil {
		return "", err
	}
	if len(values) > 0 {
		return "", fmt.Errorf("tenant credential Secrets hold values before the save: %v", values)
	}
	return fmt.Sprintf("%s's set_organization_model %s/%s (%s, mode %s) without a key: no ModelConfig, no tenant-credential-* value, operation %s pending for %s/%s",
		p.admin.Email, p.namespace, credentialsModel, p.cfg.AIModel, p.mode, p.operation, credentialsModelSecret, credentialsModelKey), nil
}

// setModel calls set_organization_model on the client and decodes a result.
func (p *credentialsProof) setModel(ctx context.Context, m *musterMCP) (*musterAnswer, *credentialAnswer, error) {
	answer, err := m.call(ctx, agentManagerToolPrefix+"set_organization_model", p.modelArgs(), mcp.MultiRoundTripParams{})
	if err != nil || answer.result == nil {
		return answer, nil, err
	}
	if answer.result.IsError {
		return nil, nil, fmt.Errorf("set_organization_model failed: %s", excerpt(answer.result.text(), 300))
	}
	var result credentialAnswer
	if err := json.Unmarshal([]byte(answer.result.text()), &result); err != nil {
		return nil, nil, fmt.Errorf("set_organization_model's answer: %w: %s", err, excerpt(answer.result.text(), 300))
	}
	return answer, &result, nil
}

// tenantCredentialValues names the tenant credential Secrets of ns that hold
// a value; the values stay unread past their length.
func tenantCredentialValues(ctx context.Context, ns string) ([]string, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	list, err := k.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range list.Items {
		if !strings.HasPrefix(s.Name, "tenant-credential-") {
			continue
		}
		for key, value := range s.Data {
			if len(value) > 0 {
				out = append(out, s.Name+"/"+key)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// modernGeneration: a 2026-07-28 client declaring URL elicitation gets
// input_required, the message prefixed with agent-manager's key.
func (p *credentialsProof) modernGeneration() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-modern", protocol: mcp.ProtocolVersion20260728, urlElicitation: true})
	if err != nil {
		return "", err
	}
	defer m.close()
	answer, _, err := p.setModel(ctx, m)
	if err != nil {
		return "", err
	}
	if answer.input == nil {
		return "", fmt.Errorf("an MCP %s client declaring URL elicitation got no input_required result", m.protocol())
	}
	e, err := oneURLElicitation(answer)
	if err != nil {
		return "", err
	}
	if !attributedTo(e.Message, agentManagerMCPServer) {
		return "", fmt.Errorf("the elicitation's message %q is not prefixed with [%s]", e.Message, agentManagerMCPServer)
	}
	if err := p.formLink(e.URL); err != nil {
		return "", err
	}
	return fmt.Sprintf("input_required on %s with requestState, one URL elicitation %q", m.protocol(), excerpt(e.Message, 160)), nil
}

// legacyGeneration: a 2025-11-25 client that declared URL elicitation at
// initialize gets -32042 carrying the form's URL.
func (p *credentialsProof) legacyGeneration() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-legacy", protocol: mcp.ProtocolVersion20251125, urlElicitation: true})
	if err != nil {
		return "", err
	}
	defer m.close()
	if m.protocol() != mcp.ProtocolVersion20251125 {
		return "", fmt.Errorf("the client pinned to %s runs on %s", mcp.ProtocolVersion20251125, m.protocol())
	}
	answer, _, err := p.setModel(ctx, m)
	if err != nil {
		return "", err
	}
	if len(answer.urlRequired) == 0 {
		return "", errors.New("the call was not answered with -32042 (URLElicitationRequired)")
	}
	e, err := oneURLElicitation(answer)
	if err != nil {
		return "", err
	}
	if err := p.formLink(e.URL); err != nil {
		return "", err
	}
	return fmt.Sprintf("JSON-RPC error %d with one URL elicitation to %s", mcp.URL_ELICITATION_REQUIRED, uiURLPrefix(p.pre.agentManager[flagCredentialUIURL])+"…"), nil
}

// oneURLElicitation is the answer's one URL elicitation.
func oneURLElicitation(a *musterAnswer) (mcp.ElicitationParams, error) {
	es := a.urlElicitations()
	if len(es) != 1 {
		return mcp.ElicitationParams{}, fmt.Errorf("the answer carries %d URL elicitations, want one", len(es))
	}
	return es[0], nil
}

// formLink checks a link is the credential UI's.
func (p *credentialsProof) formLink(link string) error {
	prefix := uiURLPrefix(p.pre.agentManager[flagCredentialUIURL])
	if prefix == "" || !strings.HasPrefix(link, prefix) {
		return fmt.Errorf("the link %q is not the credential UI's (%s…)", link, prefix)
	}
	return nil
}

// plainGeneration: the plain client's answer carries the link in
// credentials[].next.
func (p *credentialsProof) plainGeneration() (string, error) {
	if p.formURL == "" {
		return "", errors.New("no pending operation from the plain client's call")
	}
	if err := p.formLink(p.formURL); err != nil {
		return "", err
	}
	return fmt.Sprintf("credentials[].next carries %s (operation %s)", p.formURL, p.operation), nil
}

// resourceLinkCheck is the structuredContent and resource_link of the plain
// client's answer, skipped on an agent-manager that predates them.
func (p *credentialsProof) resourceLinkCheck(uiSkip string) organizationCheck {
	c := organizationCheck{surface: surfaceAgentManager, boundary: "a client without URL elicitation gets structuredContent and a resource_link to the form", skip: uiSkip}
	if c.skip == "" {
		ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
		declared, err := p.plain.declaresOutputSchema(ctx, agentManagerToolPrefix+"set_organization_model")
		cancel()
		switch {
		case err != nil:
			c.skip = "describe_tool set_organization_model: " + err.Error()
		case !declared:
			c.skip = fmt.Sprintf("agent-manager %s declares no outputSchema on set_organization_model: it predates structuredContent and the resource_link on credential writes (giantswarm/agent-manager#329)", p.agentManagerVersion)
		}
	}
	c.run = func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
		defer cancel()
		answer, _, err := p.setModel(ctx, p.plain)
		if err != nil {
			return "", err
		}
		if answer.result == nil || len(answer.result.StructuredContent) == 0 {
			return "", errors.New("the answer carries no structuredContent")
		}
		var structured credentialAnswer
		if err := json.Unmarshal(answer.result.StructuredContent, &structured); err != nil {
			return "", fmt.Errorf("structuredContent: %w", err)
		}
		if _, link, err := pendingCredential(structured, credentialsModelSecret, credentialsModelKey); err != nil || link != p.formURL {
			return "", fmt.Errorf("structuredContent's credential link %q, want %s (%v)", link, p.formURL, err)
		}
		if !slices.Contains(answer.result.resourceLinks(), p.formURL) {
			return "", fmt.Errorf("the answer's resource_link blocks %v do not carry %s", answer.result.resourceLinks(), p.formURL)
		}
		return "structuredContent names the pending credential; a resource_link block carries " + p.formURL, nil
	}
	return c
}

// credentialREST is agent-manager's REST API on a port-forward to its pod:
// the in-cluster Service a portal backend calls with the person's token.
type credentialREST struct {
	base string
	stop func()
	http *http.Client
}

// dialCredentialREST forwards a local port to an agent-manager pod.
func dialCredentialREST(ctx context.Context) (*credentialREST, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	pods, err := k.targetPods(ctx, platformNamespace, "deploy/"+agentManagerMCPServer)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(pods, func(pod corev1.Pod) bool { return podReady(&pod) })
	if i < 0 {
		return nil, fmt.Errorf("the Deployment %s/%s has no Ready pod", platformNamespace, agentManagerMCPServer)
	}
	port, stop, err := portForwardPod(ctx, platformNamespace, pods[i].Name, credentialsRESTPort)
	if err != nil {
		return nil, err
	}
	return &credentialREST{base: fmt.Sprintf("http://127.0.0.1:%d", port), stop: stop, http: &http.Client{Timeout: musterMCPTimeout}}, nil
}

// do sends one request with the person's token and reads the answer; the
// request body is never part of an error.
func (r *credentialREST) do(ctx context.Context, method, operation, token string, header http.Header, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.base+credentialOperationsPath+operation, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, credentialOperationsPath+operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, raw, err
}

// otherPersonRefused: another lab user's read of the admin's operation is
// refused, as the phishing rule of agent-manager's credential operations
// requires (the operation is bound to its preparer's issuer and subject).
func (p *credentialsProof) otherPersonRefused() (string, error) {
	if p.operation == "" {
		return "", errors.New("no pending operation to read")
	}
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	resp, raw, err := rest.do(ctx, http.MethodGet, p.operation, p.otherToken, nil, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		return "", fmt.Errorf("%s's GET of %s's operation answered %d, want 404 or 403: %s", p.other.Email, p.admin.Email, resp.StatusCode, excerpt(string(raw), 200))
	}
	if bytes.Contains(raw, []byte(p.namespace)) || resp.Header.Get("ETag") != "" {
		return "", fmt.Errorf("%s's refused GET still carries the operation's organization or an ETag", p.other.Email)
	}
	// The twin: the preparer reads it.
	own, _, err := rest.do(ctx, http.MethodGet, p.operation, p.adminToken, nil, nil)
	if err != nil {
		return "", err
	}
	if own.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the twin: %s's own GET answered %d", p.admin.Email, own.StatusCode)
	}
	return fmt.Sprintf("%s's GET answered %d; %s's own GET 200", p.other.Email, resp.StatusCode, p.admin.Email), nil
}

// confirmation is the part of the credential API's confirmation the proof
// reads.
type confirmation struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Mode         string `json:"mode"`
	Organization struct {
		Namespace string `json:"namespace"`
	} `json:"organization"`
	GitTarget *struct {
		Repository string `json:"repository"`
		Base       string `json:"base"`
	} `json:"gitTarget"`
}

// save drives the credential API as the portal's form: the confirmation,
// then the value with If-Match; the original call retried returns the
// receipt.
func (p *credentialsProof) save() (string, error) {
	if p.operation == "" {
		return "", errors.New("no pending operation to save")
	}
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	resp, raw, err := rest.do(ctx, http.MethodGet, p.operation, p.adminToken, nil, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s answered %d: %s", p.operation, resp.StatusCode, excerpt(string(raw), 300))
	}
	etag, err := strongETag(resp.Header.Get("ETag"))
	if err != nil {
		return "", err
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		return "", fmt.Errorf("GET answered Cache-Control %q, want no-store", cc)
	}
	var c confirmation
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("the confirmation: %w", err)
	}
	switch {
	case c.ID != p.operation || c.State != "pending" || c.Mode != p.mode || c.Organization.Namespace != p.namespace:
		return "", fmt.Errorf("the confirmation names operation %q state %q mode %q in %q, want %s pending %s in %s", c.ID, c.State, c.Mode, c.Organization.Namespace, p.operation, p.mode, p.namespace)
	case p.mode == modeCommit && (c.GitTarget == nil || c.GitTarget.Repository != p.pre.agentManager[flagOrgsRepository]):
		return "", fmt.Errorf("the confirmation's git target %+v is not %s", c.GitTarget, p.pre.agentManager[flagOrgsRepository])
	}
	body, err := credentialSaveBody(credentialsModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	header := http.Header{"Content-Type": {"application/json"}, "If-Match": {etag}}
	resp, raw, err = rest.do(ctx, http.MethodPost, p.operation, p.adminToken, header, body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s answered %d: %s", p.operation, resp.StatusCode, excerpt(string(raw), 300))
	}
	var receipt credentialAnswer
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return "", fmt.Errorf("the receipt: %w", err)
	}
	p.receipt = &receipt
	want := credentialStatePresent
	if p.mode == modeCommit {
		want = credentialStateCommitted
		if receipt.Commit == nil || receipt.Commit.PullRequest == "" {
			return "", errors.New("the receipt in mode commit names no pull request")
		}
	}
	if receipt.Pending || len(receipt.Credentials) == 0 || receipt.Credentials[0].State != want {
		return "", fmt.Errorf("the receipt is pending=%v with credentials %+v, want %s", receipt.Pending, receipt.Credentials, want)
	}
	retried, result, err := p.setModel(ctx, p.plain)
	if err != nil {
		return "", err
	}
	if retried.result == nil || result.Pending || result.CredentialOperation != "" && result.CredentialOperation != p.operation {
		return "", errors.New("the original call retried does not return the receipt")
	}
	detail := fmt.Sprintf("ETag %s, Cache-Control no-store; POST with If-Match saved %s/%s (%s) as %s; the original call retried returns the receipt", etag, credentialsModelSecret, credentialsModelKey, shortSum(p.valueSum), want)
	if p.mode == modeCommit {
		return detail + "; pull request " + receipt.Commit.PullRequest, nil
	}
	sum, found, err := secretValueSum(ctx, p.namespace, credentialsModelSecret, credentialsModelKey)
	if err != nil {
		return "", err
	}
	if !found || sum != p.valueSum {
		return "", fmt.Errorf("mode apply: Secret %s/%s holds %s, want %s", p.namespace, credentialsModelSecret, shortSum(sum), shortSum(p.valueSum))
	}
	return detail + "; mode apply: the live Secret's SHA-256 matches", nil
}

// pullRequest reads the receipt's pull request back on GitHub with the
// person's own gh login: author, commit author and the Secret's diff.
func (p *credentialsProof) pullRequest() (string, error) {
	if p.receipt == nil || p.receipt.Commit == nil {
		return "", errors.New("no receipt with a pull request")
	}
	c := p.receipt.Commit
	if c.Repository != p.pre.agentManager[flagOrgsRepository] {
		return "", fmt.Errorf("the pull request is on %s, want agent-manager's organizations repository %s", c.Repository, p.pre.agentManager[flagOrgsRepository])
	}
	login, err := personGH("api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	out, err := personGH("api", fmt.Sprintf("repos/%s/pulls/%d", c.Repository, c.Number), "--jq", `[.user.login, .head.sha, .state] | @tsv`)
	if err != nil {
		return "", err
	}
	fields := strings.Split(out, "\t")
	if len(fields) != 3 || fields[2] != "open" {
		return "", fmt.Errorf("pull request %s answered %q, want an open pull request", c.PullRequest, out)
	}
	author, head := fields[0], fields[1]
	commit, err := personGH("api", fmt.Sprintf("repos/%s/commits/%s", c.Repository, head), "--jq", `[(.author.login // ""), (.committer.login // "")] | @tsv`)
	if err != nil {
		return "", err
	}
	commitAuthor, committer, _ := strings.Cut(commit, "\t")
	if author != login || commitAuthor != login || c.Author != login {
		return "", fmt.Errorf("pull request %s is opened by %q, its commit authored by %q, the receipt says %q; the person is %q", c.PullRequest, author, commitAuthor, c.Author, login)
	}
	files, err := personGH("api", "--paginate", fmt.Sprintf("repos/%s/pulls/%d/files", c.Repository, c.Number), "--jq", `.[] | {filename, patch}`)
	if err != nil {
		return "", err
	}
	secretPath := credentialSecretPath(p.pre.orgsContentPrefix(), organizationsRootNamespace, credentialsOrganization, credentialsModelSecret)
	seen := false
	dec := json.NewDecoder(strings.NewReader(files))
	for {
		var f struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		if err := dec.Decode(&f); err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("pull request files: %w", err)
		}
		if f.Filename == secretPath {
			seen = true
			if err := sopsEncryptedPatch(f.Patch, p.value); err != nil {
				return "", fmt.Errorf("%s: %w", f.Filename, err)
			}
		} else if err := valueInPatch(f.Patch, p.value); err != nil {
			return "", fmt.Errorf("%s: %w", f.Filename, err)
		}
	}
	if !seen {
		return "", fmt.Errorf("pull request %s carries no %s", c.PullRequest, secretPath)
	}
	return fmt.Sprintf("%s on %s opened by %s, its commit authored by %s (committer %s); %s SOPS-encrypted, no file carries the value", c.PullRequest, c.Repository, author, commitAuthor, committer, secretPath), nil
}

// fluxApplied merges the pull request as the person and waits for the
// organization's Kustomization to apply and decrypt it.
func (p *credentialsProof) fluxApplied() (string, error) {
	if p.receipt == nil || p.receipt.Commit == nil {
		return "", errors.New("no pull request to merge")
	}
	c := p.receipt.Commit
	fluxNS, decryption := p.pre.operator[flagFluxNamespace], p.pre.operator[flagFluxDecryption]
	identity, keyPath, err := ensureCredentialsAgeIdentity()
	if err != nil {
		return "", err
	}
	recipient := identity.Recipient().String()
	sopsFile, err := readSOPSConfig(c.Repository, c.Base)
	if err != nil {
		return "", err
	}
	secretPath := credentialSecretPath(p.pre.orgsContentPrefix(), organizationsRootNamespace, credentialsOrganization, credentialsModelSecret)
	if err := sopsRuleFor(sopsFile, secretPath, recipient); err != nil {
		return "", fmt.Errorf("%w — commit to %s@%s's .sops.yaml:\n%s", err, c.Repository, c.Base, sopsRuleHint(p.pre.orgsContentPrefix(), recipient))
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialsFluxWait+2*kubeReadTimeout)
	defer cancel()
	if err := placeCredentialsAgeSecret(ctx, fluxNS, decryption, keyPath); err != nil {
		return "", err
	}
	kustomization, err := getObject(ctx, kustomizationGVR, fluxNS, p.namespace)
	if err != nil {
		return "", fmt.Errorf("the organization's Kustomization %s/%s: %w", fluxNS, p.namespace, err)
	}
	provider, _, _ := unstructured.NestedString(kustomization.Object, "spec", "decryption", "provider")
	secretRef, _, _ := unstructured.NestedString(kustomization.Object, "spec", "decryption", "secretRef", "name")
	if provider != "sops" || secretRef != decryption {
		return "", fmt.Errorf("the Kustomization %s/%s decrypts with %q from %q, want sops from %s", fluxNS, p.namespace, provider, secretRef, decryption)
	}
	step("%s merges %s as themselves", p.admin.Email, c.PullRequest)
	if _, err := personGH("pr", "merge", strconv.Itoa(c.Number), "--repo", c.Repository, "--squash"); err != nil {
		return "", err
	}
	p.merged = true
	merge, err := personGH("pr", "view", strconv.Itoa(c.Number), "--repo", c.Repository, "--json", "mergeCommit", "--jq", ".mergeCommit.oid")
	if err != nil || merge == "" {
		return "", fmt.Errorf("the merge commit of %s: %q %v", c.PullRequest, merge, err)
	}
	source, err := fluxSourceGVR(firstNonEmpty(p.pre.operator[flagFluxSourceKind], defaultFluxSourceKind))
	if err != nil {
		return "", err
	}
	if err := requestFluxReconcile(ctx, source, fluxNS, p.pre.operator[flagFluxSourceName]); err != nil {
		return "", err
	}
	if err := requestFluxReconcile(ctx, kustomizationGVR, fluxNS, p.namespace); err != nil {
		return "", err
	}
	applied, err := waitKustomizationApplied(ctx, fluxNS, p.namespace, merge)
	if err != nil {
		return "", err
	}
	sum, found, err := secretValueSum(ctx, p.namespace, credentialsModelSecret, credentialsModelKey)
	if err != nil {
		return "", err
	}
	if !found || sum != p.valueSum {
		return "", fmt.Errorf("the Secret %s/%s holds %s after the apply, want %s", p.namespace, credentialsModelSecret, shortSum(sum), shortSum(p.valueSum))
	}
	mc, err := modelConfigGVR()
	if err != nil {
		return "", err
	}
	if exists, err := objectExists(ctx, mc, p.namespace, credentialsModel); err != nil || !exists {
		return "", fmt.Errorf("ModelConfig %s/%s missing after the apply (err %v)", p.namespace, credentialsModel, err)
	}
	return fmt.Sprintf("merged %s (%s); Kustomization %s/%s %s, decryption sops from %s (the lab's age recipient %s); Secret %s/%s %s, ModelConfig %s present",
		c.PullRequest, merge[:min(12, len(merge))], fluxNS, p.namespace, applied, decryption, recipient, p.namespace, credentialsModelSecret, shortSum(sum), credentialsModel), nil
}

// tenantSignInCheck is the tenant namespace's MCP server sign-in, skipped
// while muster serves no organization namespace's MCPServers.
func (p *credentialsProof) tenantSignInCheck() organizationCheck {
	c := organizationCheck{surface: surfaceMuster, boundary: "a sign-in an organization namespace's MCP server needs is elicited attributed to its <namespace>/<name> key"}
	tenancy, err := readMusterTenancy()
	if err != nil {
		c.run = func() (string, error) { return "", err }
		return c
	}
	nsLabels, err := namespaceLabels([]string{p.namespace})
	if err != nil {
		c.run = func() (string, error) { return "", err }
		return c
	}
	if problem := tenancy.problem(nsLabels, organizationMCPServerLabels); problem != "" {
		c.skip = problem
		return c
	}
	c.run = p.tenantSignIn
	return c
}

// tenantSignIn applies an OAuth MCPServer in the organization namespace and
// has the admin ask for its sign-in on a client declaring URL elicitation.
func (p *credentialsProof) tenantSignIn() (string, error) {
	if err := applyOrganizationManifest(credentialsSignInManifest(p.namespace)); err != nil {
		return "", err
	}
	defer removeCredentialsSignInServer(p.namespace)
	key := tenantServerKey(p.namespace, credentialsSignInServer)
	state, err := waitTenantMCPServerState(p.namespace, credentialsSignInServer, mcpServerStateAuthRequired)
	if err != nil {
		return "", err
	}
	protocol := mcp.ProtocolVersion20251125
	if p.pre.musterModern {
		protocol = mcp.ProtocolVersion20260728
	}
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-signin", protocol: protocol, urlElicitation: true})
	if err != nil {
		return "", err
	}
	defer m.close()
	answer, err := m.call(ctx, "core_auth_login", map[string]any{serverKey: key}, mcp.MultiRoundTripParams{})
	if err != nil {
		return "", err
	}
	e, err := oneURLElicitation(answer)
	if err != nil {
		return "", err
	}
	how := signInAttribution(e.Message, key)
	if how == "" {
		return "", fmt.Errorf("the sign-in elicitation's message %q does not name %s", e.Message, key)
	}
	if !strings.Contains(e.URL, oauthProxyStartPath) {
		return "", fmt.Errorf("the sign-in link %q is not muster's %s", e.URL, oauthProxyStartPath)
	}
	return fmt.Sprintf("MCPServer %s %s; core_auth_login on %s elicits %q (%s) to muster's %s", key, state, m.protocol(), excerpt(e.Message, 160), how, oauthProxyStartPath), nil
}

// credentialsSignInManifest is the tenant namespace's MCPServer that needs a
// per-server sign-in: muster's own protected /mcp, as lab-oauth-fixture,
// with the authorization server muster discovers there (an organization
// namespace's server cannot name a client Secret muster may read).
func credentialsSignInManifest(ns string) string {
	return fmt.Sprintf(`apiVersion: muster.giantswarm.io/v1alpha1
kind: MCPServer
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  type: streamable-http
  url: %s
  autoStart: true
  auth:
    type: oauth
    forwardToken: false
`, credentialsSignInServer, ns, organizationsTestLabel, organizationsTestMark, oauthFixtureURL)
}

// removeCredentialsSignInServer deletes the tenant MCPServer, best effort.
func removeCredentialsSignInServer(ns string) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*kubeReadTimeout)
	defer cancel()
	if err := deleteObject(ctx, gvr, ns, credentialsSignInServer, kubeReadTimeout); err != nil && !apierrors.IsNotFound(err) {
		note("cleanup: %v", err)
	}
}

// waitTenantMCPServerState waits for an MCPServer of ns to read state.
func waitTenantMCPServerState(ns, name, want string) (string, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return "", err
	}
	var state string
	ok := waitFor(int(organizationsMusterSettle/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		obj, err := getObject(ctx, gvr, ns, name)
		if err != nil {
			state = err.Error()
			return false
		}
		state, _, _ = unstructured.NestedString(obj.Object, "status", "state")
		return state == want
	})
	if !ok {
		return "", fmt.Errorf("MCPServer %s/%s reads %q after %s, want %s", ns, name, state, organizationsMusterSettle, want)
	}
	return state, nil
}

// deleteResult is the part of delete_organization's answer the proof reads.
type deleteResult struct {
	Deleted bool `json:"deleted"`
	DryRun  bool `json:"dryRun"`
	Erases  *struct {
		Namespace   string   `json:"namespace"`
		Models      []string `json:"models"`
		Credentials []string `json:"credentials"`
		Members     []string `json:"members"`
	} `json:"erases"`
}

// erase deletes the organization through delete_organization as the
// admin: what it erases, the confirmed dry run, the delete; then the
// namespace and every RoleBinding referring to the organization are gone.
func (p *credentialsProof) erase() (string, error) {
	session, err := openMusterSession(p.cfg, p.adminToken, "organizations-credentials-erase")
	if err != nil {
		return "", err
	}
	confirm := organizationsRootNamespace + "/" + credentialsOrganization
	base := map[string]any{namespaceKey: organizationsRootNamespace, nameKey: credentialsOrganization}
	var preview, dry, done deleteResult
	for _, c := range []struct {
		args map[string]any
		into *deleteResult
	}{
		{base, &preview},
		{withArgs(base, map[string]any{"confirm": confirm, dryRunArg: true}), &dry},
		{withArgs(base, map[string]any{"confirm": confirm}), &done},
	} {
		if err := session.callServerJSON(agentManagerToolPrefix+"delete_organization", c.args, c.into); err != nil {
			return "", err
		}
	}
	switch {
	case preview.Deleted || preview.Erases == nil || preview.Erases.Namespace != p.namespace:
		return "", fmt.Errorf("delete_organization without confirm answered deleted=%v erases=%+v, want what would be erased in %s", preview.Deleted, preview.Erases, p.namespace)
	case dry.Deleted || !dry.DryRun:
		return "", fmt.Errorf("the confirmed dry run answered deleted=%v dryRun=%v", dry.Deleted, dry.DryRun)
	case !done.Deleted:
		return "", errors.New("delete_organization with the typed confirmation answered deleted=false")
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	var last string
	gone := waitFor(int(organizationCleanupWait/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		_, orgErr := getObject(ctx, gvr, organizationsRootNamespace, credentialsOrganization)
		_, nsErr := getObject(ctx, gvrNamespaces, "", p.namespace)
		last = fmt.Sprintf("organization: %v; namespace: %v", orgErr, nsErr)
		return apierrors.IsNotFound(orgErr) && apierrors.IsNotFound(nsErr)
	})
	if !gone {
		return "", fmt.Errorf("organization %s and namespace %s not gone within %s (%s)", confirm, p.namespace, organizationCleanupWait, last)
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	bindings, err := k.clientset.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	ref := organizationRef{parent: organizationsRootNamespace, name: credentialsOrganization, namespace: p.namespace, uid: p.uid}
	if left := organizationBindingLeftovers(bindings.Items, ref); len(left) > 0 {
		return "", fmt.Errorf("RoleBindings referring to the erased organization remain: %s", strings.Join(left, ", "))
	}
	p.namespace = ""
	return fmt.Sprintf("without confirm: erases %d models, %d credentials, %d members in %s; the confirmed dry run deleted nothing; the delete erased it, namespace gone, no RoleBinding of %d refers to it",
		len(preview.Erases.Models), len(preview.Erases.Credentials), len(preview.Erases.Members), preview.Erases.Namespace, len(bindings.Items)), nil
}

// withArgs is base with more arguments.
func withArgs(base, more map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(more))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// cleanup closes the client and, after a merge, removes the organization's
// content path from the organizations repository's base, so the next run's
// create finds no file. A failed run keeps the organization for diagnosis.
func (p *credentialsProof) cleanup() {
	if p.plain != nil {
		p.plain.close()
	}
	if p.namespace != "" {
		removeCredentialsSignInServer(p.namespace)
	}
	if p.retainForDiagnostics && p.namespace != "" {
		note("retaining organization %s/%s (namespace %s) for diagnosis", organizationsRootNamespace, credentialsOrganization, p.namespace)
	}
	if p.merged {
		if err := p.removeContentPath(); err != nil {
			note("cleanup: %v", err)
		}
	}
}

// removeContentPath commits the removal of the organization's content path
// to the organizations repository's base with the person's git credentials.
func (p *credentialsProof) removeContentPath() error {
	repo, base := p.pre.agentManager[flagOrgsRepository], p.pre.orgsBranch()
	path := strings.Join([]string{p.pre.orgsContentPrefix(), organizationsRootNamespace, credentialsOrganization}, "/")
	dir, err := os.MkdirTemp(StateDir, "credentials-cleanup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	clone := filepath.Join(dir, "repo")
	steps := [][]string{
		{"clone", "-q", "--depth", "1", "--branch", base, measureGitURL + "/" + repo + ".git", clone},
		{"-C", clone, "rm", "-r", "-q", "--ignore-unmatch", path},
		{"-C", clone, "commit", "-q", "-m", "chore: remove the agentlab credentials proof's organization content"},
		{"-C", clone, gitPush, "-q", "origin", "HEAD:" + base},
	}
	for _, args := range steps {
		if _, err := personGit(dir, args...); err != nil {
			return fmt.Errorf("removing %s from %s@%s (remove it by hand before the next run): %w", path, repo, base, err)
		}
	}
	note("cleanup: removed %s from %s@%s", path, repo, base)
	return nil
}
