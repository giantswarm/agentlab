package lab

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/yosida95/uritemplate/v3"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	sigsyaml "sigs.k8s.io/yaml"
)

// The pure decisions of the credentials part (organizationstest_credentials.go).

// containerFlags reads a container's `--name=value` arguments; a bare
// `--name` reads "true".
func containerFlags(args []string) map[string]string {
	flags := map[string]string{}
	for _, a := range args {
		name, ok := strings.CutPrefix(a, "--")
		if !ok || name == "" {
			continue
		}
		key, value, found := strings.Cut(name, "=")
		if key == "" {
			continue
		}
		if !found {
			value = strconv.FormatBool(true)
		}
		flags[key] = value
	}
	return flags
}

// containerArgs are the command and arguments of the container named name,
// or of the one container when none is named so.
func containerArgs(containers []corev1.Container, name string) ([]string, error) {
	if i := slices.IndexFunc(containers, func(c corev1.Container) bool { return c.Name == name }); i >= 0 {
		return append(slices.Clone(containers[i].Command), containers[i].Args...), nil
	}
	if len(containers) == 1 {
		return append(slices.Clone(containers[0].Command), containers[0].Args...), nil
	}
	return nil, fmt.Errorf("no container named %s among %d", name, len(containers))
}

// credentialPrerequisites is what the credentials part stands on, read from
// the live lab: agent-manager's flags, the person's GitHub grant, what muster
// speaks, the Flux controllers and agent-platform-operator's commit mode.
type credentialPrerequisites struct {
	// agentManager are the agent-manager Deployment's flags.
	agentManager map[string]string
	// operator are agent-platform-operator's flags.
	operator map[string]string
	// grant is "" while the person holds their grant of the workspaces'
	// github instance App, else why not.
	grant string
	// musterModern says muster answered an MCP 2026-07-28 client on that
	// revision.
	musterModern bool
	// outputSchema is the outputSchema muster's describe_tool lists for
	// set_organization_model, nil without one.
	outputSchema map[string]any
	// kustomizeController says a Ready kustomize-controller runs.
	kustomizeController bool
}

// The agent-manager and operator flags the part reads.
const (
	flagCredentialUIURL    = "credential-ui-url"              // #nosec G101 -- a flag name, not a credential
	flagTokenExchange      = "github-token-exchange-endpoint" // #nosec G101 -- a flag name, not a credential
	flagOrgsRepository     = "organizations-repository"
	flagOrgsBranch         = "organizations-branch"
	flagOrgsContentPrefix  = "organizations-content-prefix"
	flagFluxNamespace      = "flux-namespace"
	flagFluxSourceName     = "flux-source-name"
	flagFluxSourceKind     = "flux-source-kind"
	flagFluxDecryption     = "flux-decryption-secret"
	defaultOrgsBranch      = "main"
	defaultOrgsContentPath = "agent-platform/organizations"
	defaultFluxSourceKind  = "GitRepository"
	// modeApply is agent-manager's live write mode, modeCommit's twin.
	modeApply = "apply"
)

// credentialAgentManagerFloor is the first agent-manager that prepares
// credential operations and serves their REST API.
const credentialAgentManagerFloor = "2.22.0-rc.3" // #nosec G101 -- a version, not a credential

// uiURLSkip is why no credential operation can be prepared: agent-manager
// has no credential UI URL.
func (c credentialPrerequisites) uiURLSkip() string {
	if c.agentManager[flagCredentialUIURL] != "" {
		return ""
	}
	return "agent-manager runs without credentials.uiURL (Deployment " + platformNamespace + "/" + agentManagerMCPServer + " has no --" + flagCredentialUIURL +
		"): put agent-manager.credentials.uiURL, an https URL template with {operation}, in a platform.valuesFiles overlay on agent-manager " + credentialAgentManagerFloor + " or newer, then `agentlab platform`"
}

