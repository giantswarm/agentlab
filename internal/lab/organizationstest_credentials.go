package lab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
// Every prerequisite is read from the live lab and never configured here,
// except the SOPS age Secret the organizations' Kustomizations decrypt with:
// the lab places its own where none exists or agentlab wrote it. A missing
// prerequisite is a SKIP that names its fix. The key typed is a random value
// generated in this process, printed only as its SHA-256, and redacted with
// its base64 from every answer an error quotes.

const (
	organizationCredentialsPart = "credentials"
	// credentialsOrganization is the part's own organization, erased at the
	// end; credentialsModel the model it adds without a key, whose key
	// agent-manager names by default.
	credentialsOrganization = "agentlab-credentials"                  // #nosec G101 -- an organization name, not a credential
	credentialsModel        = "agentlab-credentials"                  // #nosec G101 -- a ModelConfig name, not a credential
	credentialsModelSecret  = "tenant-credential-" + credentialsModel // #nosec G101 -- a Secret's NAME, not a credential
	credentialsModelKey     = "apiKey"
	// credentialsOtherModel is the model the other person adds without a
	// key: their own operation, which they read and then cancel.
	credentialsOtherModel       = "agentlab-credentials-other"                 // #nosec G101 -- a ModelConfig name, not a credential
	credentialsOtherModelSecret = "tenant-credential-" + credentialsOtherModel // #nosec G101 -- a Secret's NAME, not a credential
	// credentialsDeclineModel and credentialsDismissModel are the models
	// whose URL elicitation the person declines and dismisses.
	credentialsDeclineModel = "agentlab-credentials-decline" // #nosec G101 -- a ModelConfig name, not a credential
	credentialsDismissModel = "agentlab-credentials-dismiss" // #nosec G101 -- a ModelConfig name, not a credential
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
	// operationPending and operationCancelled are a credential operation's
	// states as its confirmation reads them.
	operationPending   = "pending"
	operationCancelled = "cancelled"
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
	// admin and other are both admins of the organization: other is a lab
	// user outside platform-admins, bound by name.
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
	// retry that returns the receipt is its call again. modern is the
	// 2026-07-28 generation, kept open with its input_required answer, which
	// it retries before and after the save. agent-manager binds an
	// operation to the person, the client and the MCP session it sees
	// (agent-manager docs/organization-credentials.md, step 4), so
	// modernOperation is the plain client's operation only while muster
	// reaches agent-manager on one session for both.
	plain           *musterMCP
	modern          *musterMCP
	modernAnswer    *musterAnswer
	modernOperation string
	// legacy is the 2025-11-25 generation declaring URL elicitation, kept
	// open on its listening stream with the elicitationId it was given.
	legacy              *musterMCP
	legacyElicitationID string
	operation, formURL  string
	otherOperation      string
	// otherBlocked is why the other person could not prepare an operation
	// of their own: the SKIP of the checks that need it.
	otherBlocked string
	// started is when the run began: the logs judged for the value are
	// read from it.
	started             time.Time
	receipt             *credentialAnswer
	agentManagerVersion string
	merged              bool
	// live says the model and its credential Secret exist on the cluster:
	// saved in mode apply, or applied by Flux after the merge.
	live                 bool
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
	p := &credentialsProof{cfg: cfg, admin: admin, other: other, value: value, valueSum: sha256.Sum256([]byte(value)), started: time.Now()}
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
		{surface: surfaceAgentManager, boundary: "an MCP 2026-07-28 client declaring URL elicitation gets input_required, its message attributed to agent-manager by muster, and stays input_required when it retries having accepted", skip: generations, run: p.modernGeneration},
		{surface: surfaceAgentManager, boundary: "an MCP 2026-07-28 client whose person declines or dismisses the URL elicitation gets an error result, nothing written and the operation cancelled", skip: generations, run: p.declinedAndDismissed},
		{surface: surfaceAgentManager, boundary: "an MCP 2025-11-25 client that declared URL elicitation gets -32042 carrying one URL mode elicitation with an elicitationId and the form's URL", skip: generations, run: p.legacyGeneration},
		{surface: surfaceAgentManager, boundary: "a client without URL elicitation gets a result whose credentials[].next carries the form's link", skip: uiSkip, run: p.plainGeneration},
		p.resourceLinkCheck(uiSkip),
		{surface: surfaceCredentialAPI, boundary: "an operation's confirmation is its preparer's alone: the admin reads their own, another admin's read is refused as an unknown operation's, an anonymous one 401", skip: uiSkip, run: p.otherPersonRefused},
		{surface: surfaceCredentialAPI, boundary: "an organization admin outside platform-admins prepares an operation of their own and reads it; the first admin's read of it is refused as an unknown operation's", skip: uiSkip, run: p.otherOwnOperation},
		{surface: surfaceAgentManager, boundary: "a person with read-only rights on the organization (a viewer) is refused set_organization_model with nothing written, and their POST of the admin's operation with its ETag is refused with nothing written", skip: firstNonEmpty(uiSkip, p.viewerSkip()), run: p.viewerRefused},
		{surface: surfaceCredentialAPI, boundary: "DELETE with If-Match cancels a pending operation: a stale ETag is refused, and after the cancel a save is refused with nothing written", skip: uiSkip, run: p.cancel},
		{surface: surfaceCredentialAPI, boundary: "the form's save: GET with a strong ETag and the consumer the confirmation lists, a POST without If-Match (428) or with a stale one refused with nothing written, POST with If-Match and only the requested pair returns the receipt, a second POST and each client generation's retry the same receipt", skip: uiSkip, run: p.save},
		{surface: surfaceCredentialAPI, boundary: "a save whose Secret another consumer came to reference is refused 409 with nothing written, and preparing it again is refused", skip: uiSkip, run: p.sharedSecretRefused},
		{surface: surfaceCredentialAPI, boundary: "the confirmations of set_organization_connector and set_organization_egress_policy list the one consumer each and, for the EgressPolicy, exactly the origins it sends the credential to; both cancelled with nothing written", skip: uiSkip, run: p.connectorAndEgressConfirmations},
		{surface: surfaceGitHub, boundary: "the save opens a pull request as the person, its Secret SOPS-encrypted", skip: commit, run: p.pullRequest},
		{surface: surfaceFlux, boundary: "the merged pull request is applied and decrypted by the organization's Kustomization: the Secret's SHA-256 matches and the ModelConfig exists", skip: flux, run: p.fluxApplied},
		{surface: surfaceCredentialAPI, boundary: "neither agent-manager's nor muster's logs carry the value, plain or base64, each read with at least one line", skip: uiSkip, run: p.valueNotLogged},
		{surface: surfaceAgentManager, boundary: "a notifications/elicitation/complete reaches only the 2025-11-25 client that initiated the elicitation, carrying its elicitationId", skip: generations, run: p.completionsScoped},
		p.tenantSignInCheck(),
		{surface: surfaceAgentManager, boundary: "delete_organization names the models and credentials it erases, then erases the organization after the typed confirmation: its namespace, its Flux Kustomization and ServiceAccount, and every RoleBinding referring to it gone", run: p.erase},
	}
}

