package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// The credentials part's refusals and confirmations beyond the save
// (giantswarm/agentlab#1236 against giantswarm/agent-manager#293): a person
// without rights on the organization, a Secret another consumer came to
// reference, what the confirmations of a connector and an egress policy
// list, and a second POST of a saved operation.

const (
	// credentialsRightsSettle bounds the wait for the part's RoleBindings to
	// show in the people's access reviews.
	credentialsRightsSettle = 15 * time.Second
	// credentialsViewerBinding binds the part's viewer to the organization's
	// read-only role, agent-platform:user.
	credentialsViewerBinding = "agentlab-credentials-viewer" // #nosec G101 -- a RoleBinding name, not a credential
	// credentialsViewerModel is the model a viewer tries to add.
	credentialsViewerModel = "agentlab-credentials-viewer" // #nosec G101 -- a ModelConfig name, not a credential
	// credentialsSharedModel is the model whose Secret another consumer,
	// credentialsSharedOther, comes to reference before the save.
	credentialsSharedModel  = "agentlab-credentials-shared"                 // #nosec G101 -- a ModelConfig name, not a credential
	credentialsSharedOther  = "agentlab-credentials-shared-other"           // #nosec G101 -- a ModelConfig name, not a credential
	credentialsSharedSecret = "tenant-credential-" + credentialsSharedModel // #nosec G101 -- a Secret's NAME, not a credential
	// credentialsConnector and credentialsEgress are the connector and the
	// egress policy whose confirmations the part reads, then cancels.
	credentialsConnector    = "agentlab-credentials-connector"               // #nosec G101 -- a RemoteMCPServer name, not a credential
	credentialsConnectorURL = "https://mcp.agentlab-credentials.example/mcp" // #nosec G101 -- a URL, not a credential
	credentialsEgress       = "agentlab-credentials-egress"                  // #nosec G101 -- an EgressPolicy name, not a credential
	// credentialTokenKey is the key agent-manager names a connector's and
	// an egress policy's credential by default.
	credentialTokenKey = "token"
	// egressPolicyResource is kagent's EgressPolicy, which agent-manager
	// writes for set_organization_egress_policy.
	egressPolicyResource = "egresspolicies." + kagentAPIGroup
	egressPolicyKind     = "EgressPolicy"
	// The access reviews' verbs and the Secret resource.
	reviewVerbList   = "list"
	reviewVerbCreate = "create"
	resourceSecrets  = "secrets"
	// egressOriginKey is a destination's origin in an egress policy's
	// arguments.
	egressOriginKey = "origin"
	// headerContentType and mediaTypeJSON are the save's body type.
	headerContentType = "Content-Type"
	mediaTypeJSON     = "application/json"
)

// The egress policy's origins: two that get the credential, one reached
// without it, which no confirmation may list as receiving it.
const (
	credentialsEgressOrigin      = "https://api.agentlab-credentials.example"     // #nosec G101 -- an origin, not a credential
	credentialsEgressOtherOrigin = "https://uploads.agentlab-credentials.example" // #nosec G101 -- an origin, not a credential
	credentialsEgressPlainOrigin = "https://public.agentlab-credentials.example"  // #nosec G101 -- an origin, not a credential
)

// organizationRights are a person's rights in an organization namespace as
// SelfSubjectAccessReviews with their own token read them: the reads and
// writes agent-manager's calls as the person meet.
type organizationRights struct {
	// listModelConfigs is the read every role of the organization holds;
	// createSecrets and createModelConfigs are the admin's writes.
	listModelConfigs, createSecrets, createModelConfigs bool
}

// readOrganizationRights reads a person's organizationRights in ns.
func readOrganizationRights(token, ns string) (organizationRights, error) {
	_, cs, err := personClients(token)
	if err != nil {
		return organizationRights{}, err
	}
	mc, err := modelConfigGVR()
	if err != nil {
		return organizationRights{}, err
	}
	var r organizationRights
	for _, q := range []struct {
		into                  *bool
		verb, group, resource string
	}{
		{&r.listModelConfigs, reviewVerbList, mc.Group, mc.Resource},
		{&r.createSecrets, reviewVerbCreate, "", resourceSecrets},
		{&r.createModelConfigs, reviewVerbCreate, mc.Group, mc.Resource},
	} {
		if *q.into, err = selfAccess(cs, ns, q.verb, q.group, q.resource); err != nil {
			return organizationRights{}, err
		}
	}
	return r, nil
}

