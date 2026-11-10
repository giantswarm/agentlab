package lab

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestContainerFlags(t *testing.T) {
	flags := containerFlags([]string{"--listen=:8080", "--multi-tenant", "--credential-ui-url=https://p.example/c?operation={operation}&x=1", "serve", "-v", "--=x"})
	require.Equal(t, map[string]string{ // #nosec G101 -- flag fixtures, not credentials
		"listen":            ":8080",
		"multi-tenant":      strconv.FormatBool(true),
		"credential-ui-url": "https://p.example/c?operation={operation}&x=1",
	}, flags)
}

func TestCredentialPrerequisites(t *testing.T) {
	complete := credentialPrerequisites{
		agentManager: map[string]string{flagCredentialUIURL: "https://p.example/{operation}", flagTokenExchange: "https://m.example/oauth/token", flagOrgsRepository: "giantswarm/proof"}, // #nosec G101 -- flag fixtures, not credentials
		operator:     map[string]string{flagFluxNamespace: "flux-ns", flagFluxSourceName: "installation", flagFluxDecryption: "sops-age"},
		musterModern: true, kustomizeController: true,
	}
	require.Empty(t, complete.uiURLSkip())
	require.Empty(t, complete.commitSkip())
	require.Empty(t, complete.generationsSkip())
	require.Empty(t, complete.fluxSkip())
	require.Equal(t, defaultOrgsBranch, complete.orgsBranch())
	require.Equal(t, defaultOrgsContentPath, complete.orgsContentPrefix())

	for _, tc := range []struct {
		name   string
		change func(*credentialPrerequisites)
		skip   func(credentialPrerequisites) string
		want   string
	}{
		{"no ui url", func(c *credentialPrerequisites) { delete(c.agentManager, flagCredentialUIURL) }, credentialPrerequisites.uiURLSkip, "agent-manager.credentials.uiURL"},
		{"no token exchange", func(c *credentialPrerequisites) { delete(c.agentManager, flagTokenExchange) }, credentialPrerequisites.commitSkip, "githubTokenExchange"},
		{"no organizations repository", func(c *credentialPrerequisites) { delete(c.agentManager, flagOrgsRepository) }, credentialPrerequisites.commitSkip, "tenants.commit.repository"},
		{"no grant", func(c *credentialPrerequisites) { c.grant = "admin has not granted" }, credentialPrerequisites.commitSkip, "admin has not granted"},
		{"muster on 2025-11-25", func(c *credentialPrerequisites) { c.musterModern = false }, credentialPrerequisites.generationsSkip, "giantswarm/muster#1812"},
		{"no kustomize-controller", func(c *credentialPrerequisites) { c.kustomizeController = false }, credentialPrerequisites.fluxSkip, "flux-engine.instance.components"},
		{"operator not in commit mode", func(c *credentialPrerequisites) { delete(c.operator, flagFluxSourceName) }, credentialPrerequisites.fluxSkip, "--flux-source-name"},
		{"no decryption secret", func(c *credentialPrerequisites) { delete(c.operator, flagFluxDecryption) }, credentialPrerequisites.fluxSkip, "--flux-decryption-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := complete
			c.agentManager = map[string]string{}
			for k, v := range complete.agentManager {
				c.agentManager[k] = v
			}
			c.operator = map[string]string{}
			for k, v := range complete.operator {
				c.operator[k] = v
			}
			tc.change(&c)
			require.Contains(t, tc.skip(c), tc.want)
		})
	}

	custom := credentialPrerequisites{agentManager: map[string]string{flagOrgsBranch: "trunk", flagOrgsContentPrefix: "/orgs/"}}
	require.Equal(t, "trunk", custom.orgsBranch())
	require.Equal(t, "orgs", custom.orgsContentPrefix())
}

func TestCredentialSecretPath(t *testing.T) {
	require.Equal(t, "agent-platform/organizations/org-acme/payments/tenant-credential-x-secret.enc.yaml",
		credentialSecretPath("/agent-platform/organizations/", "org-acme", "payments", "tenant-credential-x"))
}

func TestStrongETag(t *testing.T) {
	got, err := strongETag(`"abc123"`)
	require.NoError(t, err)
	require.Equal(t, `"abc123"`, got)
	for _, bad := range []string{"", `W/"abc"`, "abc", `""`, `"a"b"`} {
		_, err := strongETag(bad)
		require.Error(t, err, bad)
	}
}

