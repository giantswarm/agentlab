package lab

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"filippo.io/age"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestUIURLPrefix(t *testing.T) {
	require.Equal(t, "https://p.example/orgs/", uiURLPrefix("https://p.example/orgs/{parent}/{organization}?operation={operation}"))
	require.Equal(t, "https://p.example/c", uiURLPrefix("https://p.example/c{?operation}"))
	require.Equal(t, "https://p.example/c", uiURLPrefix("https://p.example/c"))
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

func TestDescribesOutputSchema(t *testing.T) {
	declared, err := describesOutputSchema(`{"name":"x_agent-manager_set_organization_model","outputSchema":{"type":"object","properties":{"credentials":{}}}}`)
	require.NoError(t, err)
	require.True(t, declared)
	declared, err = describesOutputSchema(`{"name":"x_agent-manager_set_organization_model","inputSchema":{"type":"object"}}`)
	require.NoError(t, err)
	require.False(t, declared)
	_, err = describesOutputSchema("not json")
	require.Error(t, err)
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

func TestCredentialsOtherUser(t *testing.T) {
	cfg := &config.Config{Users: []config.User{
		{Email: "first@lab.test", Groups: []string{platformAdminsGroup}},
		{Email: "second@lab.test", Groups: []string{platformAdminsGroup}},
		{Email: "third@lab.test", Groups: []string{developersGroup}},
	}}
	require.Equal(t, "third@lab.test", credentialsOtherUser(cfg, &cfg.Users[0]).Email)
	require.Nil(t, credentialsOtherUser(&config.Config{Users: cfg.Users[:2]}, &cfg.Users[0]))
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
