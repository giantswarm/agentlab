package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	rbacv1 "k8s.io/api/rbac/v1"
)

// The credentials part's save after lost rights (giantswarm/agentlab#1250,
// giantswarm/agent-manager#293 AC3): the other organization admin prepares
// an operation, is removed from the organization's admins, and POSTs the
// value with the confirmation's ETag. agent-manager checks the person's
// rights again on every read, save and cancel of an operation (its
// docs/organization-credentials.md, "Storage and write boundaries"): the
// save is refused before anything is written or the operation changes. With
// the rights given back, the same operation at the same ETag saves.

const (
	// credentialsLostModel is the model the other admin prepares, then
	// loses the right to save.
	credentialsLostModel       = "agentlab-credentials-lost"                 // #nosec G101 -- a ModelConfig name, not a credential
	credentialsLostModelSecret = "tenant-credential-" + credentialsLostModel // #nosec G101 -- a Secret's NAME, not a credential
	// codeForbidden is agent-manager's REST error code of an authorization
	// refusal (internal/api/rest.go errorCodes).
	codeForbidden = "forbidden"
)

// lostRightsSkip is the SKIP reason of the lost-rights verdicts on an
// agent-manager older than credentialsWriteOnlyFloor, "" from it on. Such an
// agent-manager refuses every organization admin outside platform-admins
// their own operation (it reads the credential Secret as the caller,
// giantswarm/agent-manager#359), and the lab admin, a platform admin, cannot
// lose the right: nobody can prepare an operation and then lose the right.
func lostRightsSkip(version string) string {
	if atLeast(version, credentialsWriteOnlyFloor) {
		return ""
	}
	return fmt.Sprintf("agent-manager %s refuses an organization admin outside %s their own operation (forbidden: %s), and the lab admin, a platform admin, cannot lose the right: a person who prepares and then loses it needs agent-manager %s or newer (giantswarm/agent-manager#360)",
		version, platformAdminsGroup, credentialStateUnverifiable, credentialsWriteOnlyFloor)
}

// lostRightsFixtureProblem is why the refusal after the binding's removal
// would not prove a lost right: an access review still lets the person
// create Secrets or ModelConfigs in the organization. "" once both are
// denied.
func lostRightsFixtureProblem(email, ns string, r organizationRights) string {
	if !r.createSecrets && !r.createModelConfigs {
		return ""
	}
	return fmt.Sprintf("fixture wrong: %s, removed from the RoleBinding %s/%s, may still create Secrets (%v) or ModelConfigs (%v) there: they did not lose the right, so a refusal would not be a lost right's", email, ns, organizationAdminsBinding, r.createSecrets, r.createModelConfigs)
}

// restAnswer is one answer of agent-manager's credential API: its status and
// body.
type restAnswer struct {
	code int
	body []byte
}

// errorMessage is the message of agent-manager's REST error body, "" for
// another body.
func errorMessage(raw []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return body.Error.Message
}

// lostRightsRefusal judges the save and the read of the person's own
// operation after they lost the right. Both must be agent-manager's 403
// forbidden: a 404 is the refusal of another person's operation, 412 a
// stale ETag, 409 an expired or changed operation, 200 a save accepted. The
// save's message is sanitized (credentialSaveError); the read, which passes
// the same ownership and rights check (credentialOperation), must name the
// denied create of this operation's ModelConfig or Secret in ns and the
// admin role it needs. It returns that denial.
func lostRightsRefusal(ns, model, secret string, save, read restAnswer) (string, error) {
	if save.code != http.StatusForbidden || errorCode(save.body) != codeForbidden {
		return "", fmt.Errorf("the save after the lost right answered %d %q, want 403 %s", save.code, errorCode(save.body), codeForbidden)
	}
	if read.code != http.StatusForbidden || errorCode(read.body) != codeForbidden {
		return "", fmt.Errorf("the read of their own operation after the lost right answered %d %q, want 403 %s", read.code, errorCode(read.body), codeForbidden)
	}
	message := errorMessage(read.body)
	if !strings.Contains(message, organizationAdminRole+" in organization namespace "+ns) {
		return "", fmt.Errorf("the read's refusal %q does not name %s in %s: an unrelated guard", excerpt(message, 300), organizationAdminRole, ns)
	}
	for _, denied := range []string{
		fmt.Sprintf("you may not create modelconfigs.%s/%s in %s;", kagentAPIGroup, model, ns),
		fmt.Sprintf("you may not create %s/%s in %s;", resourceSecrets, secret, ns),
	} {
		if strings.Contains(message, denied) {
			return strings.TrimSuffix(denied, ";"), nil
		}
	}
	return "", fmt.Errorf("the read's refusal %q denies no create of ModelConfig %s or Secret %s in %s: an unrelated guard", excerpt(message, 300), model, secret, ns)
}