func TestAttribution(t *testing.T) {
	require.True(t, attributedTo("[agent-manager] Enter the value.", "agent-manager"))
	require.False(t, attributedTo("[agent-manager] Enter\nthe value.", "agent-manager"))
	require.False(t, attributedTo("Enter the value. [agent-manager]", "agent-manager"))
	require.False(t, attributedTo("[agent-manager-x] Enter", "agent-manager"))

	key := "agents-x-abc/srv"
	require.Equal(t, "prefixed ["+key+"]", signInAttribution("["+key+"] Sign in to continue.", key))
	require.Equal(t, "muster's sign-in sentence naming "+key, signInAttribution("Sign in to "+key+" to continue.", key))
	require.Empty(t, signInAttribution("Sign in to agents-x-abc/other to continue.", key))
}

func TestPendingCredential(t *testing.T) {
	var a credentialAnswer
	require.NoError(t, json.Unmarshal([]byte(`{"pending":true,"credentialOperation":"op1","credentials":[
		{"secret":"tenant-credential-m","key":"apiKey","state":"missing","formUrl":"https://p.example/c?operation=op1","next":"Enter the credential in the authenticated credential UI: https://p.example/c?operation=op1 . Never send the value to a tool or a chat."}]}`), &a))
	op, link, err := pendingCredential(a, "tenant-credential-m", "apiKey")
	require.NoError(t, err)
	require.Equal(t, "op1", op)
	require.Equal(t, "https://p.example/c?operation=op1", link)

	_, _, err = pendingCredential(a, "tenant-credential-other", "apiKey")
	require.ErrorContains(t, err, "do not name")

	a.Credentials[0].Next = "another page"
	_, _, err = pendingCredential(a, "tenant-credential-m", "apiKey")
	require.ErrorContains(t, err, "does not carry its formUrl")

	a.Credentials[0].State = "present"
	_, _, err = pendingCredential(a, "tenant-credential-m", "apiKey")
	require.ErrorContains(t, err, "want missing")

	a.Pending = false
	_, _, err = pendingCredential(a, "tenant-credential-m", "apiKey")
	require.ErrorContains(t, err, "no pending credential operation")
}

func TestCredentialSaveBody(t *testing.T) {
	body, err := credentialSaveBody("tenant-credential-m", "apiKey", "v")
	require.NoError(t, err)
	require.JSONEq(t, `{"values":[{"secret":"tenant-credential-m","key":"apiKey","value":"v"}]}`, string(body))
}

func TestSOPSEncryptedPatch(t *testing.T) {
	value := "s3cr3t-value-0123456789"
	encrypted := `+apiVersion: v1
+kind: Secret
+metadata:
+    name: tenant-credential-m
+data:
+    apiKey: ENC[AES256_GCM,data:Zm9vYmFy,iv:aXY=,tag:dGFn,type:str]
+sops:
+    age:
+        - recipient: age1abc`
	require.NoError(t, sopsEncryptedPatch(encrypted, value))
	require.ErrorContains(t, sopsEncryptedPatch(encrypted+"\n+# "+value, value), "plaintext")
	leaked := sopsEncryptedPatch(encrypted+"\n+# "+base64.StdEncoding.EncodeToString([]byte(value)), value)
	require.ErrorContains(t, leaked, "base64")
	require.NotContains(t, leaked.Error(), value)
	require.ErrorContains(t, sopsEncryptedPatch("+data:\n+    apiKey: Zm9v", value), "no sops metadata")
	require.ErrorContains(t, sopsEncryptedPatch("+data:\n+    apiKey: Zm9v\n+sops:\n", value), "no SOPS-encrypted value")
	require.NoError(t, valueInPatch("+kind: ModelConfig", value))
}