// checkPeople holds the part's people to their roles with their own
// access reviews, before any verdict stands on them: the viewer reads the
// organization's ModelConfigs and creates neither Secrets nor ModelConfigs,
// the other admin creates both. A wrong fixture fails the part.
func (p *credentialsProof) checkPeople() error {
	var problem string
	var readErr error
	// The RoleBindings setUp just wrote reach the apiserver's authorizer
	// through its informer: the reviews are read again for a few seconds.
	waitFor(int(credentialsRightsSettle/pollInterval), pollInterval, func() bool {
		viewer, err := readOrganizationRights(p.viewerToken, p.namespace)
		if err == nil {
			var other organizationRights
			if other, err = readOrganizationRights(p.otherToken, p.namespace); err == nil {
				problem = firstNonEmpty(viewerFixtureProblem(p.viewer.Email, p.namespace, viewer), otherAdminFixtureProblem(p.other.Email, p.namespace, other))
			}
		}
		readErr = err
		return err == nil && problem == ""
	})
	switch {
	case readErr != nil:
		return readErr
	case problem != "":
		return errors.New(problem)
	}
	note("access reviews in %s: %s lists modelconfigs and creates neither secrets nor modelconfigs; %s creates both", p.namespace, p.viewer.Email, p.other.Email)
	return nil
}

// viewerWriteRefused: the viewer, bound agent-platform:user in the
// organization by setUp, whose access reviews setUp held to reading its
// ModelConfigs and not creating Secrets and ModelConfigs there, calls set_organization_model for a model of their own.
// The refusal must be that authorization: the apiserver's RBAC denial of
// the viewer quoted in agent-manager's forbidden (from 2.24.0-rc.2 its
// dry-run create of the Secret as the caller, giantswarm/agent-manager#360).
// An older agent-manager refuses every person outside platform-admins on
// reading the Secret first, which says nothing of the viewer's rights: a
// SKIP naming the release that fixes it. Nothing of the model is written.
func (p *credentialsProof) viewerWriteRefused() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.viewerToken, musterMCPOptions{name: "agentlab-credentials-viewer", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	defer m.close()
	args := withArgs(p.modelArgs(), map[string]any{nameKey: credentialsViewerModel, modeArg: modeApply})
	answer, err := m.call(ctx, agentManagerToolPrefix+"set_organization_model", args, mcp.MultiRoundTripParams{})
	if err != nil {
		return "", err
	}
	refusal, err := refusedWith(answer, "forbidden")
	if err != nil {
		return "", fmt.Errorf("%s's set_organization_model %s: %w", p.viewer.Email, credentialsViewerModel, err)
	}
	if err := nothingWritten(ctx, p.namespace, credentialsViewerModel, "tenant-credential-"+credentialsViewerModel); err != nil {
		return "", fmt.Errorf("after %s's refused call: %w", p.viewer.Email, err)
	}
	denied, skip, err := viewerRefusalVerdict(p.agentManagerVersion, refusal, apiserverUserPrefix+p.viewer.Email, p.namespace)
	switch {
	case err != nil:
		return "", err
	case skip != "":
		return "", skipCheck("%s", skip)
	}
	return fmt.Sprintf("%s (%s in %s only; access reviews: list modelconfigs allowed, create secrets and modelconfigs denied): set_organization_model %s refused by the apiserver's denial of %s %q, no ModelConfig, no value",
		p.viewer.Email, organizationUserRole, p.namespace, credentialsViewerModel, denied, excerpt(refusal, 200)), nil
}