// operationUnchanged checks an operation reads as it did before a refused
// save: pending at the same ETag, so the refusal changed nothing of it.
func operationUnchanged(beforeETag, afterState, afterETag string) error {
	if afterState != operationPending || afterETag != beforeETag {
		return fmt.Errorf("the operation reads %q at %s after the refused save, want %s at %s, as before it", afterState, afterETag, operationPending, beforeETag)
	}
	return nil
}

// adminsSubjects are the subjects of the organization's admins binding:
// platform-admins, and the other admin unless they are removed.
func (p *credentialsProof) adminsSubjects(withOther bool) []rbacv1.Subject {
	subjects := []rbacv1.Subject{groupSubject(platformAdminsGroup)}
	if withOther {
		subjects = append(subjects, userSubject(p.other))
	}
	return subjects
}

// setOtherAdmin binds the other admin as an admin of the organization again,
// or removes them, and records it for cleanup.
func (p *credentialsProof) setOtherAdmin(admin bool) error {
	if err := bindOrganizationRole(p.namespace, organizationAdminsBinding, organizationAdminRole, p.adminsSubjects(admin)); err != nil {
		return err
	}
	p.otherRemoved = !admin
	return nil
}

// restoreOtherAdmin gives the other admin their binding back after a run
// that removed it, best effort: a failed run leaves the lab usable.
func (p *credentialsProof) restoreOtherAdmin() {
	if !p.otherRemoved || p.namespace == "" {
		return
	}
	if err := p.setOtherAdmin(true); err != nil {
		note("cleanup: binding %s as %s in %s again: %v", p.other.Email, organizationAdminRole, p.namespace, err)
		return
	}
	note("cleanup: %s bound as %s in %s again", p.other.Email, organizationAdminRole, p.namespace)
}

// waitOtherRights reads the other admin's rights until problem says nothing
// is wrong with them or the authorizer had credentialsRightsSettle to see
// the binding's change, and returns what is still wrong.
func (p *credentialsProof) waitOtherRights(problem func(organizationRights) string) (string, error) {
	var why string
	var readErr error
	waitFor(int(credentialsRightsSettle/pollInterval), pollInterval, func() bool {
		var r organizationRights
		r, readErr = readOrganizationRights(p.otherToken, p.namespace)
		if readErr == nil {
			why = problem(r)
		}
		return readErr == nil && why == ""
	})
	return why, readErr
}

// lostRightsSaveRefused: the other admin, whose access review allows
// creating Secrets and ModelConfigs in the organization, prepares model
// credentialsLostModel without its key and reads the confirmation with its
// ETag. The lab then removes them from the organization's admins binding,
// and their access review must deny both creates. Their POST of the value
// with that ETag must be refused 403 forbidden and their read of the
// operation 403 with the denial of this operation's write in the
// organization's namespace; nothing is written.
func (p *credentialsProof) lostRightsSaveRefused() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), credentialsSaveTimeout)
	defer cancel()
	before, err := readOrganizationRights(p.otherToken, p.namespace)
	if err != nil {
		return "", err
	}
	if problem := otherAdminFixtureProblem(p.other.Email, p.namespace, before); problem != "" {
		return "", errors.New(problem)
	}
	other, err := dialMusterMCP(ctx, p.cfg, p.otherToken, musterMCPOptions{name: "agentlab-credentials-lost", protocol: mcp.ProtocolVersion20251125})
	if err != nil {
		return "", err
	}
	defer other.close()
	args := withArgs(p.modelArgs(), map[string]any{nameKey: credentialsLostModel, modeArg: modeApply})
	answer, result, err := setModelWith(ctx, other, args, mcp.MultiRoundTripParams{})
	if err != nil {
		return "", fmt.Errorf("%s's set_organization_model %s: %w", p.other.Email, credentialsLostModel, err)
	}
	if answer.result == nil {
		return "", fmt.Errorf("%s's client without URL elicitation was answered with %d elicitations", p.other.Email, len(answer.urlElicitations()))
	}
	if p.lostOperation, _, err = pendingCredential(*result, credentialsLostModelSecret, credentialsModelKey); err != nil {
		return "", fmt.Errorf("%s's set_organization_model %s: %w", p.other.Email, credentialsLostModel, err)
	}
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	c, etag, err := p.readConfirmation(ctx, rest, p.lostOperation, p.otherToken)
	if err != nil {
		return "", err
	}
	if c.State != operationPending {
		return "", fmt.Errorf("operation %s reads %q before the right is lost, want %s", p.lostOperation, c.State, operationPending)
	}
	p.lostETag = etag
	step("%s removed from the RoleBinding %s/%s (%s)", p.other.Email, p.namespace, organizationAdminsBinding, organizationAdminRole)
	if err := p.setOtherAdmin(false); err != nil {
		return "", err
	}
	why, err := p.waitOtherRights(func(r organizationRights) string {
		return lostRightsFixtureProblem(p.other.Email, p.namespace, r)
	})
	switch {
	case err != nil:
		return "", err
	case why != "":
		return "", errors.New(why)
	}
	body, err := credentialSaveBody(credentialsLostModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	resp, raw, err := rest.do(ctx, http.MethodPost, p.lostOperation, p.otherToken, http.Header{headerContentType: {mediaTypeJSON}, headerIfMatch: {etag}}, body)
	if err != nil {
		return "", err
	}
	save := restAnswer{code: resp.StatusCode, body: raw}
	if how := valueCarried(string(raw), p.value); how != "" {
		return "", fmt.Errorf("the refused save's answer carries the value %s", how)
	}
	code, readBody, _, err := rest.get(ctx, p.otherToken, p.lostOperation)
	if err != nil {
		return "", err
	}
	denied, err := lostRightsRefusal(p.namespace, credentialsLostModel, credentialsLostModelSecret, save, restAnswer{code: code, body: readBody})
	if err != nil {
		return "", fmt.Errorf("%s: %w (the save answered %s; the read %s)", p.other.Email, err, excerpt(redactValue(string(raw), p.value), 300), excerpt(string(readBody), 300))
	}
	if err := nothingWritten(ctx, p.namespace, credentialsLostModel, credentialsLostModelSecret); err != nil {
		return "", fmt.Errorf("after the save refused for the lost right: %w", err)
	}
	return fmt.Sprintf("%s (access review: create secrets and modelconfigs allowed) prepared operation %s at %s; removed from %s/%s, access review: both denied; POST with that ETag 403 %s %q, their GET 403 naming %q; no ModelConfig %s, no value in %s",
		p.other.Email, p.lostOperation, etag, p.namespace, organizationAdminsBinding, codeForbidden, excerpt(errorMessage(raw), 120), denied, credentialsLostModel, credentialsLostModelSecret), nil
}