func TestSOPSRuleFor(t *testing.T) {
	recipient := "age1labrecipient"
	path := credentialSecretPath(defaultOrgsContentPath, "agentlab-organizations", "agentlab-credentials", credentialsModelSecret)
	require.NoError(t, sopsRuleFor([]byte(sopsRuleHint(defaultOrgsContentPath, recipient)), path, recipient))

	require.ErrorContains(t, sopsRuleFor([]byte(sopsRuleHint(defaultOrgsContentPath, "age1other")), path, recipient), "does not encrypt for the lab's age recipient")
	require.ErrorContains(t, sopsRuleFor([]byte(sopsRuleHint("elsewhere", recipient)), path, recipient), "no creation rule")
	everything := "creation_rules:\n  - path_regex: .*\n    encrypted_regex: .*\n    age: " + recipient
	require.ErrorContains(t, sopsRuleFor([]byte(everything), path, recipient), "leave the Secret's kind and name readable")
	two := "creation_rules:\n  - path_regex: ^agent-platform/.*\n    encrypted_regex: ^(data|stringData)$\n    age: age1first\n  - path_regex: .*\n    encrypted_regex: ^(data|stringData)$\n    age: " + recipient
	require.ErrorContains(t, sopsRuleFor([]byte(two), path, recipient), "does not encrypt for", "SOPS takes the first matching rule")
	several := "creation_rules:\n  - path_regex: ^agent-platform/organizations/.*-secret\\.enc\\.yaml$\n    encrypted_regex: ^(data|stringData)$\n    age: age1a, " + recipient
	require.NoError(t, sopsRuleFor([]byte(several), path, recipient))
}

func TestOrganizationBindingLeftovers(t *testing.T) {
	org := organizationRef{parent: "agentlab-organizations", name: "agentlab-credentials", namespace: "agents-agentlab-credentials-x7k2q", uid: "uid-1"}
	binding := func(ns, name string, labels map[string]string, subjects ...rbacv1.Subject) rbacv1.RoleBinding {
		return rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}, Subjects: subjects}
	}
	bindings := []rbacv1.RoleBinding{
		binding("kagent", "unrelated", nil, rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "oidc:developers"}),
		binding(org.namespace, "organization-admins", nil),
		binding("flux-ns", "by-uid", map[string]string{organizationUIDLabel: "uid-1"}),
		binding("flux-ns", "other-uid", map[string]string{organizationUIDLabel: "uid-2"}),
		binding("agentlab-organizations", "by-name", map[string]string{organizationLabel: org.name, organizationParentLabel: org.parent}),
		binding("agentlab-organizations", "same-name-other-parent", map[string]string{organizationLabel: org.name, organizationParentLabel: "org-other"}),
		binding("kagent", "sa-of-namespace", nil, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: org.namespace, Name: "builder"}),
		binding("flux-ns", "flux-identity", nil, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: "flux-ns", Name: org.namespace}),
	}
	require.Equal(t, []string{
		"agentlab-organizations/by-name",
		org.namespace + "/organization-admins",
		"flux-ns/by-uid",
		"flux-ns/flux-identity",
		"kagent/sa-of-namespace",
	}, organizationBindingLeftovers(bindings, org))
	require.Empty(t, organizationBindingLeftovers(bindings[:1], org))
}

func TestParseAgeIdentityTakesAgeKeygenFiles(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	file := "# created: 2026-11-09T14:00:00Z\n# public key: " + identity.Recipient().String() + "\n" + identity.String() + "\n"
	parsed, err := parseAgeIdentity([]byte(file))
	require.NoError(t, err)
	require.Equal(t, identity.Recipient().String(), parsed.Recipient().String())

	other, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	_, err = parseAgeIdentity([]byte(identity.String() + "\n" + other.String() + "\n"))
	require.Error(t, err)
	_, err = parseAgeIdentity([]byte("# nothing\n"))
	require.Error(t, err)
}

func TestDescribeToolResponseOutputSchema(t *testing.T) {
	var declared, undeclared describeToolResponse
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x_agent-manager_set_organization_model","outputSchema":{"type":"object","properties":{"credentials":{}}}}`), &declared))
	require.NotEmpty(t, declared.OutputSchema)
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x_agent-manager_set_organization_model","inputSchema":{"type":"object"}}`), &undeclared))
	require.Empty(t, undeclared.OutputSchema)
}

func TestCredentialsSignInManifest(t *testing.T) {
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			URL  string `json:"url"`
			Auth struct {
				Type         string `json:"type"`
				ForwardToken bool   `json:"forwardToken"`
			} `json:"auth"`
		} `json:"spec"`
	}
	require.NoError(t, sigsyaml.Unmarshal([]byte(credentialsSignInManifest("agents-x")), &obj))
	require.Equal(t, "MCPServer", obj.Kind)
	require.Equal(t, credentialsSignInServer, obj.Metadata.Name)
	require.Equal(t, "agents-x", obj.Metadata.Namespace)
	require.Equal(t, organizationsTestMark, obj.Metadata.Labels[organizationsTestLabel])
	require.Equal(t, oauthFixtureURL, obj.Spec.URL)
	require.Equal(t, "oauth", obj.Spec.Auth.Type)
	require.False(t, obj.Spec.Auth.ForwardToken)
}

