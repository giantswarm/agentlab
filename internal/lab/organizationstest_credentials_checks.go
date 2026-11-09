package lab

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

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
	outputSchema bool
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

// uiURLPrefix is the fixed part of the credential UI URL template, up to its
// first expression: every elicited link starts with it.
func uiURLPrefix(template string) string {
	prefix, _, _ := strings.Cut(template, "{")
	return prefix
}

// credentialAnswer is the part of agent-manager's ResourceResult the proof
// reads: the credentials and the pending operation, or the receipt.
type credentialAnswer struct {
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Credentials []struct {
		Secret  string `json:"secret"`
		Key     string `json:"key"`
		State   string `json:"state"`
		FormURL string `json:"formUrl"`
		Next    string `json:"next"`
	} `json:"credentials"`
	CredentialOperation string `json:"credentialOperation"`
	Pending             bool   `json:"pending"`
	DryRun              bool   `json:"dryRun"`
	Commit              *struct {
		Repository  string `json:"repository"`
		Base        string `json:"base"`
		Branch      string `json:"branch"`
		PullRequest string `json:"pullRequest"`
		Number      int    `json:"number"`
		Author      string `json:"author"`
		Files       []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
		} `json:"files"`
	} `json:"commit"`
}

// pendingCredential checks an answer is a pending credential operation for
// the one missing key of secret, its form linked from credentials[].next,
// and returns the operation and the link.
func pendingCredential(a credentialAnswer, secret, key string) (operation, formURL string, err error) {
	if !a.Pending || a.CredentialOperation == "" {
		return "", "", fmt.Errorf("the answer names no pending credential operation (pending=%v, credentialOperation=%q)", a.Pending, a.CredentialOperation)
	}
	i := slices.IndexFunc(a.Credentials, func(c struct {
		Secret  string `json:"secret"`
		Key     string `json:"key"`
		State   string `json:"state"`
		FormURL string `json:"formUrl"`
		Next    string `json:"next"`
	}) bool {
		return c.Secret == secret && c.Key == key
	})
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
	switch {
	case value == "":
		return nil
	case strings.Contains(patch, value):
		return errors.New("the diff carries the value in plaintext")
	case strings.Contains(patch, base64.StdEncoding.EncodeToString([]byte(value))):
		return errors.New("the diff carries the value base64-encoded")
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