// viewerSaveRefused: the viewer's POST of the admin's pending operation,
// with its current ETag and a value, is refused as an unknown operation's
// would be (the operation is the admin's alone), nothing is written and the
// admin's operation stays pending at the same ETag.
func (p *credentialsProof) viewerSaveRefused() (string, error) {
	if p.operation == "" {
		return "", errors.New("no pending operation of the admin")
	}
	token := p.viewerToken
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	before, etag, err := p.readConfirmation(ctx, rest, p.operation, p.adminToken)
	if err != nil {
		return "", err
	}
	body, err := credentialSaveBody(credentialsModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	save := func(operation string) (int, []byte, error) {
		resp, raw, err := rest.do(ctx, http.MethodPost, operation, token, http.Header{headerContentType: {mediaTypeJSON}, headerIfMatch: {etag}}, body)
		if err != nil {
			return 0, nil, err
		}
		return resp.StatusCode, raw, nil
	}
	unknown, _, err := save(randomHex(24))
	if err != nil {
		return "", err
	}
	code, raw, err := save(p.operation)
	if err != nil {
		return "", err
	}
	switch {
	case code != http.StatusNotFound && code != http.StatusForbidden:
		return "", fmt.Errorf("%s's POST of %s's operation answered %d, want 404 or 403: %s", p.viewer.Email, p.admin.Email, code, excerpt(redactValue(string(raw), p.value), 200))
	case code != unknown:
		return "", fmt.Errorf("%s's POST of %s's operation answered %d, an unknown operation's %d: the refusal tells the operation exists", p.viewer.Email, p.admin.Email, code, unknown)
	}
	if err := nothingWritten(ctx, p.namespace, credentialsModel, credentialsModelSecret); err != nil {
		return "", fmt.Errorf("after %s's refused POST: %w", p.viewer.Email, err)
	}
	after, etagAfter, err := p.readConfirmation(ctx, rest, p.operation, p.adminToken)
	if err != nil {
		return "", err
	}
	if after.State != operationPending || before.State != operationPending || etagAfter != etag {
		return "", fmt.Errorf("%s's operation reads %q at %s after %s's refused POST, want %s at %s", p.admin.Email, after.State, etagAfter, p.viewer.Email, operationPending, etag)
	}
	return fmt.Sprintf("%s's POST of %s's operation with its ETag %d, as an unknown operation's; nothing written, the operation %s at %s",
		p.viewer.Email, p.admin.Email, code, operationPending, etag), nil
}

// sharedSecretRefusal is agent-manager's refusal to prepare a credential a
// consumer other than the one asking references.
const sharedSecretRefusal = "another consumer references the credential Secret" // #nosec G101 -- an error text, not a credential

// sharedSecretRefused: the admin prepares a model naming a new Secret;
// before the save another ModelConfig comes to reference that Secret. The
// save is refused 409 conflict with nothing written (agent-manager refuses a
// Secret referenced by another object than the one the operation names; its
// REST answer drops the reason, so only the status and code are judged,
// with nothing else changed since the confirmation), and preparing the model
// again, with a fresh requestId, is refused for that reference.
func (p *credentialsProof) sharedSecretRefused() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-shared", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	defer m.close()
	args := withArgs(p.modelArgs(), map[string]any{nameKey: credentialsSharedModel, modeArg: modeApply, "credentialSecret": credentialsSharedSecret})
	answer, result, err := setModelWith(ctx, m, args, mcp.MultiRoundTripParams{})
	if err != nil {
		return "", err
	}
	if answer.result == nil {
		return "", fmt.Errorf("a client without URL elicitation was answered with %d elicitations", len(answer.urlElicitations()))
	}
	operation, _, err := pendingCredential(*result, credentialsSharedSecret, credentialsModelKey)
	if err != nil {
		return "", err
	}
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	_, etag, err := p.readConfirmation(ctx, rest, operation, p.adminToken)
	if err != nil {
		return "", err
	}
	defer p.cancelQuietly(rest, operation)
	step("ModelConfig %s/%s comes to reference %s before the save", p.namespace, credentialsSharedOther, credentialsSharedSecret)
	if err := applyOrganizationManifest(sharedSecretConsumerManifest(p.namespace, p.cfg.AIModel)); err != nil {
		return "", err
	}
	defer removeModelConfig(p.namespace, credentialsSharedOther)
	body, err := credentialSaveBody(credentialsSharedSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	resp, raw, err := rest.do(ctx, http.MethodPost, operation, p.adminToken, http.Header{headerContentType: {mediaTypeJSON}, headerIfMatch: {etag}}, body)
	if err != nil {
		return "", err
	}
	if code := errorCode(raw); resp.StatusCode != http.StatusConflict || code != "conflict" {
		return "", fmt.Errorf("the save of a Secret %s also references answered %d %q, want 409 conflict: %s", credentialsSharedOther, resp.StatusCode, code, excerpt(redactValue(string(raw), p.value), 200))
	}
	if err := nothingWritten(ctx, p.namespace, credentialsSharedModel, credentialsSharedSecret); err != nil {
		return "", fmt.Errorf("after the refused save: %w", err)
	}
	again, err := m.call(ctx, agentManagerToolPrefix+"set_organization_model", withArgs(args, map[string]any{"requestId": randomHex(8)}), mcp.MultiRoundTripParams{})
	if err != nil {
		return "", err
	}
	refusal, err := refusedWith(again, "conflict")
	if err != nil {
		return "", fmt.Errorf("preparing %s again: %w", credentialsSharedModel, err)
	}
	if !strings.Contains(refusal, sharedSecretRefusal) {
		return "", fmt.Errorf("preparing %s again was refused %q, not for the Secret %s references", credentialsSharedModel, excerpt(refusal, 200), credentialsSharedOther)
	}
	if err := nothingWritten(ctx, p.namespace, credentialsSharedModel, credentialsSharedSecret); err != nil {
		return "", fmt.Errorf("after the refused preparation: %w", err)
	}
	return fmt.Sprintf("operation %s for %s/%s; with ModelConfig %s referencing it too, POST with If-Match answered 409 conflict, no ModelConfig %s and no value; prepared again it is refused %q",
		operation, credentialsSharedSecret, credentialsModelKey, credentialsSharedOther, credentialsSharedModel, excerpt(refusal, 160)), nil
}

// sharedSecretConsumerManifest is a ModelConfig that references the shared
// Secret: the other consumer, applied as the lab's cluster admin.
func sharedSecretConsumerManifest(ns, model string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  provider: %s
  model: %s
  apiKeySecret: %s
  apiKeySecretKey: %s
`, organizationModelConfigAPIVersion, organizationModelConfigKind, credentialsSharedOther, ns, organizationsTestLabel, organizationsTestMark,
		organizationModelProvider, model, credentialsSharedSecret, credentialsModelKey)
}

// removeModelConfig deletes a ModelConfig of the part, best effort.
func removeModelConfig(ns, name string) {
	gvr, err := modelConfigGVR()
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*kubeReadTimeout)
	defer cancel()
	if err := deleteObject(ctx, gvr, ns, name, kubeReadTimeout); err != nil && !apierrors.IsNotFound(err) {
		note("cleanup: %v", err)
	}
}

// cancelQuietly cancels a pending operation of the admin with its current
// ETag, best effort.
func (p *credentialsProof) cancelQuietly(rest *credentialREST, operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	c, etag, err := p.readConfirmation(ctx, rest, operation, p.adminToken)
	if err != nil || c.State != operationPending {
		return
	}
	resp, _, err := rest.do(ctx, http.MethodDelete, operation, p.adminToken, http.Header{headerIfMatch: {etag}}, nil)
	switch {
	case err != nil:
		note("cleanup: cancelling operation %s: %v", operation, err)
	case resp.StatusCode != http.StatusNoContent:
		note("cleanup: cancelling operation %s answered %d", operation, resp.StatusCode)
	}
}

// errorCode is the stable code of agent-manager's REST error body, "" for
// another body.
func errorCode(raw []byte) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return body.Error.Code
}

// connectorAndEgressConfirmations: the admin adds a connector and an egress
// policy without their credentials; each confirmation shows the one write
// and lists that object alone as the Secret's consumer, the connector with
// the URL it sends the value to and the egress policy with exactly the two
// origins that get it, not the one reached without it. Each is then
// cancelled with nothing written.
func (p *credentialsProof) connectorAndEgressConfirmations() (string, error) {
	connectorGVR, err := gvrFor(remoteMCPServerResource)
	if err != nil {
		return "", err
	}
	egressGVR, err := gvrFor(egressPolicyResource)
	if err != nil {
		return "", fmt.Errorf("the apiserver serves no %s, which set_organization_egress_policy writes: %w", egressPolicyResource, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, p.cfg, p.adminToken, musterMCPOptions{name: "agentlab-credentials-consumers", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	defer m.close()
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	bearer := map[string]any{"credentialHeader": authorizationHeader, "credentialPrefix": "Bearer "}
	cases := []struct {
		tool, kind, name string
		args             map[string]any
		want             credentialReference
	}{
		{
			tool: "set_organization_connector", kind: remoteMCPServerKind, name: credentialsConnector,
			args: map[string]any{namespaceKey: p.namespace, nameKey: credentialsConnector, "url": credentialsConnectorURL, "credentialHeader": authorizationHeader, modeArg: modeApply},
			want: credentialReference{Kind: remoteMCPServerKind, Name: credentialsConnector, Key: credentialTokenKey, URL: credentialsConnectorURL},
		},
		{
			tool: "set_organization_egress_policy", kind: egressPolicyKind, name: credentialsEgress,
			args: map[string]any{namespaceKey: p.namespace, nameKey: credentialsEgress, modeArg: modeApply, "destinations": []any{
				withArgs(bearer, map[string]any{egressOriginKey: credentialsEgressOrigin}),
				withArgs(bearer, map[string]any{egressOriginKey: credentialsEgressOtherOrigin}),
				map[string]any{egressOriginKey: credentialsEgressPlainOrigin},
			}},
			want: credentialReference{Kind: egressPolicyKind, Name: credentialsEgress, Key: credentialTokenKey, Origins: []string{credentialsEgressOrigin, credentialsEgressOtherOrigin}},
		},
	}
	exists := map[string]func(name string) (bool, error){
		remoteMCPServerKind: func(name string) (bool, error) { return objectExists(ctx, connectorGVR, p.namespace, name) },
		egressPolicyKind:    func(name string) (bool, error) { return objectExists(ctx, egressGVR, p.namespace, name) },
	}
	outcomes := make([]string, 0, len(cases))
	for _, c := range cases {
		secret := "tenant-credential-" + c.name
		answer, result, err := credentialToolWith(ctx, m, c.tool, c.args, mcp.MultiRoundTripParams{})
		if err != nil {
			return "", err
		}
		if answer.result == nil {
			return "", fmt.Errorf("%s: a client without URL elicitation was answered with %d elicitations", c.tool, len(answer.urlElicitations()))
		}
		operation, _, err := pendingCredential(*result, secret, credentialTokenKey)
		if err != nil {
			return "", fmt.Errorf("%s: %w", c.tool, err)
		}
		confirmation, etag, err := p.readConfirmation(ctx, rest, operation, p.adminToken)
		if err != nil {
			return "", err
		}
		if err := confirmationListsOnly(*confirmation, c.kind, c.name, []credentialReference{c.want}); err != nil {
			p.cancelQuietly(rest, operation)
			return "", fmt.Errorf("%s's confirmation: %w", c.tool, err)
		}
		resp, raw, err := rest.do(ctx, http.MethodDelete, operation, p.adminToken, http.Header{headerIfMatch: {etag}}, nil)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusNoContent {
			return "", fmt.Errorf("DELETE of %s's operation %s answered %d, want 204: %s", c.tool, operation, resp.StatusCode, excerpt(string(raw), 200))
		}
		present, err := exists[c.kind](c.name)
		if err != nil {
			return "", err
		}
		_, found, err := secretValueSum(ctx, p.namespace, secret, credentialTokenKey)
		if err != nil {
			return "", err
		}
		if present || found {
			return "", fmt.Errorf("after cancelling %s's operation: %s %s exists %v, Secret %s holds a value %v", c.tool, c.kind, c.name, present, secret, found)
		}
		origins := ""
		if len(c.want.Origins) > 0 {
			origins = ", origins " + strings.Join(c.want.Origins, " and ") + " (not " + credentialsEgressPlainOrigin + ")"
		}
		outcomes = append(outcomes, fmt.Sprintf("%s: operation %s lists only %s %s (key %s%s), cancelled, nothing written", c.tool, operation, c.kind, c.name, c.want.Key, origins))
	}
	return strings.Join(outcomes, "; "), nil
}

// secondSave posts the saved operation again with the ETag it was saved
// with: a completed operation answers its receipt, not another write (the
// caller compares the resourceVersions after it).
func (p *credentialsProof) secondSave(post func(string) (*http.Response, []byte, error), etag string, receipt credentialAnswer, want string) (string, error) {
	resp, raw, err := post(etag)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("a second POST of the saved operation answered %d, want 200 with the receipt: %s", resp.StatusCode, excerpt(redactValue(string(raw), p.value), 300))
	}
	if how := valueCarried(string(raw), p.value); how != "" {
		return "", fmt.Errorf("the second POST's receipt carries the value %s", how)
	}
	var second credentialAnswer
	if err := json.Unmarshal(raw, &second); err != nil {
		return "", fmt.Errorf("the second POST's receipt: %w", err)
	}
	if err := receiptNames(second, p.operation, credentialsModelSecret, credentialsModelKey, want); err != nil {
		return "", fmt.Errorf("the second POST's receipt: %w", err)
	}
	if err := sameReceipt(receipt, second); err != nil {
		return "", fmt.Errorf("the second POST: %w", err)
	}
	return "a second POST answered 200 with the same receipt", nil
}