// commitSkip is why a save cannot open a pull request as the person: commit
// mode for organizations is not wired, or the person holds no grant.
func (c credentialPrerequisites) commitSkip() string {
	switch {
	case c.agentManager[flagTokenExchange] == "":
		return "agent-manager runs without githubTokenExchange (no --" + flagTokenExchange + "): commit mode needs platform.workspaces' github instance, muster keeping its App and agent-manager 2.22.0-rc.3 or newer (docs/agents.md \"Commit mode\"), then `agentlab platform`"
	case c.agentManager[flagOrgsRepository] == "":
		return "agent-manager offers no commit mode for organizations (no --" + flagOrgsRepository + "): put agent-manager.tenants.commit.repository (the first platform.workspaces.github.proofRepositories entry) and tenants.commit.objectsPath in a platform.valuesFiles overlay (agent-manager docs/organizations-commit-mode.md), then `agentlab platform`"
	case c.grant != "":
		return c.grant
	}
	return ""
}

// generationsSkip is why muster cannot answer the client generations:
// it speaks MCP 2025-11-25 only.
func (c credentialPrerequisites) generationsSkip() string {
	if c.musterModern {
		return ""
	}
	return "muster answered an MCP 2026-07-28 client on 2025-11-25: passing agent-manager's input requests through to each client generation is giantswarm/muster#1812; run a muster that carries it (platform.devImages.muster or its release), then `agentlab platform`"
}

// fluxSkip is why Flux cannot apply the organization's merged path: no
// kustomize-controller, or agent-platform-operator not in commit mode with a
// SOPS key.
func (c credentialPrerequisites) fluxSkip() string {
	switch {
	case !c.kustomizeController:
		return "no Ready kustomize-controller runs: the chart's bundled Flux engine runs flux-engine.instance.components (source-controller and helm-controller by default); add kustomize-controller there in a platform.valuesFiles overlay (its image must be in flux-engine.instance.distribution.registry), then `agentlab platform`"
	case c.operator[flagFluxNamespace] == "" || c.operator[flagFluxSourceName] == "":
		return "agent-platform-operator keeps no Kustomization per organization (no --" + flagFluxNamespace + " and --" + flagFluxSourceName + "): its commit mode applies <organization-path> from the installation's Flux source; set both in the operator's values, then `agentlab platform`"
	case c.operator[flagFluxDecryption] == "":
		return "agent-platform-operator's Kustomizations decrypt nothing (no --" + flagFluxDecryption + "): name the installation's SOPS key Secret in --" + flagFluxNamespace + " in the operator's values, then `agentlab platform`"
	}
	return ""
}

// orgsBranch and orgsContentPrefix are agent-manager's organizations
// repository branch and content prefix, defaults included.
func (c credentialPrerequisites) orgsBranch() string {
	return firstNonEmpty(c.agentManager[flagOrgsBranch], defaultOrgsBranch)
}

func (c credentialPrerequisites) orgsContentPrefix() string {
	return strings.Trim(firstNonEmpty(c.agentManager[flagOrgsContentPrefix], defaultOrgsContentPath), "/")
}

// credentialSecretPath is where agent-manager's commit mode writes an
// organization's credential Secret (agent-manager
// docs/organizations-commit-mode.md "The layout").
func credentialSecretPath(contentPrefix, parent, organization, secret string) string {
	return strings.Join([]string{strings.Trim(contentPrefix, "/"), parent, organization, secret + "-secret.enc.yaml"}, "/")
}

// strongETag reads an ETag header as RFC 9110 section 8.8.3 writes a strong
// one: a quoted opaque tag without the W/ weakness prefix. It returns the
// header as If-Match carries it.
func strongETag(header string) (string, error) {
	switch {
	case header == "":
		return "", errors.New("no ETag")
	case strings.HasPrefix(header, "W/"):
		return "", fmt.Errorf("ETag %s is weak", header)
	case len(header) < 3 || header[0] != '"' || header[len(header)-1] != '"' || strings.ContainsAny(header[1:len(header)-1], `" `):
		return "", fmt.Errorf("ETag %s is not a quoted entity tag", header)
	}
	return header, nil
}

// attributedTo says whether an elicitation's message is muster's
// attribution of it to the server key: one line, prefixed with the key in
// brackets (giantswarm/muster#1812).
func attributedTo(message, key string) bool {
	return !strings.ContainsAny(message, "\r\n") && strings.HasPrefix(message, "["+key+"] ")
}

// signInAttribution says how a sign-in elicitation's message names the
// server it signs in to: the bracketed key muster puts on a backend's
// elicitation, or the key in muster's own sign-in sentence. "" names it
// neither way.
func signInAttribution(message, key string) string {
	switch {
	case attributedTo(message, key):
		return "prefixed [" + key + "]"
	case strings.HasPrefix(message, "Sign in to "+key+" to continue"):
		return "muster's sign-in sentence naming " + key
	}
	return ""
}