func TestWithArgs(t *testing.T) {
	base := map[string]any{"a": 1}
	got := withArgs(base, map[string]any{"b": 2})
	require.Equal(t, map[string]any{"a": 1, "b": 2}, got)
	require.Equal(t, map[string]any{"a": 1}, base)
}

func TestMusterAnswerURLElicitations(t *testing.T) {
	url := mcp.ElicitationParams{Mode: mcp.ElicitationModeURL, URL: "https://p.example"}
	form := mcp.ElicitationParams{Mode: mcp.ElicitationModeForm}
	a := &musterAnswer{input: mcp.InputRequests{
		"u": {Method: mcp.MethodElicitationCreate, Elicitation: &url},
		"f": {Method: mcp.MethodElicitationCreate, Elicitation: &form},
	}}
	require.Equal(t, []mcp.ElicitationParams{url}, a.urlElicitations())
	_, err := oneURLElicitation(&musterAnswer{urlRequired: []mcp.ElicitationParams{url, url}})
	require.ErrorContains(t, err, "2 URL elicitations")
	_, err = oneURLElicitation(&musterAnswer{result: &musterToolResult{IsError: true, Content: []musterToolContent{{Type: toolResultText, Text: "registering muster with its authorization server: redirect_uri: hostname resolves to private IP address"}}}})
	require.ErrorContains(t, err, "isError true")
	require.ErrorContains(t, err, "redirect_uri: hostname resolves to private IP address")
}

func TestEnsureCredentialsAgeIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	first, path, err := ensureCredentialsAgeIdentity()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(credentialsAgeDir, credentialsAgeKey), path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	second, _, err := ensureCredentialsAgeIdentity()
	require.NoError(t, err)
	require.Equal(t, first.Recipient().String(), second.Recipient().String(), "the identity is generated once and kept")
}

func TestUserSubject(t *testing.T) {
	require.Equal(t, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "oidc:carol@lab.test"}, userSubject(&config.User{Email: "carol@lab.test"}))
}

func TestRedactValue(t *testing.T) {
	value := "s3cr3t-value"
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	got := redactValue(`{"value":"`+value+`","data":"`+encoded+`"}`, value)
	require.Equal(t, `{"value":"<redacted>","data":"<redacted>"}`, got)
	require.NotContains(t, got, value)
	require.NotContains(t, got, encoded)
	require.Equal(t, "unchanged", redactValue("unchanged", ""))
}

func TestReceiptNames(t *testing.T) {
	var a credentialAnswer
	require.NoError(t, json.Unmarshal([]byte(`{"credentialOperation":"op1","credentials":[
		{"secret":"tenant-credential-o","key":"apiKey","state":"missing"},
		{"secret":"tenant-credential-m","key":"apiKey","state":"committed"}]}`), &a))
	require.NoError(t, receiptNames(a, "op1", "tenant-credential-m", "apiKey", credentialStateCommitted))
	require.ErrorContains(t, receiptNames(a, "op1", "tenant-credential-m", "apiKey", credentialStatePresent), `is "committed", want present`)
	require.ErrorContains(t, receiptNames(a, "op1", "tenant-credential-x", "apiKey", credentialStateCommitted), "do not name tenant-credential-x/apiKey")
	require.ErrorContains(t, receiptNames(a, "op2", "tenant-credential-m", "apiKey", credentialStateCommitted), `names operation "op1"`)

	a.CredentialOperation = ""
	require.NoError(t, receiptNames(a, "op1", "tenant-credential-m", "apiKey", credentialStateCommitted))
	a.Pending = true
	require.ErrorContains(t, receiptNames(a, "op1", "tenant-credential-m", "apiKey", credentialStateCommitted), "still pending")
	require.ErrorContains(t, receiptNames(credentialAnswer{}, "op1", "tenant-credential-m", "apiKey", credentialStateCommitted), "do not name")
}

func TestAgeKeyFileMode(t *testing.T) {
	require.NoError(t, ageKeyFileMode(0o600))
	require.NoError(t, ageKeyFileMode(0o400))
	require.ErrorContains(t, ageKeyFileMode(0o640), "mode 0640")
	require.Error(t, ageKeyFileMode(0o604))
	require.Error(t, ageKeyFileMode(0o602))
}

