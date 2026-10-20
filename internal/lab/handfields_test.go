package lab

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// backstageWorkload is a Deployment of the platform's backstage container as
// a release renders it (rendered) or as it runs (live), the env given as
// {"name":…,"value":…} objects.
func backstageWorkload(t *testing.T, hostNetwork bool, env ...string) *unstructured.Unstructured {
	t.Helper()
	obj := helmDeployment()
	obj.SetName(componentBackstage)
	var spec map[string]any
	if err := json.Unmarshal(fmt.Appendf(nil, `{"replicas":1,"template":{"spec":{"hostNetwork":%t,"containers":[`+
		`{"name":"backstage","image":"gsoci.azurecr.io/giantswarm/backstage:1.0.0","env":[%s]}]}}}`,
		hostNetwork, strings.Join(env, ",")), &spec); err != nil {
		t.Fatal(err)
	}
	obj.Object[crSpec] = runtime.DeepCopyJSON(spec)
	return obj
}

func envVar(name, value string) string { return fmt.Sprintf(`{"name":%q,"value":%q}`, name, value) }

// TestPutBackRenderOfKubectlSetEnv: a Deployment a hand patched — an env var
// added by `kubectl set env`, hostNetwork flipped by `kubectl patch` — is put
// back to its release's render: the added env var removed, the flipped field
// set to the rendered value, both managers' entries dropped, the rendered env
// var and the release's writers untouched; what was put back is named.
func TestPutBackRenderOfKubectlSetEnv(t *testing.T) {
	rendered := backstageWorkload(t, false, envVar("LOG_LEVEL", "info"))
	live := backstageWorkload(t, true, envVar("LOG_LEVEL", "info"), envVar("AGENT_PLATFORM_APP_CONFIG_CHECKSUM", "abc"))
	live.SetManagedFields(slices.Concat(helmDeployment(renderedFields, statusFields, labRestart).GetManagedFields(), helmDeployment(
		managedEntry("kubectl-set", "",
			`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"backstage\"}":{"f:env":{"k:{\"name\":\"AGENT_PLATFORM_APP_CONFIG_CHECKSUM\"}":{".":{},"f:name":{},"f:value":{}}}}}}}}}`),
		managedEntry("kubectl-patch", "",
			`{"f:spec":{"f:template":{"f:spec":{"f:hostNetwork":{}}}}}`),
	).GetManagedFields()))

	put, err := putBackRender(live, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Object[crSpec], rendered.Object[crSpec]) {
		t.Errorf("spec after the put-back = %v, want the render %v", live.Object[crSpec], rendered.Object[crSpec])
	}
	var managers []string
	for _, entry := range live.GetManagedFields() {
		managers = append(managers, entry.Manager)
	}
	if want := []string{renderedFields.Manager, statusFields.Manager, labRestart.Manager}; !reflect.DeepEqual(managers, want) {
		t.Errorf("managers after the put-back = %q, want %q", managers, want)
	}
	if foreign, err := foreignFieldsOf(live); err != nil || len(foreign) > 0 {
		t.Errorf("foreign fields after the put-back = %v (%v), want none", foreign, err)
	}
	var named []string
	for _, f := range put {
		named = append(named, f.String())
	}
	want := []string{
		`deployment agent-platform/backstage: .spec.template.spec.containers[name="backstage"].env[name="AGENT_PLATFORM_APP_CONFIG_CHECKSUM"] by kubectl-set`,
		`deployment agent-platform/backstage: .spec.template.spec.hostNetwork by kubectl-patch`,
	}
	if !reflect.DeepEqual(named, want) {
		t.Errorf("put back = %q, want %q", named, want)
	}
}

// TestPutBackRenderLeavesOtherManagers: a field written by a manager that is
// not kubectl's imperative edit (an operator's, a rollout's) is no hand's to
// undo: left in place and in managedFields, for the foreign-field check to
// name.
func TestPutBackRenderLeavesOtherManagers(t *testing.T) {
	rendered := backstageWorkload(t, false)
	live := backstageWorkload(t, false, envVar("INJECTED", "1"))
	live.SetManagedFields(helmDeployment(renderedFields, managedEntry("some-operator", "",
		`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"backstage\"}":{"f:env":{"k:{\"name\":\"INJECTED\"}":{".":{},"f:name":{},"f:value":{}}}}}}}}}`),
	).GetManagedFields())

	put, err := putBackRender(live, rendered)
	if err != nil || len(put) > 0 {
		t.Fatalf("putBackRender = %v, %v; want nothing put back", put, err)
	}
	if foreign, _ := foreignFieldsOf(live); len(foreign) != 1 || foreign[0].Manager != "some-operator" {
		t.Errorf("foreign fields = %v, want the operator's env var", foreign)
	}
}

// TestPutBackRenderNamesAPathItCannotWalk: a hand's field the walk cannot
// address (a list item by position) fails naming the field, and the object
// is left as it was.
func TestPutBackRenderNamesAPathItCannotWalk(t *testing.T) {
	rendered := backstageWorkload(t, false)
	live := backstageWorkload(t, false)
	live.SetManagedFields(helmDeployment(managedEntry("kubectl-edit", "",
		`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"i:0":{}}}}}}`),
	).GetManagedFields())
	before := live.DeepCopy()

	_, err := putBackRender(live, rendered)
	if err == nil || !strings.Contains(err.Error(), ".spec.template.spec.containers[0] by kubectl-edit") {
		t.Fatalf("putBackRender error = %v, want one naming the field and its manager", err)
	}
	if !reflect.DeepEqual(live, before) {
		t.Errorf("the object changed on a failed put-back")
	}
}