// credentialAnswer is the part of agent-manager's ResourceResult the proof
// reads: the credentials and the pending operation, or the receipt.
type credentialAnswer struct {
	Namespace           string            `json:"namespace"`
	Kind                string            `json:"kind"`
	Name                string            `json:"name"`
	Credentials         []credentialEntry `json:"credentials"`
	CredentialOperation string            `json:"credentialOperation"`
	Pending             bool              `json:"pending"`
	DryRun              bool              `json:"dryRun"`
	Commit              *struct {
		Repository  string `json:"repository"`
		Base        string `json:"base"`
		Branch      string `json:"branch"`
		PullRequest string `json:"pullRequest"`
		Number      int    `json:"number"`
		Author      string `json:"author"`
	} `json:"commit"`
}

// credentialEntry is one credential of agent-manager's ResourceResult: its
// Secret and key, its state and, while missing, the form's link.
type credentialEntry struct {
	Secret  string `json:"secret"`
	Key     string `json:"key"`
	State   string `json:"state"`
	FormURL string `json:"formUrl"`
	Next    string `json:"next"`
}

// pendingCredential checks an answer is a pending credential operation for
// the one missing key of secret, its form linked from credentials[].next,
// and returns the operation and the link.
func pendingCredential(a credentialAnswer, secret, key string) (operation, formURL string, err error) {
	if !a.Pending || a.CredentialOperation == "" {
		return "", "", fmt.Errorf("the answer names no pending credential operation (pending=%v, credentialOperation=%q)", a.Pending, a.CredentialOperation)
	}
	i := slices.IndexFunc(a.Credentials, func(c credentialEntry) bool { return c.Secret == secret && c.Key == key })
	if i < 0 {
		return "", "", fmt.Errorf("the answer's credentials do not name %s/%s", secret, key)
	}
	c := a.Credentials[i]
	switch {
	case c.State != "missing":
		return "", "", fmt.Errorf("credential %s/%s is %q, want missing", secret, key, c.State)
	case c.FormURL == "":
		return "", "", fmt.Errorf("credential %s/%s has no formUrl", secret, key)
	case !strings.Contains(c.Next, c.FormURL):
		return "", "", fmt.Errorf("credentials[].next of %s/%s does not carry its formUrl: %q", secret, key, excerpt(c.Next, 200))
	}
	return a.CredentialOperation, c.FormURL, nil
}