func TestEnsureCredentialsAgeIdentityRefusesAReadableKey(t *testing.T) {
	t.Chdir(t.TempDir())
	_, path, err := ensureCredentialsAgeIdentity()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644)) // #nosec G302 -- the refused mode under test
	_, _, err = ensureCredentialsAgeIdentity()
	require.ErrorContains(t, err, "chmod 600")
}

func TestCredentialsContentPath(t *testing.T) {
	require.Equal(t, "organizations/"+organizationsRootNamespace+"/"+credentialsOrganization, credentialsContentPath("organizations"))
}

func TestContentPathRemovalCommand(t *testing.T) {
	got := contentPathRemovalCommand("acme/orgs", "main", "organizations/p/o")
	require.Contains(t, got, "git clone -q --depth 1 --branch main "+measureGitURL+"/acme/orgs.git agentlab-credentials-cleanup")
	require.Contains(t, got, "rm -r -q organizations/p/o")
	require.Contains(t, got, "push -q origin HEAD:main")
	require.NotContains(t, got, "%!")
}

func TestContainerArgs(t *testing.T) {
	sidecar := corev1.Container{Name: "sidecar", Args: []string{"--other"}}
	main := corev1.Container{Name: agentManagerMCPServer, Command: []string{"/agent-manager", "--flux-namespace=flux"}, Args: []string{"--credential-ui-url=https://x"}}
	args, err := containerArgs([]corev1.Container{sidecar, main}, agentManagerMCPServer)
	require.NoError(t, err)
	require.Equal(t, []string{"/agent-manager", "--flux-namespace=flux", "--credential-ui-url=https://x"}, args)
	require.Equal(t, map[string]string{flagFluxNamespace: "flux", flagCredentialUIURL: "https://x"}, containerFlags(args)) // #nosec G101 -- flag names, not credentials
	args, err = containerArgs([]corev1.Container{sidecar}, agentManagerMCPServer)
	require.NoError(t, err, "the one container is read whatever its name")
	require.Equal(t, sidecar.Args, args)
	_, err = containerArgs([]corev1.Container{sidecar, {Name: "other"}}, agentManagerMCPServer)
	require.ErrorContains(t, err, "no container named")
}

func TestFormOperation(t *testing.T) {
	const tpl = "https://portal.example/organizations/agentlab/{parent}/{organization}/credentials{?operation}"
	op, err := formOperation(tpl, "https://portal.example/organizations/agentlab/root/org/credentials?operation=9b0e2fc5", "root", "org")
	require.NoError(t, err)
	require.Equal(t, "9b0e2fc5", op)

	for name, link := range map[string]string{
		"another organization": "https://portal.example/organizations/agentlab/root/other/credentials?operation=9b0e2fc5",
		"another origin":       "https://evil.example/organizations/agentlab/root/org/credentials?operation=9b0e2fc5",
		"an extra parameter":   "https://portal.example/organizations/agentlab/root/org/credentials?operation=9b0e2fc5&token=abc",
		"no operation":         "https://portal.example/organizations/agentlab/root/org/credentials?operation=",
	} {
		_, err := formOperation(tpl, link, "root", "org")
		require.Error(t, err, name)
	}

	op, err = formOperation("https://p.example/c/{operation}", "https://p.example/c/abc-1", "root", "org")
	require.NoError(t, err)
	require.Equal(t, "abc-1", op)
	_, err = formOperation("http://p.example/c/{operation}", "http://p.example/c/abc-1", "root", "org")
	require.ErrorContains(t, err, "not an https URL")
	_, err = formOperation("https://p.example/c", "https://p.example/c", "root", "org")
	require.ErrorContains(t, err, "does not expand {operation} once")
}

func TestSameReceipt(t *testing.T) {
	commit := func(number int, branch string) credentialAnswer {
		var a credentialAnswer
		require.NoError(t, json.Unmarshal([]byte(`{"commit":{"number":`+strconv.Itoa(number)+`,"branch":"`+branch+`"}}`), &a))
		return a
	}
	require.NoError(t, sameReceipt(credentialAnswer{}, credentialAnswer{}))
	require.NoError(t, sameReceipt(commit(7, "b"), commit(7, "b")))
	require.ErrorContains(t, sameReceipt(commit(7, "b"), commit(8, "b")), "#8")
	require.ErrorContains(t, sameReceipt(commit(7, "b"), commit(7, "c")), `"c"`)
	require.Error(t, sameReceipt(commit(7, "b"), credentialAnswer{}))
}

