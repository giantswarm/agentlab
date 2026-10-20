package lab

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// managedEntry is one managedFields entry of a fixture: the manager, the
// subresource it wrote ("" for the object) and its fieldsV1 as JSON.
func managedEntry(manager, subresource, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1",
		FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(fields), Subresource: subresource,
	}
}

// helmDeployment is a Helm-owned Deployment carrying the entries.
func helmDeployment(entries ...metav1.ManagedFieldsEntry) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("apps/v1")
	obj.SetKind(kindDeployment)
	obj.SetNamespace(platformNamespace)
	obj.SetName(componentMuster)
	obj.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	obj.SetManagedFields(entries)
	return obj
}

// The fields a muster Deployment carries as the lab runs it: the release's
// render by helm-controller, the controller-manager's status and revision,
// the lab's own and kubectl's rollout restarts.
var (
	renderedFields = managedEntry("helm-controller", "",
		`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"muster\"}":{".":{},"f:image":{},"f:name":{}}},"f:hostNetwork":{}}}}}`)
	statusFields = managedEntry("kube-controller-manager", "status",
		`{"f:status":{"f:replicas":{},"f:readyReplicas":{}}}`)
	revisionFields = managedEntry("kube-controller-manager", "",
		`{"f:metadata":{"f:annotations":{".":{},"f:deployment.kubernetes.io/revision":{}}}}`)
	labRestart = managedEntry(applyFieldManager, "",
		`{"f:spec":{"f:template":{"f:metadata":{"f:annotations":{"f:kubectl.kubernetes.io/restartedAt":{}}}}}}`)
	kubectlRestart = managedEntry("kubectl-rollout", "",
		`{"f:spec":{"f:template":{"f:metadata":{"f:annotations":{".":{},"f:kubectl.kubernetes.io/restartedAt":{}}}}}}`)
)

// TestForeignFieldsOfAPatchedDeployment: a Deployment patched by a non-Helm
// field manager — hostNetwork and dnsPolicy by `kubectl patch`, an
// init container added the same way, an env var by `kubectl set env` — is
// reported per manager with the outermost paths it wrote (the container, not
// its every field); the release's writers, the status subresource and the
// rollout restarts are never reported.
func TestForeignFieldsOfAPatchedDeployment(t *testing.T) {
	patched := helmDeployment(renderedFields, statusFields, revisionFields, labRestart, kubectlRestart,
		managedEntry("kubectl-patch", "",
			`{"f:spec":{"f:template":{"f:spec":{"f:dnsPolicy":{},"f:hostNetwork":{},"f:initContainers":{".":{},"k:{\"name\":\"dex-localhost\"}":{".":{},"f:image":{},"f:name":{},"f:args":{}}}}}}}`),
		managedEntry("kubectl-set", "",
			`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"muster\"}":{"f:env":{"k:{\"name\":\"DEBUG\"}":{".":{},"f:name":{},"f:value":{}}}}}}}}}`),
	)
	found, err := foreignFieldsOf(patched)
	if err != nil {
		t.Fatal(err)
	}
	want := []foreignFields{
		{Kind: kindDeployment, Namespace: platformNamespace, Name: componentMuster, Manager: "kubectl-patch",
			Fields: []string{".spec.template.spec.dnsPolicy", ".spec.template.spec.hostNetwork", ".spec.template.spec.initContainers"}},
		{Kind: kindDeployment, Namespace: platformNamespace, Name: componentMuster, Manager: "kubectl-set",
			Fields: []string{`.spec.template.spec.containers[name="muster"].env[name="DEBUG"]`}},
	}
	if len(found) != len(want) {
		t.Fatalf("found %+v, want %+v", found, want)
	}
	for i := range want {
		if found[i].Manager != want[i].Manager || found[i].Name != want[i].Name || !slices.Equal(found[i].Fields, want[i].Fields) {
			t.Errorf("entry %d = %+v, want %+v", i, found[i], want[i])
		}
	}

	err = foreignFieldsError(found)
	for _, w := range []string{
		"2 platform workload field(s) were written outside the platform's Helm releases",
		"deployment agent-platform/muster: .spec.template.spec.dnsPolicy, .spec.template.spec.hostNetwork, .spec.template.spec.initContainers by kubectl-patch",
		"by kubectl-set",
	} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
	if line := workloadsLine(found); !strings.HasPrefix(line, "patched outside the releases") || !strings.Contains(line, "by kubectl-patch; deployment") {
		t.Errorf("workloadsLine = %q", line)
	}
}

// TestForeignFieldsOfARenderedDeployment: a Deployment carrying only the
// release's render, its status and rollout restarts reports nothing.
func TestForeignFieldsOfARenderedDeployment(t *testing.T) {
	found, err := foreignFieldsOf(helmDeployment(renderedFields, statusFields, revisionFields, labRestart, kubectlRestart))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %+v, want none", found)
	}
	if line := workloadsLine(found); line != "as the releases render them" {
		t.Errorf("workloadsLine = %q", line)
	}
}

// TestForeignFieldsOfTheConfigReloadStamp: the connectivity release's
// config-reload hook stamps the app-config checksum into the Backstage pod
// template under its own field manager. It is the release's writer: neither
// reported as foreign nor put back as a hand's edit.
func TestForeignFieldsOfTheConfigReloadStamp(t *testing.T) {
	stamp := managedEntry(configReloadFieldManager, "",
		`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"backstage\"}":{"f:env":{"k:{\"name\":\"AGENT_PLATFORM_APP_CONFIG_CHECKSUM\"}":{".":{},"f:name":{},"f:value":{}}}}}}}}}`)
	found, err := foreignFieldsOf(helmDeployment(renderedFields, statusFields, revisionFields, stamp))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %+v, want none", found)
	}
	if isHandEdit(stamp) {
		t.Errorf("the config-reload stamp is taken for a hand's edit")
	}
}

// TestForeignFieldsJSON: `agentlab status -o json` carries the fields.
func TestForeignFieldsJSON(t *testing.T) {
	raw, err := json.Marshal(Status{ForeignFields: []foreignFields{{Kind: kindDeployment, Namespace: platformNamespace, Name: componentMuster, Manager: "kubectl-patch", Fields: []string{".spec.template.spec.hostNetwork"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"foreignFields":[{"kind":"Deployment","namespace":"agent-platform","name":"muster","manager":"kubectl-patch","fields":[".spec.template.spec.hostNetwork"]}]`) {
		t.Errorf("json = %s", raw)
	}
}