// setUp keeps the part's organization Ready from scratch, its admins the
// platform admins and the other person, and signs both people in.
func (p *credentialsProof) setUp() error {
	if err := checkProofOrganization(credentialsOrganization); err != nil {
		return err
	}
	if err := p.removeLeftover(); err != nil {
		return err
	}
	step("Organization %s/%s Ready; group %s and %s bound to %s there", organizationsRootNamespace, credentialsOrganization, platformAdminsGroup, p.other.Email, organizationAdminRole)
	ns, err := ensureOrganization(organizationsRootNamespace, credentialsOrganization, "Credentials proof")
	if err != nil {
		return err
	}
	p.namespace = ns
	if err := markProofOrganization(credentialsOrganization); err != nil {
		return err
	}
	if err := bindOrganizationRole(ns, organizationAdminsBinding, organizationAdminRole, []rbacv1.Subject{groupSubject(platformAdminsGroup), userSubject(p.other)}); err != nil {
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
		args, err := containerArgs(deployment.Spec.Template.Spec.Containers, d.name)
		if err != nil {
			return fmt.Errorf("reading the flags of Deployment %s/%s: %w", platformNamespace, d.name, err)
		}
		*d.flags = containerFlags(args)
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
	described, err := session.describeTool(agentManagerToolPrefix + "set_organization_model")
	if err != nil {
		return err
	}
	p.pre.outputSchema = described.OutputSchema
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
	plain, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-plain", protocol: mcp.ProtocolVersion20251125, listen: true})
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
	if err := modelConfigPresent(ctx, p.namespace, credentialsModel, false); err != nil {
		return "", fmt.Errorf("before its credential was saved: %w", err)
	}
	values, err := tenantCredentialValues(ctx, p.namespace)
	if err != nil {
		return "", err
	}
	if len(values) > 0 {
		return "", fmt.Errorf("tenant credential Secrets hold values before the save: %v", values)
	}
	detail := fmt.Sprintf("%s's set_organization_model %s/%s (%s, mode %s) without a key: no ModelConfig, no tenant-credential-* value, operation %s pending for %s/%s",
		p.admin.Email, p.namespace, credentialsModel, p.cfg.AIModel, p.mode, p.operation, credentialsModelSecret, credentialsModelKey)
	if p.mode != modeCommit {
		return detail, nil
	}
	repo, path := p.pre.agentManager[flagOrgsRepository], credentialsContentPath(p.pre.orgsContentPrefix())
	open, err := openPullRequestsTouching(repo, path)
	if err != nil {
		return "", err
	}
	if len(open) > 0 {
		return "", fmt.Errorf("pull requests %v on %s touch %s before the save", open, repo, path)
	}
	return detail + "; no open pull request on " + repo + " touches " + path, nil
}

// openPullRequestsTouching are the person's open pull requests on repo
// that change a file under path.
func openPullRequestsTouching(repo, path string) ([]string, error) {
	out, err := personGH("pr", "list", "--repo", repo, "--state", "open", "--author", "@me", "--json", "number,files",
		"--jq", fmt.Sprintf(`.[] | select(any(.files[]; .path | startswith(%s))) | .number`, strconv.Quote(path+"/")))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// setModel calls set_organization_model with the part's model on the client
// and decodes a result.
func (p *credentialsProof) setModel(ctx context.Context, m *musterMCP) (*musterAnswer, *credentialAnswer, error) {
	return setModelWith(ctx, m, p.modelArgs(), mcp.MultiRoundTripParams{})
}

// setModelWith calls set_organization_model with args, answering a previous
// input_required with roundTrip, and decodes a result.
func setModelWith(ctx context.Context, m *musterMCP, args map[string]any, roundTrip mcp.MultiRoundTripParams) (*musterAnswer, *credentialAnswer, error) {
	return credentialToolWith(ctx, m, "set_organization_model", args, roundTrip)
}

// credentialToolWith calls one of agent-manager's model, connector and
// egress tools with args, answering a previous input_required with
// roundTrip, and decodes a result.
func credentialToolWith(ctx context.Context, m *musterMCP, tool string, args map[string]any, roundTrip mcp.MultiRoundTripParams) (*musterAnswer, *credentialAnswer, error) {
	answer, err := m.call(ctx, agentManagerToolPrefix+tool, args, roundTrip)
	if err != nil || answer.result == nil {
		return answer, nil, err
	}
	if answer.result.IsError {
		return nil, nil, fmt.Errorf("%s failed: %s", tool, excerpt(answer.result.text(), 300))
	}
	var result credentialAnswer
	if err := json.Unmarshal([]byte(answer.result.text()), &result); err != nil {
		return nil, nil, fmt.Errorf("%s's answer: %w: %s", tool, err, excerpt(answer.result.text(), 300))
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
	p.modern = m
	answer, operation, err := p.inputRequired(ctx, mcp.MultiRoundTripParams{})
	if err != nil {
		return "", err
	}
	e, _ := oneURLElicitation(answer)
	if !attributedTo(e.Message, agentManagerMCPServer) {
		return "", fmt.Errorf("the elicitation's message %q is not prefixed with [%s]", e.Message, agentManagerMCPServer)
	}
	retried, again, err := p.inputRequired(ctx, answer.accepted())
	if err != nil {
		return "", fmt.Errorf("the retry having accepted, before the save: %w", err)
	}
	if again != operation {
		return "", fmt.Errorf("the retry having accepted names operation %s, the first answer %s", again, operation)
	}
	p.modernAnswer, p.modernOperation = retried, operation
	binding := "the plain client's operation"
	if operation != p.operation {
		binding = "its own, not the plain client's " + p.operation
	}
	return fmt.Sprintf("input_required on %s with requestState, one URL elicitation %q for operation %s (%s); retried having accepted, input_required again for the same operation",
		m.protocol(), excerpt(e.Message, 160), operation, binding), nil
}

// inputRequired calls set_organization_model on the 2026-07-28 client with
// roundTrip and checks the answer is input_required with a requestState and
// one URL elicitation to the credential UI, whose operation it returns.
func (p *credentialsProof) inputRequired(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*musterAnswer, string, error) {
	answer, err := p.modern.call(ctx, agentManagerToolPrefix+"set_organization_model", p.modelArgs(), roundTrip)
	if err != nil {
		return nil, "", err
	}
	switch {
	case answer.input == nil:
		return nil, "", fmt.Errorf("an MCP %s client declaring URL elicitation got no input_required result", p.modern.protocol())
	case answer.requestState == "":
		return nil, "", fmt.Errorf("the input_required result on %s carries no requestState", p.modern.protocol())
	}
	e, err := oneURLElicitation(answer)
	if err != nil {
		return nil, "", err
	}
	operation, err := p.formLink(e.URL)
	return answer, operation, err
}

// legacyGeneration: a 2025-11-25 client that declared URL elicitation at
// initialize gets -32042 carrying the form's URL.
func (p *credentialsProof) legacyGeneration() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-legacy", protocol: mcp.ProtocolVersion20251125, urlElicitation: true, listen: true})
	if err != nil {
		return "", err
	}
	p.legacy = m
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
	if e.Mode != mcp.ElicitationModeURL || e.ElicitationID == "" {
		return "", fmt.Errorf("the -32042 elicitation has mode %q and elicitationId %q: MCP 2025-11-25 requires mode url and an elicitationId", e.Mode, e.ElicitationID)
	}
	p.legacyElicitationID = e.ElicitationID
	if _, err := p.formLink(e.URL); err != nil {
		return "", err
	}
	return fmt.Sprintf("JSON-RPC error %d with one URL mode elicitation %s to %s", mcp.URL_ELICITATION_REQUIRED, e.ElicitationID, e.URL), nil
}

// declinedAndDismissed: on the 2026-07-28 client, for a model of its own
// each, the person declines the URL elicitation and dismisses it. Each
// retry ends the call with an error result naming the action, writes
// nothing and cancels the operation (MCP 2026-07-28 client/elicitation,
// "Error Handling").
func (p *credentialsProof) declinedAndDismissed() (string, error) {
	if p.modern == nil {
		return "", errors.New("no 2026-07-28 client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*musterMCPTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	tool := agentManagerToolPrefix + "set_organization_model"
	outcomes := make([]string, 0, 2)
	for _, c := range []struct {
		action mcp.ElicitationResponseAction
		model  string
		word   string
	}{
		{mcp.ElicitationResponseActionDecline, credentialsDeclineModel, "declined"},
		{mcp.ElicitationResponseActionCancel, credentialsDismissModel, "cancelled"},
	} {
		args := withArgs(p.modelArgs(), map[string]any{nameKey: c.model, modeArg: modeApply})
		answer, err := p.modern.call(ctx, tool, args, mcp.MultiRoundTripParams{})
		if err != nil {
			return "", err
		}
		if answer.input == nil {
			return "", fmt.Errorf("model %s without a key got no input_required result", c.model)
		}
		e, err := oneURLElicitation(answer)
		if err != nil {
			return "", err
		}
		operation, err := p.formLink(e.URL)
		if err != nil {
			return "", err
		}
		final, err := p.modern.call(ctx, tool, args, answer.answered(c.action))
		if err != nil {
			return "", fmt.Errorf("the retry having %s: %w", c.word, err)
		}
		switch {
		case final.result == nil:
			return "", fmt.Errorf("the retry having %s was answered with %d elicitations, want an error result", c.word, len(final.urlElicitations()))
		case !final.result.IsError || !strings.Contains(final.result.text(), c.word):
			return "", fmt.Errorf("the retry having %s answered isError=%v %q, want an error result saying %s", c.word, final.result.IsError, excerpt(final.result.text(), 200), c.word)
		}
		confirmation, _, err := p.readConfirmation(ctx, rest, operation, p.adminToken)
		if err != nil {
			return "", err
		}
		if confirmation.State != operationCancelled {
			return "", fmt.Errorf("operation %s reads %q after the person %s, want %s", operation, confirmation.State, c.word, operationCancelled)
		}
		if err := nothingWritten(ctx, p.namespace, c.model, "tenant-credential-"+c.model); err != nil {
			return "", fmt.Errorf("after the person %s: %w", c.word, err)
		}
		outcomes = append(outcomes, fmt.Sprintf("%s: isError %q, operation %s %s, no ModelConfig %s", c.action, excerpt(final.result.text(), 80), operation, operationCancelled, c.model))
	}
	return strings.Join(outcomes, "; "), nil
}

// completionsScoped: any notifications/elicitation/complete reaches only
// the client that initiated the elicitation, carrying its elicitationId
// (MCP 2025-11-25 client/elicitation). A server MAY send none, so its
// absence is reported, not failed. Both 2025-11-25 clients listened on
// their standalone stream since they connected.
func (p *credentialsProof) completionsScoped() (string, error) {
	if p.legacy == nil || p.plain == nil {
		return "", errors.New("no 2025-11-25 clients")
	}
	if got := p.plain.completedElicitations(); len(got) > 0 {
		return "", fmt.Errorf("the client without URL elicitation, sent no elicitation, was notified complete for %v", got)
	}
	got := p.legacy.completedElicitations()
	for _, id := range got {
		if id != p.legacyElicitationID {
			return "", fmt.Errorf("the 2025-11-25 client was notified complete for elicitationId %q, it was given %s", id, p.legacyElicitationID)
		}
	}
	if len(got) == 0 {
		return "no notifications/elicitation/complete reached either 2025-11-25 client (a server MAY send none; muster relays none of a backend's until giantswarm/muster#1830); the client without URL elicitation was notified of nothing", nil
	}
	return fmt.Sprintf("the initiating client was notified complete for its elicitationId %s only; the client without URL elicitation was notified of nothing", p.legacyElicitationID), nil
}

// oneURLElicitation is the answer's one URL elicitation.
func oneURLElicitation(a *musterAnswer) (mcp.ElicitationParams, error) {
	es := a.urlElicitations()
	if len(es) != 1 {
		return mcp.ElicitationParams{}, fmt.Errorf("the answer carries %d URL elicitations, want one", len(es))
	}
	return es[0], nil
}

// formLink checks a link is the credential UI URL template expanded for the
// part's organization and returns the operation it carries.
func (p *credentialsProof) formLink(link string) (string, error) {
	return formOperation(p.pre.agentManager[flagCredentialUIURL], link, organizationsRootNamespace, credentialsOrganization)
}

// plainGeneration: the plain client's answer carries the link in
// credentials[].next.
func (p *credentialsProof) plainGeneration() (string, error) {
	if p.formURL == "" {
		return "", errors.New("no pending operation from the plain client's call")
	}
	operation, err := p.formLink(p.formURL)
	if err != nil {
		return "", err
	}
	if operation != p.operation {
		return "", fmt.Errorf("the link %s carries operation %s, the answer names %s", p.formURL, operation, p.operation)
	}
	return fmt.Sprintf("credentials[].next carries %s (operation %s), the credential UI URL template expanded with the operation, the parent and the organization only", p.formURL, p.operation), nil
}

// resourceLinkCheck is the structuredContent and resource_link of the plain
// client's answer, skipped on an agent-manager that predates them.
func (p *credentialsProof) resourceLinkCheck(uiSkip string) organizationCheck {
	c := organizationCheck{surface: surfaceAgentManager, boundary: "a client without URL elicitation gets structuredContent and a resource_link to the form", skip: uiSkip}
	if c.skip == "" && len(p.pre.outputSchema) == 0 {
		c.skip = fmt.Sprintf("agent-manager %s declares no outputSchema on set_organization_model: it predates structuredContent and the resource_link on credential writes (giantswarm/agent-manager#329)", p.agentManagerVersion)
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
		if err := conformsTo(p.pre.outputSchema, answer.result.StructuredContent); err != nil {
			return "", fmt.Errorf("structuredContent does not conform to the declared outputSchema: %w", err)
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
		return "structuredContent conforms to the declared outputSchema and names the pending credential; a resource_link block carries " + p.formURL, nil
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

// do sends one request with the person's token, anonymous without one, and
// reads the answer; the request body is never part of an error.
func (r *credentialREST) do(ctx context.Context, method, operation, token string, header http.Header, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.base+credentialOperationsPath+operation, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(req.Header, header)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, credentialOperationsPath+operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, raw, err
}

// otherPersonRefused: an operation is bound to its preparer's issuer and
// subject, as the phishing rule of agent-manager's credential operations
// requires. The admin reads their own operation; the other person, an
// admin of the organization too, is refused it with the answer of an
// operation that does not exist (no oracle), and an anonymous read is
// refused.
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
	if err := rest.ownRead(ctx, p.admin.Email, p.adminToken, p.operation); err != nil {
		return "", err
	}
	unknown, err := rest.refusedAsUnknown(ctx, p.other.Email, p.otherToken, p.admin.Email, p.operation, p.namespace, p.value)
	if err != nil {
		return "", err
	}
	anonymous, _, _, err := rest.get(ctx, "", p.operation)
	if err != nil {
		return "", err
	}
	if anonymous != http.StatusUnauthorized {
		return "", fmt.Errorf("an anonymous GET of the operation answered %d, want 401", anonymous)
	}
	return fmt.Sprintf("%s's own GET 200; %s, %s of %s too, GET %d as an unknown operation's; anonymous 401",
		p.admin.Email, p.other.Email, organizationAdminRole, credentialsOrganization, unknown), nil
}

// otherOwnOperation: the other person prepares an operation of their own,
// reads it, and the admin is refused it as an unknown operation. While
// agent-manager refuses that person's own preparation
// (giantswarm/agent-manager#359) it is a SKIP, and so is the cancel of it.
func (p *credentialsProof) otherOwnOperation() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*musterMCPTimeout)
	defer cancel()
	other, err := dialMusterMCP(ctx, p.cfg, p.otherToken, musterMCPOptions{name: "agentlab-credentials-other", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	defer other.close()
	args := withArgs(p.modelArgs(), map[string]any{nameKey: credentialsOtherModel, modeArg: modeApply})
	answer, result, err := setModelWith(ctx, other, args, mcp.MultiRoundTripParams{})
	if reason := ownCredentialRefused(p.other.Email, p.agentManagerVersion, err); reason != "" {
		p.otherBlocked = reason
		return "", skipCheck("%s", reason)
	}
	if err != nil {
		return "", err
	}
	if answer.result == nil {
		return "", fmt.Errorf("%s's client without URL elicitation was answered with %d elicitations", p.other.Email, len(answer.urlElicitations()))
	}
	if p.otherOperation, _, err = pendingCredential(*result, credentialsOtherModelSecret, credentialsModelKey); err != nil {
		return "", fmt.Errorf("%s's set_organization_model %s: %w", p.other.Email, credentialsOtherModel, err)
	}
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	if err := rest.ownRead(ctx, p.other.Email, p.otherToken, p.otherOperation); err != nil {
		return "", err
	}
	unknown, err := rest.refusedAsUnknown(ctx, p.admin.Email, p.adminToken, p.other.Email, p.otherOperation, p.namespace, p.value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (outside %s) prepared operation %s: their own GET 200; %s's GET %d as an unknown operation's",
		p.other.Email, platformAdminsGroup, p.otherOperation, p.admin.Email, unknown), nil
}

// get GETs an operation with token, anonymous without one.
func (r *credentialREST) get(ctx context.Context, token, operation string) (int, []byte, http.Header, error) {
	resp, raw, err := r.do(ctx, http.MethodGet, operation, token, nil, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// ownRead checks a person reads their own operation.
func (r *credentialREST) ownRead(ctx context.Context, who, token, operation string) error {
	code, _, _, err := r.get(ctx, token, operation)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("%s's GET of their own operation %s answered %d, want 200", who, operation, code)
	}
	return nil
}

// refusedAsUnknown checks who's GET of owner's operation answers what an
// unknown operation answers them (404 or 403), carrying neither the
// operation's namespace nor an ETag, and returns that status.
func (r *credentialREST) refusedAsUnknown(ctx context.Context, who, token, owner, operation, namespace, value string) (int, error) {
	unknown, _, _, err := r.get(ctx, token, randomHex(24))
	if err != nil {
		return 0, err
	}
	code, raw, header, err := r.get(ctx, token, operation)
	if err != nil {
		return 0, err
	}
	switch {
	case code != unknown:
		return 0, fmt.Errorf("%s's GET of %s's operation answered %d, an unknown operation %d: the refusal tells the operation exists: %s", who, owner, code, unknown, excerpt(redactValue(string(raw), value), 200))
	case code != http.StatusNotFound && code != http.StatusForbidden:
		return 0, fmt.Errorf("%s's GET of %s's operation answered %d, want 404 or 403", who, owner, code)
	case bytes.Contains(raw, []byte(namespace)) || header.Get("ETag") != "":
		return 0, fmt.Errorf("%s's refused GET still carries the operation's organization or an ETag", who)
	}
	return code, nil
}

// readConfirmation GETs an operation as token and decodes the confirmation
// with its strong ETag.
func (p *credentialsProof) readConfirmation(ctx context.Context, rest *credentialREST, operation, token string) (*confirmation, string, error) {
	resp, raw, err := rest.do(ctx, http.MethodGet, operation, token, nil, nil)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s answered %d: %s", operation, resp.StatusCode, excerpt(redactValue(string(raw), p.value), 300))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		return nil, "", fmt.Errorf("GET %s answered Cache-Control %q, want no-store", operation, cc)
	}
	etag, err := strongETag(resp.Header.Get("ETag"))
	if err != nil {
		return nil, "", err
	}
	var c confirmation
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, "", fmt.Errorf("the confirmation of %s: %w", operation, err)
	}
	return &c, etag, nil
}

// credentialsSaveTimeout bounds the save's exchanges: the refused saves,
// the save itself (agent-manager gives a commit-mode save 90 seconds) and
// each generation's retry.
const credentialsSaveTimeout = 4 * time.Minute

// headerIfMatch is the precondition header of a save and a cancel (RFC
// 9110 section 13.1.1).
const headerIfMatch = "If-Match"

// staleETag is an entity tag no operation carries.
const staleETag = `"agentlab-stale"`

// cancel: the other person cancels their operation with DELETE and its
// ETag, after a stale ETag is refused; the operation then reads cancelled,
// a save with its ETag is refused, and nothing of the model is written.
func (p *credentialsProof) cancel() (string, error) {
	if p.otherOperation == "" && p.otherBlocked != "" {
		return "", skipCheck("the cancel is the other person's own operation: %s", p.otherBlocked)
	}
	if p.otherOperation == "" {
		return "", errors.New("no operation of the other person to cancel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	c, etag, err := p.readConfirmation(ctx, rest, p.otherOperation, p.otherToken)
	if err != nil {
		return "", err
	}
	if c.State != operationPending {
		return "", fmt.Errorf("operation %s reads %q before the cancel, want %s", p.otherOperation, c.State, operationPending)
	}
	resp, _, err := rest.do(ctx, http.MethodDelete, p.otherOperation, p.otherToken, http.Header{headerIfMatch: {staleETag}}, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		return "", fmt.Errorf("DELETE with a stale If-Match answered %d, want 412", resp.StatusCode)
	}
	resp, raw, err := rest.do(ctx, http.MethodDelete, p.otherOperation, p.otherToken, http.Header{headerIfMatch: {etag}}, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("DELETE with If-Match answered %d, want 204: %s", resp.StatusCode, excerpt(string(raw), 200))
	}
	c, etag, err = p.readConfirmation(ctx, rest, p.otherOperation, p.otherToken)
	if err != nil {
		return "", err
	}
	if c.State != operationCancelled {
		return "", fmt.Errorf("operation %s reads %q after the cancel, want %s", p.otherOperation, c.State, operationCancelled)
	}
	body, err := credentialSaveBody(credentialsOtherModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	resp, raw, err = rest.do(ctx, http.MethodPost, p.otherOperation, p.otherToken, http.Header{headerContentType: {mediaTypeJSON}, headerIfMatch: {etag}}, body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		return "", fmt.Errorf("a save of the cancelled operation answered %d, want 412: %s", resp.StatusCode, excerpt(redactValue(string(raw), p.value), 200))
	}
	if err := nothingWritten(ctx, p.namespace, credentialsOtherModel, credentialsOtherModelSecret); err != nil {
		return "", fmt.Errorf("after the cancel: %w", err)
	}
	return fmt.Sprintf("%s's operation %s: DELETE with a stale If-Match 412, with its ETag 204; it reads %s, a save with its ETag 412, no ModelConfig %s and no value in %s",
		p.other.Email, p.otherOperation, operationCancelled, credentialsOtherModel, credentialsOtherModelSecret), nil
}

// nothingWritten checks a model has neither a ModelConfig nor a value in
// its credential Secret.
func nothingWritten(ctx context.Context, ns, model, secret string) error {
	_, found, err := secretValueSum(ctx, ns, secret, credentialsModelKey)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("the Secret %s/%s holds a value", ns, secret)
	}
	return modelConfigPresent(ctx, ns, model, false)
}

// modelConfigPresent checks the ModelConfig ns/name exists, or does not when
// want is false.
func modelConfigPresent(ctx context.Context, ns, name string, want bool) error {
	mc, err := modelConfigGVR()
	if err != nil {
		return err
	}
	exists, err := objectExists(ctx, mc, ns, name)
	switch {
	case err != nil:
		return fmt.Errorf("reading the ModelConfig %s/%s: %w", ns, name, err)
	case exists && !want:
		return fmt.Errorf("the ModelConfig %s/%s exists", ns, name)
	case !exists && want:
		return fmt.Errorf("the ModelConfig %s/%s is missing", ns, name)
	}
	return nil
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
	// References are the objects the operation writes that name a
	// requested Secret; Changes the writes themselves.
	References []credentialReference `json:"references"`
	Changes    []credentialChange    `json:"changes"`
}

// save drives the credential API as the portal's form: the confirmation,
// two refused saves (no If-Match, a stale one) that write nothing, then the
// value with If-Match. The original call retried on the plain client and on
// the 2026-07-28 one returns the save's receipt.
func (p *credentialsProof) save() (string, error) {
	if p.operation == "" {
		return "", errors.New("no pending operation to save")
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialsSaveTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	c, etag, err := p.readConfirmation(ctx, rest, p.operation, p.adminToken)
	if err != nil {
		return "", err
	}
	switch {
	case c.ID != p.operation || c.State != operationPending || c.Mode != p.mode || c.Organization.Namespace != p.namespace:
		return "", fmt.Errorf("the confirmation names operation %q state %q mode %q in %q, want %s %s %s in %s", c.ID, c.State, c.Mode, c.Organization.Namespace, p.operation, operationPending, p.mode, p.namespace)
	case p.mode == modeCommit && (c.GitTarget == nil || c.GitTarget.Repository != p.pre.agentManager[flagOrgsRepository]):
		return "", fmt.Errorf("the confirmation's git target %+v is not %s", c.GitTarget, p.pre.agentManager[flagOrgsRepository])
	}
	if err := confirmationListsOnly(*c, organizationModelConfigKind, credentialsModel, []credentialReference{{Kind: organizationModelConfigKind, Name: credentialsModel, Key: credentialsModelKey}}); err != nil {
		return "", err
	}
	missingIfMatch, ifMatchNote := ifMatchRequiredStatuses(p.agentManagerVersion)
	body, err := credentialSaveBody(credentialsModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	post := func(ifMatch string) (*http.Response, []byte, error) {
		header := http.Header{headerContentType: {mediaTypeJSON}}
		if ifMatch != "" {
			header.Set(headerIfMatch, ifMatch)
		}
		return rest.do(ctx, http.MethodPost, p.operation, p.adminToken, header, body)
	}
	refusals := make([]string, 0, 2)
	for _, refused := range []struct {
		ifMatch string
		want    []int
	}{
		{"", missingIfMatch},
		{staleETag, []int{http.StatusPreconditionFailed}},
	} {
		resp, raw, err := post(refused.ifMatch)
		if err != nil {
			return "", err
		}
		if !slices.Contains(refused.want, resp.StatusCode) {
			return "", fmt.Errorf("POST with If-Match %q answered %d, want %v: %s", refused.ifMatch, resp.StatusCode, refused.want, excerpt(redactValue(string(raw), p.value), 300))
		}
		refusals = append(refusals, fmt.Sprintf("%s %d", firstNonEmpty(refused.ifMatch, "no If-Match"), resp.StatusCode))
	}
	if again, _, err := p.readConfirmation(ctx, rest, p.operation, p.adminToken); err != nil || again.State != operationPending {
		return "", fmt.Errorf("after the refused saves the operation is not %s (%+v, %v)", operationPending, again, err)
	}
	if err := nothingWritten(ctx, p.namespace, credentialsModel, credentialsModelSecret); err != nil {
		return "", fmt.Errorf("after the refused saves: %w", err)
	}
	resp, raw, err := post(etag)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s answered %d: %s", p.operation, resp.StatusCode, excerpt(redactValue(string(raw), p.value), 300))
	}
	if how := valueCarried(string(raw), p.value); how != "" {
		return "", fmt.Errorf("the receipt carries the value %s", how)
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
	if err := receiptNames(receipt, p.operation, credentialsModelSecret, credentialsModelKey, want); err != nil {
		return "", fmt.Errorf("the receipt: %w", err)
	}
	saved, err := liveVersions(ctx, p.namespace, credentialsModel, credentialsModelSecret)
	if err != nil {
		return "", err
	}
	again, err := p.secondSave(post, etag, receipt, want)
	if err != nil {
		return "", err
	}
	retried, err := p.retriesReturn(ctx, receipt, want)
	if err != nil {
		return "", err
	}
	after, err := liveVersions(ctx, p.namespace, credentialsModel, credentialsModelSecret)
	if err != nil {
		return "", err
	}
	if after != saved {
		return "", fmt.Errorf("the second POST or the retries wrote again: the Secret's and the ModelConfig's resourceVersions %v after the save, %v after them", saved, after)
	}
	detail := fmt.Sprintf("ETag %s, Cache-Control no-store, the confirmation lists only ModelConfig %s (key %s); POST refused with %s%s, nothing written; POST with If-Match saved %s/%s (%s) as %s; %s; %s, the Secret's and the ModelConfig's resourceVersions unchanged",
		etag, credentialsModel, credentialsModelKey, strings.Join(refusals, " and "), ifMatchNote, credentialsModelSecret, credentialsModelKey, shortSum(p.valueSum), want, again, retried)
	if p.mode == modeCommit {
		if err := nothingWritten(ctx, p.namespace, credentialsModel, credentialsModelSecret); err != nil {
			return "", fmt.Errorf("mode commit, before the pull request is merged: %w", err)
		}
		branches, err := personGH("pr", "list", "--repo", receipt.Commit.Repository, "--state", "open", "--head", receipt.Commit.Branch, "--json", "number", "--jq", ".[].number")
		if err != nil {
			return "", err
		}
		if open := strings.Fields(branches); len(open) != 1 || open[0] != strconv.Itoa(receipt.Commit.Number) {
			return "", fmt.Errorf("open pull requests on %s's branch %s: %v, want only #%d", receipt.Commit.Repository, receipt.Commit.Branch, open, receipt.Commit.Number)
		}
		return detail + "; pull request " + receipt.Commit.PullRequest + ", the one open on " + receipt.Commit.Branch + "; before its merge no live Secret value and no ModelConfig", nil
	}
	sum, found, err := secretValueSum(ctx, p.namespace, credentialsModelSecret, credentialsModelKey)
	if err != nil {
		return "", err
	}
	if !found || sum != p.valueSum {
		return "", fmt.Errorf("mode apply: Secret %s/%s holds %s, want %s", p.namespace, credentialsModelSecret, shortSum(sum), shortSum(p.valueSum))
	}
	if err := modelConfigPresent(ctx, p.namespace, credentialsModel, true); err != nil {
		return "", fmt.Errorf("mode apply, after the save: %w", err)
	}
	p.live = true
	return detail + "; mode apply: the live Secret's SHA-256 matches and ModelConfig " + credentialsModel + " exists", nil
}

// retriesReturn retries the original call on the plain client and, when it
// ran, on the 2026-07-28 client having accepted. A retry bound to the saved
// operation must answer its receipt, not another write; the 2026-07-28
// client bound to an operation of its own stays input_required for it, as a
// 2026-07-28 retry does until its operation completes. No answer carries the
// value.
func (p *credentialsProof) retriesReturn(ctx context.Context, receipt credentialAnswer, want string) (string, error) {
	type retry struct {
		name      string
		m         *musterMCP
		roundTrip mcp.MultiRoundTripParams
		operation string
	}
	retries := []retry{{mcp.ProtocolVersion20251125, p.plain, mcp.MultiRoundTripParams{}, p.operation}}
	if p.modernAnswer != nil {
		retries = append(retries, retry{mcp.ProtocolVersion20260728, p.modern, p.modernAnswer.accepted(), p.modernOperation})
	}
	outcomes := make([]string, 0, len(retries))
	for _, r := range retries {
		if r.operation != p.operation {
			_, operation, err := p.inputRequired(ctx, r.roundTrip)
			if err != nil {
				return "", fmt.Errorf("the original call retried on %s, bound to its own operation %s: %w", r.name, r.operation, err)
			}
			if operation != r.operation {
				return "", fmt.Errorf("the original call retried on %s names operation %s, want its own %s", r.name, operation, r.operation)
			}
			outcomes = append(outcomes, fmt.Sprintf("on %s, bound to its own operation %s, input_required still", r.name, r.operation))
			continue
		}
		answer, result, err := setModelWith(ctx, r.m, p.modelArgs(), r.roundTrip)
		if err != nil {
			return "", fmt.Errorf("the original call retried on %s: %w", r.name, err)
		}
		if answer.result == nil {
			return "", fmt.Errorf("the original call retried on %s does not return the receipt: it was answered with %d elicitations", r.name, len(answer.urlElicitations()))
		}
		if how := valueCarried(answer.result.text()+string(answer.result.StructuredContent), p.value); how != "" {
			return "", fmt.Errorf("the original call retried on %s carries the value %s", r.name, how)
		}
		if err := receiptNames(*result, p.operation, credentialsModelSecret, credentialsModelKey, want); err != nil {
			return "", fmt.Errorf("the original call retried on %s: %w", r.name, err)
		}
		if err := sameReceipt(receipt, *result); err != nil {
			return "", fmt.Errorf("the original call retried on %s: %w", r.name, err)
		}
		outcomes = append(outcomes, "on "+r.name+" the same receipt")
	}
	return "the original call retried " + strings.Join(outcomes, "; "), nil
}

// liveVersions are the resourceVersions of the model's credential Secret and
// ModelConfig, "" for one that does not exist: a retry that writes again
// changes one of them.
func liveVersions(ctx context.Context, ns, model, secret string) ([2]string, error) {
	mc, err := modelConfigGVR()
	if err != nil {
		return [2]string{}, err
	}
	var out [2]string
	for i, read := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{gvrSecrets, secret}, {mc, model}} {
		obj, err := getObject(ctx, read.gvr, ns, read.name)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return [2]string{}, err
		default:
			out[i] = obj.GetResourceVersion()
		}
	}
	return out, nil
}

// credentialLogSources are the components a credential value passes
// through on its way in: agent-manager takes the save, muster the calls.
var credentialLogSources = []string{agentManagerMCPServer, componentMuster}

// valueNotLogged reads agent-manager's and muster's logs since the run
// began and fails when one carries the value, plain or base64.
func (p *credentialsProof) valueNotLogged() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*kubeReadTimeout)
	defer cancel()
	since := time.Since(p.started) + 5*time.Second
	var read []string
	for _, component := range credentialLogSources {
		logs, err := podLogs(ctx, platformNamespace, "deploy/"+component, component, since)
		if err != nil {
			return "", err
		}
		lines, err := judgeComponentLogs(component, logs, p.value)
		if err != nil {
			return "", err
		}
		read = append(read, fmt.Sprintf("%s (%d lines)", component, lines))
	}
	return fmt.Sprintf("the logs of %s since the run began carry neither the value (%s) nor its base64", strings.Join(read, " and "), shortSum(p.valueSum)), nil
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
	modelPath := credentialsContentPath(p.pre.orgsContentPrefix()) + "/modelconfig-" + credentialsModel + ".yaml"
	seen, seenModel := false, false
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
		seenModel = seenModel || f.Filename == modelPath
	}
	switch {
	case !seen:
		return "", fmt.Errorf("pull request %s carries no %s", c.PullRequest, secretPath)
	case !seenModel:
		return "", fmt.Errorf("pull request %s carries no %s: the consumer and its Secret belong in one pull request", c.PullRequest, modelPath)
	}
	return fmt.Sprintf("%s on %s opened by %s, its commit authored by %s (committer %s); %s and %s in it, the Secret SOPS-encrypted, no file carries the value", c.PullRequest, c.Repository, author, commitAuthor, committer, modelPath, secretPath), nil
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
	if err := modelConfigPresent(ctx, p.namespace, credentialsModel, true); err != nil {
		return "", fmt.Errorf("after the apply: %w", err)
	}
	p.live = true
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
// admin: what it erases (the model and its credential Secret among it when
// they are live: delete_organization lists the cluster's objects, never a
// pull request's), the confirmed dry run, the delete; then the
// namespace, the operator's Kustomization and ServiceAccount for it in the
// Flux namespace (both named after the namespace) and every RoleBinding
// referring to the organization are gone.
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
	case p.live && (!slices.Contains(preview.Erases.Models, credentialsModel) || !slices.Contains(preview.Erases.Credentials, credentialsModelSecret)):
		return "", fmt.Errorf("delete_organization without confirm names models %v and credentials %v, want %s and %s among them", preview.Erases.Models, preview.Erases.Credentials, credentialsModel, credentialsModelSecret)
	case dry.Deleted || !dry.DryRun:
		return "", fmt.Errorf("the confirmed dry run answered deleted=%v dryRun=%v", dry.Deleted, dry.DryRun)
	case !done.Deleted:
		return "", errors.New("delete_organization with the typed confirmation answered deleted=false")
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	fluxNS := p.pre.operator[flagFluxNamespace]
	var last string
	gone := waitFor(int(organizationCleanupWait/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		left := map[string]error{
			"AgentPlatformOrganization": absent(getObject(ctx, gvr, organizationsRootNamespace, credentialsOrganization)),
			"Namespace":                 absent(getObject(ctx, gvrNamespaces, "", p.namespace)),
		}
		if fluxNS != "" {
			left["Kustomization"] = absent(getObject(ctx, kustomizationGVR, fluxNS, p.namespace))
			left["ServiceAccount"] = absent(getObject(ctx, gvrServiceAccounts, fluxNS, p.namespace))
		}
		last = fmt.Sprint(left)
		for _, err := range left {
			if err != nil {
				return false
			}
		}
		return true
	})
	if !gone {
		return "", fmt.Errorf("organization %s, namespace %s and its Flux objects not gone within %s (%s)", confirm, p.namespace, organizationCleanupWait, last)
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
	fluxGone := ""
	if fluxNS != "" {
		fluxGone = ", its Kustomization and ServiceAccount in " + fluxNS + " gone"
	}
	return fmt.Sprintf("without confirm: erases models %v, credentials %v, %d members in %s; the confirmed dry run deleted nothing; the delete erased it, namespace gone%s, no RoleBinding of %d refers to it",
		preview.Erases.Models, preview.Erases.Credentials, len(preview.Erases.Members), preview.Erases.Namespace, fluxGone, len(bindings.Items)), nil
}

// absent is nil when a read found nothing, else why the object is not
// gone.
func absent(_ *unstructured.Unstructured, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err == nil:
		return errors.New("still present")
	}
	return err
}

// withArgs is base with more arguments.
func withArgs(base, more map[string]any) map[string]any {
	out := maps.Clone(base)
	maps.Copy(out, more)
	return out
}

// cleanup closes the client, closes a pull request nothing merged with its
// branch, and after a merge removes the organization's content path from the
// organizations repository's base, so the next run's create finds no file. A
// failed run keeps the organization for diagnosis, and after a merge its
// content path too: Flux would prune what is kept, so the run prints the
// removal instead.
func (p *credentialsProof) cleanup() {
	for _, m := range []*musterMCP{p.plain, p.modern, p.legacy} {
		if m != nil {
			m.close()
		}
	}
	if p.namespace != "" {
		removeCredentialsSignInServer(p.namespace)
	}
	if p.retainForDiagnostics && p.namespace != "" {
		note("retaining organization %s/%s (namespace %s) for diagnosis", organizationsRootNamespace, credentialsOrganization, p.namespace)
	}
	if p.receipt != nil && p.receipt.Commit != nil && !p.merged {
		c := p.receipt.Commit
		if _, err := personGH("pr", "close", strconv.Itoa(c.Number), "--repo", c.Repository, "--delete-branch"); err != nil {
			note("cleanup: closing %s: %v", c.PullRequest, err)
		} else {
			note("cleanup: closed %s and deleted its branch", c.PullRequest)
		}
	}
	if !p.merged {
		return
	}
	repo, base, path := p.pre.agentManager[flagOrgsRepository], p.pre.orgsBranch(), credentialsContentPath(p.pre.orgsContentPrefix())
	if p.retainForDiagnostics {
		note("retaining %s on %s@%s, which Flux keeps applied; remove it by hand before the next run:\n%s", path, repo, base, contentPathRemovalCommand(repo, base, path))
		return
	}
	if err := removeContentPath(repo, base, path); err != nil {
		note("cleanup: %v", err)
	}
}

// credentialsContentPath is the part's organization's directory in the
// organizations repository.
func credentialsContentPath(prefix string) string {
	return strings.Join([]string{prefix, organizationsRootNamespace, credentialsOrganization}, "/")
}

// contentPathRemovalCommand is the shell a person runs to remove path from
// repo's base branch by hand.
func contentPathRemovalCommand(repo, base, path string) string {
	return fmt.Sprintf("  git clone -q --depth 1 --branch %[2]s %[4]s/%[1]s.git agentlab-credentials-cleanup && \\\n"+
		"  git -C agentlab-credentials-cleanup rm -r -q %[3]s && \\\n"+
		"  git -C agentlab-credentials-cleanup commit -q -m 'chore: remove the agentlab credentials proof organization content' && \\\n"+
		"  git -C agentlab-credentials-cleanup push -q origin HEAD:%[2]s", repo, base, path, measureGitURL)
}

// removeContentPath commits the removal of path to repo's base with the
// person's git credentials.
func removeContentPath(repo, base, path string) error {
	dir, err := os.MkdirTemp(StateDir, "credentials-cleanup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	clone := filepath.Join(dir, "repo")
	steps := [][]string{
		{"clone", "-q", "--depth", "1", "--branch", base, measureGitURL + "/" + repo + ".git", clone},
		{"-C", clone, "rm", "-r", "-q", "--ignore-unmatch", path},
		{"-C", clone, "commit", "-q", "-m", "chore: remove the agentlab credentials proof organization content"},
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