func TestValueCarried(t *testing.T) {
	const value = "s3cr3t-value"
	require.Empty(t, valueCarried("nothing here", value))
	require.Equal(t, "in plaintext", valueCarried("key="+value, value))
	require.Equal(t, "base64-encoded", valueCarried(`"data":"`+base64.StdEncoding.EncodeToString([]byte(value))+`"`, value))
	require.Empty(t, valueCarried("anything", ""))
}

func TestMusterAnswerAccepted(t *testing.T) {
	a := &musterAnswer{input: mcp.InputRequests{"e1": {Method: mcp.MethodElicitationCreate}}, requestState: "request-state-1"}
	rt := a.accepted()
	require.Equal(t, a.requestState, rt.RequestState)
	raw, err := json.Marshal(rt.InputResponses)
	require.NoError(t, err)
	require.JSONEq(t, `{"e1":{"action":"accept"}}`, string(raw))
}

func TestAbsent(t *testing.T) {
	require.NoError(t, absent(nil, apierrors.NewNotFound(gvrNamespaces.GroupResource(), "x")))
	require.ErrorContains(t, absent(&unstructured.Unstructured{}, nil), "still present")
	require.Error(t, absent(nil, apierrors.NewForbidden(gvrNamespaces.GroupResource(), "x", nil)))
}