// lostRightsRestoredSave: the other admin bound again, their access review
// allowing both creates, reads the operation the refused save left: pending
// at the same ETag. Their POST with that ETag then saves it: the receipt
// names the Secret present, the live Secret's SHA-256 matches and the
// ModelConfig exists. The refusal was the lost right alone.
func (p *credentialsProof) lostRightsRestoredSave() (string, error) {
	if p.lostOperation == "" {
		return "", errors.New("no operation refused for the lost right")
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialsSaveTimeout)
	defer cancel()
	step("%s bound again in the RoleBinding %s/%s (%s)", p.other.Email, p.namespace, organizationAdminsBinding, organizationAdminRole)
	if err := p.setOtherAdmin(true); err != nil {
		return "", err
	}
	why, err := p.waitOtherRights(func(r organizationRights) string {
		return otherAdminFixtureProblem(p.other.Email, p.namespace, r)
	})
	switch {
	case err != nil:
		return "", err
	case why != "":
		return "", errors.New(why)
	}
	rest, err := dialCredentialREST(ctx)
	if err != nil {
		return "", err
	}
	defer rest.stop()
	c, etag, err := p.readConfirmation(ctx, rest, p.lostOperation, p.otherToken)
	if err != nil {
		return "", err
	}
	if err := operationUnchanged(p.lostETag, c.State, etag); err != nil {
		return "", err
	}
	body, err := credentialSaveBody(credentialsLostModelSecret, credentialsModelKey, p.value)
	if err != nil {
		return "", err
	}
	resp, raw, err := rest.do(ctx, http.MethodPost, p.lostOperation, p.otherToken, http.Header{headerContentType: {mediaTypeJSON}, headerIfMatch: {etag}}, body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the save with the rights back answered %d, want 200: %s", resp.StatusCode, excerpt(redactValue(string(raw), p.value), 300))
	}
	if how := valueCarried(string(raw), p.value); how != "" {
		return "", fmt.Errorf("the receipt carries the value %s", how)
	}
	var receipt credentialAnswer
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return "", fmt.Errorf("the receipt: %w", err)
	}
	if err := receiptNames(receipt, p.lostOperation, credentialsLostModelSecret, credentialsModelKey, credentialStatePresent); err != nil {
		return "", fmt.Errorf("the receipt: %w", err)
	}
	sum, found, err := secretValueSum(ctx, p.namespace, credentialsLostModelSecret, credentialsModelKey)
	if err != nil {
		return "", err
	}
	if !found || sum != p.valueSum {
		return "", fmt.Errorf("the Secret %s/%s holds %s after the save, want %s", p.namespace, credentialsLostModelSecret, shortSum(sum), shortSum(p.valueSum))
	}
	if err := modelConfigPresent(ctx, p.namespace, credentialsLostModel, true); err != nil {
		return "", fmt.Errorf("after the save: %w", err)
	}
	return fmt.Sprintf("%s bound again (access review: both creates allowed): operation %s still %s at %s; POST with that ETag saved %s/%s (%s) as %s, the live Secret's SHA-256 matches and ModelConfig %s exists",
		p.other.Email, p.lostOperation, operationPending, etag, credentialsLostModelSecret, credentialsModelKey, shortSum(p.valueSum), credentialStatePresent, credentialsLostModel), nil
}
