package lab

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kagent's ModelConfig and Agent as the fakes serve them (newFakeLab registers
// them with its mapper and the dynamic fake's list kinds, next to the
// HelmRelease, MCPServer and Prometheus kinds kube_test.go declares). The
// versions are the fakes' — the lab itself resolves every custom kind through
// discovery (gvrFor) and pins none.
var (
	kagentGroupVersion = schema.GroupVersion{Group: "kagent.dev", Version: "v1alpha2"}
	gvkModelConfig     = kagentGroupVersion.WithKind("ModelConfig")
	gvrModelConfigs    = kagentGroupVersion.WithResource("modelconfigs")
	gvkAgent           = kagentGroupVersion.WithKind("Agent")
	gvrAgents          = kagentGroupVersion.WithResource("agents")
)

// customObject builds a seed for the dynamic fake: an object of the given
// kind, namespace and name, carrying the labels.
func customObject(gvk schema.GroupVersionKind, ns, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	if len(labels) > 0 {
		u.SetLabels(labels)
	}
	return u
}

// withCondition stamps one status.condition on the object and returns it.
func withCondition(u *unstructured.Unstructured, condType, condStatus, message string) *unstructured.Unstructured {
	cond := map[string]any{fieldType: condType, fieldStatus: condStatus}
	if message != "" {
		cond["message"] = message
	}
	_ = unstructured.SetNestedSlice(u.Object, []any{cond}, fieldStatus, "conditions")
	return u
}