func TestConformsTo(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"type": "object",
		"required": ["pending", "credentials"],
		"properties": {
			"pending": {"type": "boolean"},
			"credentials": {"type": "array", "items": {"type": "object", "required": ["secret"]}}
		}
	}`), &schema))
	require.NoError(t, conformsTo(schema, json.RawMessage(`{"pending":true,"credentials":[{"secret":"tenant-credential-x"}]}`)))
	require.Error(t, conformsTo(schema, json.RawMessage(`{"pending":"yes","credentials":[]}`)))
	require.Error(t, conformsTo(schema, json.RawMessage(`{"pending":true,"credentials":[{}]}`)))
	require.Error(t, conformsTo(schema, json.RawMessage(`{"pending":true}`)))
}

// A confirmation passes only with its one write and exactly the expected
// consumers: another consumer, another key, an origin missing or an origin
// that gets no credential each fail.
func TestConfirmationListsOnly(t *testing.T) {
	egress := credentialReference{Kind: egressPolicyKind, Name: "e", Key: credentialTokenKey, Origins: []string{"https://a.example", "https://b.example"}}
	write := []credentialChange{{Action: "create", Object: map[string]any{kindKey: egressPolicyKind, crMetadata: map[string]any{nameKey: "e"}}}}
	ok := confirmation{Changes: write, References: []credentialReference{{Kind: egressPolicyKind, Name: "e", Key: credentialTokenKey, Origins: []string{"https://b.example", "https://a.example"}}}}
	require.NoError(t, confirmationListsOnly(ok, egressPolicyKind, "e", []credentialReference{egress}))
	for name, c := range map[string]confirmation{
		"another consumer": {Changes: write, References: append(slices.Clone(ok.References), credentialReference{Kind: "ModelConfig", Name: "m", Key: credentialTokenKey})},
		"another key":      {Changes: write, References: []credentialReference{{Kind: egressPolicyKind, Name: "e", Key: "apiKey", Origins: egress.Origins}}},
		"an origin short":  {Changes: write, References: []credentialReference{{Kind: egressPolicyKind, Name: "e", Key: credentialTokenKey, Origins: egress.Origins[:1]}}},
		"a plain origin":   {Changes: write, References: []credentialReference{{Kind: egressPolicyKind, Name: "e", Key: credentialTokenKey, Origins: append(slices.Clone(egress.Origins), "https://plain.example")}}},
		"no reference":     {Changes: write},
		"another write":    {Changes: []credentialChange{{Action: "create", Object: map[string]any{kindKey: egressPolicyKind, crMetadata: map[string]any{nameKey: "x"}}}}, References: ok.References},
		"two writes":       {Changes: append(slices.Clone(write), write...), References: ok.References},
	} {
		require.Error(t, confirmationListsOnly(c, egressPolicyKind, "e", []credentialReference{egress}), name)
	}
}

// 428 alone from agent-manager 2.23.0 (giantswarm/agent-manager#354); an
// older or unreadable version also admits 400 and says so.
func TestIfMatchRequiredStatuses(t *testing.T) {
	for _, v := range []string{"2.23.0", "v2.23.2", "2.24.0-rc.2"} {
		got, note := ifMatchRequiredStatuses(v)
		require.Equal(t, []int{428}, got, v)
		require.Empty(t, note, v)
	}
	for _, v := range []string{"2.22.0", "2.23.0-rc.9", ""} {
		got, note := ifMatchRequiredStatuses(v)
		require.Equal(t, []int{428, 400}, got, v)
		require.Contains(t, note, "giantswarm/agent-manager#354", v)
	}
}

func TestJudgeComponentLogs(t *testing.T) {
	value := "s3cr3t-value"
	lines, err := judgeComponentLogs("muster", "a\n\nb\n", value)
	require.NoError(t, err)
	require.Equal(t, 2, lines)
	_, err = judgeComponentLogs("muster", "\n \n", value)
	require.ErrorContains(t, err, "read no log line")
	_, err = judgeComponentLogs("muster", "x "+base64.StdEncoding.EncodeToString([]byte(value))+"\n", value)
	require.ErrorContains(t, err, "base64-encoded")
	require.NotContains(t, err.Error(), value)
}

// A refusal is agent-manager's error result with its code: muster's own
// error, a result and a pending elicitation are not.
func TestRefusedWith(t *testing.T) {
	result := func(isError bool, text string) *musterAnswer {
		return &musterAnswer{result: &musterToolResult{IsError: isError, Content: []musterToolContent{{Type: toolResultText, Text: text}}}}
	}
	text, err := refusedWith(result(true, "forbidden: credential state cannot be verified"), "forbidden")
	require.NoError(t, err)
	require.Contains(t, text, "credential state")
	_, err = refusedWith(result(true, "tool x_agent-manager_set_organization_model not found"), "forbidden")
	require.Error(t, err)
	_, err = refusedWith(result(false, `{"pending":true}`), "forbidden")
	require.Error(t, err)
	_, err = refusedWith(&musterAnswer{urlRequired: []mcp.ElicitationParams{{Mode: mcp.ElicitationModeURL}}}, "forbidden")
	require.Error(t, err)
	_, err = refusedWith(result(true, "conflict: another consumer references the credential Secret"), "forbidden")
	require.Error(t, err)
}

// Three different people by role whatever the order of the lab's users: the
// order admin, viewer, developer gives the developer as the other admin,
// never the viewer; a viewer who is also an admin or a developer is no
// viewer; a lab without either role is an error naming it.
func TestCredentialsPeople(t *testing.T) {
	admin := config.User{Email: "admin@lab.test", Groups: []string{platformAdminsGroup, developersGroup}}
	viewer := config.User{Email: "viewer@lab.test", Groups: []string{viewerGroup}}
	dev := config.User{Email: "dev@lab.test", Groups: []string{developersGroup}}
	for _, users := range [][]config.User{{admin, viewer, dev}, {admin, dev, viewer}, {viewer, admin, dev}} {
		cfg := &config.Config{Users: users}
		other, v, err := credentialsPeople(cfg, &cfg.Users[slices.IndexFunc(users, func(u config.User) bool { return u.Email == admin.Email })])
		require.NoError(t, err)
		require.Equal(t, dev.Email, other.Email)
		require.Equal(t, viewer.Email, v.Email)
	}
	const noViewer = "in viewers only"
	// The lab's default users give dev and viewer.
	defaults := config.Default()
	other, v, err := credentialsPeople(defaults, defaults.FindUser("admin@lab.local"))
	require.NoError(t, err)
	require.Equal(t, []string{accessDefaultSecond, testPortalViewer}, []string{other.Email, v.Email})
	for name, tc := range map[string]struct {
		users []config.User
		want  string
	}{
		"no second admin":                {[]config.User{admin, viewer}, "second organization admin"},
		"no viewer":                      {[]config.User{admin, dev}, noViewer},
		"the viewer is a platform admin": {[]config.User{admin, dev, {Email: "v@lab.test", Groups: []string{viewerGroup, platformAdminsGroup}}}, noViewer},
		"the viewer is a developer":      {[]config.User{admin, dev, {Email: "v@lab.test", Groups: []string{viewerGroup, developersGroup}}}, noViewer},
	} {
		cfg := &config.Config{Users: tc.users}
		_, _, err := credentialsPeople(cfg, &cfg.Users[0])
		require.ErrorContains(t, err, tc.want, name)
	}
}

// The old refusal of a person's own operation is a SKIP only below the
// release that stops reading Secrets as the caller; from it on, and for any
// other refusal, it is no SKIP.
func TestOwnCredentialRefused(t *testing.T) {
	refused := errors.New("set_organization_model failed: forbidden: credential state cannot be verified")
	// 2.24.0-rc.1 still reads the Secret; #360 is in rc.2.
	for _, v := range []string{"2.23.2", "2.24.0-rc.1"} {
		require.Contains(t, ownCredentialRefused("dev@lab.test", v, refused), "giantswarm/agent-manager#360", v)
	}
	for _, v := range []string{"2.24.0-rc.2", "2.24.0"} {
		require.Empty(t, ownCredentialRefused("dev@lab.test", v, refused), v)
	}
	require.Empty(t, ownCredentialRefused("dev@lab.test", "2.23.2", errors.New("set_organization_model failed: forbidden: create ModelConfig")))
	require.Empty(t, ownCredentialRefused("dev@lab.test", "2.23.2", nil))
}

// A fixture whose viewer may write, or whose other admin may not, is wrong.
func TestFixtureProblems(t *testing.T) {
	viewer := organizationRights{listModelConfigs: true}
	require.Empty(t, viewerFixtureProblem("v@lab.test", "ns", viewer))
	for name, r := range map[string]organizationRights{
		"creates secrets":      {listModelConfigs: true, createSecrets: true},
		"creates modelconfigs": {listModelConfigs: true, createModelConfigs: true},
		"reads nothing":        {},
	} {
		require.Contains(t, viewerFixtureProblem("v@lab.test", "ns", r), "fixture wrong", name)
	}
	admin := organizationRights{listModelConfigs: true, createSecrets: true, createModelConfigs: true}
	require.Empty(t, otherAdminFixtureProblem("d@lab.test", "ns", admin))
	require.Contains(t, otherAdminFixtureProblem("d@lab.test", "ns", viewer), "fixture wrong")
	require.Contains(t, otherAdminFixtureProblem("d@lab.test", "ns", organizationRights{listModelConfigs: true, createSecrets: true}), "fixture wrong")
}

// The viewer's refusal passes only as the apiserver's denial of that user's
// create; the read-first refusal of older releases is a SKIP below the
// floor and a failure from it; another user's denial or another guard fails.
func TestViewerRefusalVerdict(t *testing.T) {
	user := "oidc:viewer@lab.test"
	rbac := `forbidden: dry run of create credential Secret ns/tenant-credential-x: secrets "tenant-credential-x" is forbidden: User "oidc:viewer@lab.test" cannot create resource "secrets" in API group "" in the namespace "ns"; ask an admin`
	denied, skip, err := viewerRefusalVerdict("2.24.0-rc.2", rbac, user, "ns")
	require.NoError(t, err)
	require.Empty(t, skip)
	require.Equal(t, "create secrets", denied)
	denied, _, err = viewerRefusalVerdict("2.23.2", `forbidden: dry run of create ModelConfig ns/x: User "oidc:viewer@lab.test" cannot create resource "modelconfigs" in API group "kagent.dev" in the namespace "ns"`, user, "ns")
	require.NoError(t, err)
	require.Equal(t, "create modelconfigs", denied)

	old := "forbidden: credential state cannot be verified"
	for _, v := range []string{"2.23.2", "2.24.0-rc.1"} {
		_, skip, err = viewerRefusalVerdict(v, old, user, "ns")
		require.NoError(t, err, v)
		require.Contains(t, skip, "giantswarm/agent-manager#360", v)
	}
	_, _, err = viewerRefusalVerdict("2.24.0-rc.2", old, user, "ns")
	require.ErrorContains(t, err, "unrelated guard")
	for name, refusal := range map[string]string{
		"another user":      strings.ReplaceAll(rbac, "viewer@", "dev@"),
		"another namespace": strings.ReplaceAll(rbac, `namespace "ns"`, `namespace "other"`),
		"another guard":     "forbidden: organization agentlab-credentials is being deleted",
	} {
		_, _, err = viewerRefusalVerdict("2.24.0-rc.2", refusal, user, "ns")
		require.ErrorContains(t, err, "unrelated guard", name)
	}
}