// credentialSaveBody is the save's body as agent-manager's credential API
// takes it: exactly the requested Secret/key pair.
func credentialSaveBody(secret, key, value string) ([]byte, error) {
	type pair struct {
		Secret string `json:"secret"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	return json.Marshal(struct {
		Values []pair `json:"values"`
	}{Values: []pair{{Secret: secret, Key: key, Value: value}}})
}

// sopsValueRe matches a value SOPS encrypted in place.
var sopsValueRe = regexp.MustCompile(`ENC\[AES256_GCM,data:[^,\]]+,iv:[^,\]]+,tag:[^,\]]+,type:[a-z]+\]`)

// sopsEncryptedPatch checks one file's diff as GitHub shows it: SOPS
// metadata and an encrypted value, and neither the value nor its base64
// anywhere. The error never names the value.
func sopsEncryptedPatch(patch, value string) error {
	if err := valueInPatch(patch, value); err != nil {
		return err
	}
	switch {
	case !strings.Contains(patch, "sops:"):
		return errors.New("the diff carries no sops metadata")
	case !sopsValueRe.MatchString(patch):
		return errors.New("the diff carries no SOPS-encrypted value")
	}
	return nil
}

// valueInPatch fails a diff that carries the value, plain or base64; the
// error never names the value.
func valueInPatch(patch, value string) error {
	if how := valueCarried(patch, value); how != "" {
		return errors.New("the diff carries the value " + how)
	}
	return nil
}

// valueCarried says how s carries the value: "in plaintext",
// "base64-encoded", or "" when it does not.
func valueCarried(s, value string) string {
	switch {
	case value == "":
		return ""
	case strings.Contains(s, value):
		return "in plaintext"
	case strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(value))):
		return "base64-encoded"
	}
	return ""
}

// The variables of agent-manager's credential UI URL template.
const (
	uiVarOperation    = "operation"
	uiVarParent       = "parent"
	uiVarOrganization = "organization"
)

// schemeHTTPS is the scheme MCP's safe URL handling has a URL elicitation
// use outside development.
const schemeHTTPS = "https"

// formOperationMarker stands in for the operation while the credential UI
// URL template is expanded: unreserved characters, kept as is by RFC 6570.
const formOperationMarker = "agentlabformoperation"

// operationRe is an operation as a link may carry it: unreserved
// characters only (RFC 3986 section 2.3).
var operationRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// formOperation reads the operation from a link to the credential UI: the
// link must be the template expanded as agent-manager expands it (RFC 6570,
// with operation, parent and organization only), so nothing else, no value
// and no token, travels in it.
func formOperation(template, link, parent, organization string) (string, error) {
	tpl, err := uritemplate.New(template)
	if err != nil {
		return "", fmt.Errorf("credential UI URL template %q: %w", template, err)
	}
	expanded, err := tpl.Expand(uritemplate.Values{
		uiVarOperation:    uritemplate.String(formOperationMarker),
		uiVarParent:       uritemplate.String(parent),
		uiVarOrganization: uritemplate.String(organization),
	})
	if err != nil {
		return "", fmt.Errorf("credential UI URL template %q: %w", template, err)
	}
	prefix, suffix, ok := strings.Cut(expanded, formOperationMarker)
	if !ok || strings.Contains(suffix, formOperationMarker) {
		return "", fmt.Errorf("credential UI URL template %q does not expand {operation} once", template)
	}
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, suffix) || len(link) < len(prefix)+len(suffix) {
		return "", fmt.Errorf("the link %q is not the credential UI's %s", link, strings.Replace(expanded, formOperationMarker, "{operation}", 1))
	}
	if u, err := url.Parse(link); err != nil || u.Scheme != schemeHTTPS {
		return "", fmt.Errorf("the link %q is not an https URL, as MCP's safe URL handling has servers send", link)
	}
	operation := link[len(prefix) : len(link)-len(suffix)]
	if !operationRe.MatchString(operation) {
		return "", fmt.Errorf("the link %q carries %q where the operation goes", link, operation)
	}
	return operation, nil
}

// conformsTo validates a tool's structuredContent against its declared
// outputSchema, as MCP's tools page has clients do (2025-11-25
// server/tools, "Output Schema"), with the JSON Schema dialect the schema
// names (2020-12 by default).
func conformsTo(schema map[string]any, structured json.RawMessage) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("the outputSchema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("the outputSchema: %w", err)
	}
	var instance any
	if err := json.Unmarshal(structured, &instance); err != nil {
		return err
	}
	return resolved.Validate(instance)
}

// sameReceipt checks a retried call's receipt is the save's: the same
// pull request on the same branch, or neither carrying one.
func sameReceipt(saved, retried credentialAnswer) error {
	switch {
	case saved.Commit == nil && retried.Commit == nil:
		return nil
	case saved.Commit == nil || retried.Commit == nil:
		return fmt.Errorf("the save's commit %+v and the retry's %+v differ", saved.Commit, retried.Commit)
	case saved.Commit.Number != retried.Commit.Number || saved.Commit.Branch != retried.Commit.Branch:
		return fmt.Errorf("the retry names pull request #%d on %q, the save #%d on %q", retried.Commit.Number, retried.Commit.Branch, saved.Commit.Number, saved.Commit.Branch)
	}
	return nil
}

// redactedValue stands in for the typed value in an answer an error quotes.
const redactedValue = "<redacted>"

// redactValue replaces the value and its base64 in s.
func redactValue(s, value string) string {
	if value == "" {
		return s
	}
	s = strings.ReplaceAll(s, value, redactedValue)
	return strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(value)), redactedValue)
}

// receiptNames checks a credential write's answer is the receipt of
// operation: not pending, no other operation, and secret/key in state want.
func receiptNames(a credentialAnswer, operation, secret, key, want string) error {
	switch {
	case a.Pending:
		return errors.New("still pending")
	case a.CredentialOperation != "" && a.CredentialOperation != operation:
		return fmt.Errorf("names operation %q, want %s", a.CredentialOperation, operation)
	}
	for _, c := range a.Credentials {
		if c.Secret == secret && c.Key == key {
			if c.State != want {
				return fmt.Errorf("credential %s/%s is %q, want %s", secret, key, c.State, want)
			}
			return nil
		}
	}
	return fmt.Errorf("the credentials %+v do not name %s/%s", a.Credentials, secret, key)
}

// sopsConfig is the part of a repository's .sops.yaml the proof reads.
type sopsConfig struct {
	CreationRules []struct {
		PathRegex      string `json:"path_regex"`
		EncryptedRegex string `json:"encrypted_regex"`
		Age            string `json:"age"`
	} `json:"creation_rules"`
}

// sopsRuleFor checks a .sops.yaml as SOPS picks its rule: the first creation
// rule whose path_regex matches path must encrypt data and stringData for
// recipient among its age recipients.
func sopsRuleFor(file []byte, path, recipient string) error {
	var cfg sopsConfig
	if err := sigsyaml.Unmarshal(file, &cfg); err != nil {
		return fmt.Errorf(".sops.yaml: %w", err)
	}
	for _, rule := range cfg.CreationRules {
		re, err := regexp.Compile(rule.PathRegex)
		if err != nil {
			return fmt.Errorf(".sops.yaml path_regex %q: %w", rule.PathRegex, err)
		}
		if !re.MatchString(path) {
			continue
		}
		recipients := strings.FieldsFunc(rule.Age, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
		if !slices.Contains(recipients, recipient) {
			return fmt.Errorf(".sops.yaml's rule %q for %s does not encrypt for the lab's age recipient %s", rule.PathRegex, path, recipient)
		}
		encrypted, err := regexp.Compile(rule.EncryptedRegex)
		if err != nil || !encrypted.MatchString("data") || !encrypted.MatchString("stringData") || encrypted.MatchString("metadata") {
			return fmt.Errorf(".sops.yaml's rule %q encrypted_regex %q must cover data and stringData and leave the Secret's kind and name readable", rule.PathRegex, rule.EncryptedRegex)
		}
		return nil
	}
	return fmt.Errorf(".sops.yaml has no creation rule for %s", path)
}

// sopsRuleHint is the creation rule the repository's .sops.yaml needs for
// the lab's key, as agent-manager docs/organizations-commit-mode.md writes it.
func sopsRuleHint(contentPrefix, recipient string) string {
	return fmt.Sprintf("creation_rules:\n  - path_regex: ^%s/.*-secret\\.enc\\.yaml$\n    encrypted_regex: ^(data|stringData)$\n    age: %s", regexp.QuoteMeta(strings.Trim(contentPrefix, "/")), recipient)
}

// organizationRef names one organization for the erasure's leftovers.
type organizationRef struct {
	parent, name, namespace, uid string
}

// The operator's labels on what it keeps for an organization.
const organizationUIDLabel = "agent-platform.giantswarm.io/organization-uid"

// organizationBindingLeftovers names the RoleBindings that still refer to
// the erased organization: one in its namespace, one carrying its UID or
// its name and parent, or one binding a ServiceAccount of its namespace or
// named after it (its Flux identity).
func organizationBindingLeftovers(bindings []rbacv1.RoleBinding, org organizationRef) []string {
	var out []string
	for _, b := range bindings {
		refers := b.Namespace == org.namespace ||
			(org.uid != "" && b.Labels[organizationUIDLabel] == org.uid) ||
			(b.Labels[organizationLabel] == org.name && b.Labels[organizationParentLabel] == org.parent)
		for _, s := range b.Subjects {
			if s.Kind == rbacv1.ServiceAccountKind && (s.Namespace == org.namespace || s.Name == org.namespace) {
				refers = true
			}
		}
		if refers {
			out = append(out, b.Namespace+"/"+b.Name)
		}
	}
	slices.Sort(out)
	return out
}

// credentialReference is one object a confirmation lists as naming a
// requested Secret (agent-manager's CredentialReference): for a
// RemoteMCPServer the URL it sends the value to, for an EgressPolicy the
// origins.
type credentialReference struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Key     string   `json:"key"`
	URL     string   `json:"url,omitempty"`
	Origins []string `json:"origins,omitempty"`
}

// credentialChange is one write a confirmation shows before a value is
// accepted.
type credentialChange struct {
	Action string         `json:"action"`
	Object map[string]any `json:"object"`
}

// confirmationListsOnly checks a confirmation shows one write, of the object
// kind/name, and lists exactly want as the objects naming the requested
// Secret: no other consumer, no other key, and for an EgressPolicy the
// origins it sends the credential to and no other (in any order).
func confirmationListsOnly(c confirmation, kind, name string, want []credentialReference) error {
	if len(c.Changes) != 1 {
		return fmt.Errorf("the confirmation shows %d writes, want the one of %s %s", len(c.Changes), kind, name)
	}
	obj := c.Changes[0].Object
	objKind, _ := obj[kindKey].(string)
	meta, _ := obj[crMetadata].(map[string]any)
	objName, _ := meta[nameKey].(string)
	if objKind != kind || objName != name {
		return fmt.Errorf("the confirmation's write is %s %s, want %s %s", objKind, objName, kind, name)
	}
	normal := func(refs []credentialReference) []string {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			origins := slices.Clone(r.Origins)
			slices.Sort(origins)
			out = append(out, fmt.Sprintf("%s/%s key=%s url=%s origins=%v", r.Kind, r.Name, r.Key, r.URL, origins))
		}
		slices.Sort(out)
		return out
	}
	if got, w := normal(c.References), normal(want); !slices.Equal(got, w) {
		return fmt.Errorf("the confirmation lists references %v, want %v", got, w)
	}
	return nil
}

// ifMatchRequiredFloor is the first agent-manager that answers a save or a
// cancel without If-Match 428 Precondition Required (RFC 6585 section 3,
// giantswarm/agent-manager#354, from 2.23.0-rc.10); before it, 400.
const ifMatchRequiredFloor = "2.23.0"

// ifMatchRequiredStatuses are the statuses a save without If-Match may get
// from the agent-manager version the lab runs, and a note when it predates
// the floor.
func ifMatchRequiredStatuses(version string) ([]int, string) {
	if atLeast(version, ifMatchRequiredFloor) {
		return []int{http.StatusPreconditionRequired}, ""
	}
	return []int{http.StatusPreconditionRequired, http.StatusBadRequest},
		fmt.Sprintf(" (agent-manager %q predates %s, which answers 428 for a missing If-Match (giantswarm/agent-manager#354): 400 accepted)", version, ifMatchRequiredFloor)
}

// credentialStateUnverifiable is agent-manager's refusal of a person's own
// credential write when it cannot read the credential Secret as them.
const credentialStateUnverifiable = "credential state cannot be verified" // #nosec G101 -- an error text, not a credential

// credentialsWriteOnlyFloor is the first agent-manager that never reads a
// credential Secret as the caller (giantswarm/agent-manager#360, closing
// #359): an organization admin outside platform-admins prepares and saves
// credentials, and a person without the rights is refused by the
// authorization of the write itself.
const credentialsWriteOnlyFloor = "2.24.0-rc.1" // #nosec G101 -- a version, not a credential

// atLeast says whether version is floor or newer; an unreadable version is
// not.
func atLeast(version, floor string) bool {
	v, err := semver.NewVersion(version)
	return err == nil && !v.LessThan(semver.MustParse(floor))
}

// ownCredentialRefused is the SKIP reason when an agent-manager older than
// credentialsWriteOnlyFloor refused a person's own set_organization_model
// because it reads the credential Secret as the caller, which
// agent-platform:admin may not. "" for any other answer, and for any answer
// of an agent-manager at or above the floor, where that refusal is a
// failure.
func ownCredentialRefused(email, version string, err error) string {
	if err == nil || atLeast(version, credentialsWriteOnlyFloor) || !strings.Contains(err.Error(), credentialStateUnverifiable) {
		return ""
	}
	return fmt.Sprintf("agent-manager %s refuses %s's own set_organization_model (forbidden: %s): it reads the credential Secret as the caller, which agent-platform:admin may not; agent-manager %s or newer writes credentials without reading them (giantswarm/agent-manager#360, closing #359)",
		version, email, credentialStateUnverifiable, credentialsWriteOnlyFloor)
}

// viewerFixtureProblem is why the viewer's verdict cannot stand on its
// person: an access review allows them to create Secrets or ModelConfigs in
// the organization, so a refusal would not be their missing right, or
// denies them the read of its ModelConfigs, which agent-manager does before
// the write. "" for a viewer that reads and does not write.
func viewerFixtureProblem(email, ns string, r organizationRights) string {
	switch {
	case r.createSecrets || r.createModelConfigs:
		return fmt.Sprintf("fixture wrong: %s, the part's viewer, may create Secrets (%v) or ModelConfigs (%v) in %s: they hold write rights there, so a refusal would not be a viewer's", email, r.createSecrets, r.createModelConfigs, ns)
	case !r.listModelConfigs:
		return fmt.Sprintf("fixture wrong: %s, the part's viewer, may not list ModelConfigs in %s: without the read %s grants, agent-manager refuses the read before the write", email, ns, organizationUserRole)
	}
	return ""
}

// otherAdminFixtureProblem is why the other admin's verdicts cannot stand on
// their person: an access review denies them creating Secrets or
// ModelConfigs in the organization, so they are no organization admin. ""
// when both are allowed.
func otherAdminFixtureProblem(email, ns string, r organizationRights) string {
	if r.createSecrets && r.createModelConfigs {
		return ""
	}
	return fmt.Sprintf("fixture wrong: %s, bound %s in %s, may not create Secrets or ModelConfigs there: they are no organization admin, so their verdicts would prove nothing of one", email, organizationAdminRole, ns)
}

// viewerRefusalVerdict judges agent-manager's refusal of the viewer's
// set_organization_model: it passes, naming the denied write, only when it
// carries the apiserver's RBAC denial of that user creating the Secret or
// the ModelConfig. An agent-manager older than credentialsWriteOnlyFloor
// refuses on reading the Secret first, for every person outside
// platform-admins: a skip naming the release. Any other refusal is an
// unrelated guard and fails.
func viewerRefusalVerdict(version, refusal, user string) (denied, skip string, err error) {
	for _, resource := range []string{resourceSecrets, "modelconfigs"} {
		if strings.Contains(refusal, fmt.Sprintf(`User %q cannot create resource %q`, user, resource)) {
			return "create " + resource, "", nil
		}
	}
	if strings.Contains(refusal, credentialStateUnverifiable) && !atLeast(version, credentialsWriteOnlyFloor) {
		return "", fmt.Sprintf("agent-manager %s refuses the viewer on reading the credential Secret (forbidden: %s), as it refuses every person outside platform-admins, before the write's authorization: the viewer's own refusal needs agent-manager %s or newer (giantswarm/agent-manager#360)", version, credentialStateUnverifiable, credentialsWriteOnlyFloor), nil
	}
	return "", "", fmt.Errorf("agent-manager %s refused the viewer %q, not with the apiserver's denial of %s creating secrets or modelconfigs: an unrelated guard", version, excerpt(refusal, 300), user)
}

// judgeComponentLogs checks one component's logs carry the value neither
// plain nor base64 and returns how many lines were read. No line read is a
// failure: an empty read proves nothing about what the component logs.
func judgeComponentLogs(component, logs, value string) (int, error) {
	lines := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines == 0 {
		return 0, fmt.Errorf("read no log line of %s/%s since the run began: an empty read proves nothing about what it logs", platformNamespace, component)
	}
	if how := valueCarried(logs, value); how != "" {
		return lines, fmt.Errorf("the logs of %s/%s carry the value %s", platformNamespace, component, how)
	}
	return lines, nil
}

// refusedWith checks a tool's answer is agent-manager's error result with
// code ("forbidden", "conflict"): the refusal of the write itself, not
// muster's (an unknown tool) nor a pending operation. It returns the
// result's text.
func refusedWith(a *musterAnswer, code string) (string, error) {
	switch {
	case a == nil:
		return "", errors.New("no answer")
	case a.result == nil:
		return "", fmt.Errorf("answered with %d URL elicitations, not refused", len(a.urlElicitations()))
	case !a.result.IsError:
		return "", fmt.Errorf("answered a result, not refused: %s", excerpt(a.result.text(), 200))
	}
	text := a.result.text()
	if !strings.Contains(text, code+": ") {
		return "", fmt.Errorf("refused with %q, not agent-manager's %s", excerpt(text, 200), code)
	}
	return text, nil
}
