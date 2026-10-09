package lab

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kagent's ModelConfig, AgentTemplate, Agent, Harness and RemoteMCPServer as
// the fakes serve them (newFakeLab registers them with its mapper and the
// dynamic fake's list kinds, next to the HelmRelease, MCPServer and
// Prometheus kinds kube_test.go declares). The versions are the fakes' — the
// lab itself resolves every custom kind through discovery (gvrFor) and pins
// none.
var (
	kagentGroupVersion = schema.GroupVersion{Group: kagentAPIGroup, Version: "v1alpha3"}
	// releasedKagentGroupVersion is the ModelConfig's group on the kagent
	// line before the api.kagent.dev crossing.
	releasedKagentGroupVersion = schema.GroupVersion{Group: "kagent.dev", Version: kagentGroupVersion.Version}
	gvkModelConfig             = kagentGroupVersion.WithKind("ModelConfig")
	gvrModelConfigs            = kagentGroupVersion.WithResource("modelconfigs")
	gvkAgentTemplate           = kagentGroupVersion.WithKind(kindAgentTemplate)
	gvrAgentTemplates          = kagentGroupVersion.WithResource("agenttemplates")
	gvkAgent                   = kagentGroupVersion.WithKind(kindAgent)
	gvrAgents                  = kagentGroupVersion.WithResource("agents")
	gvkHarness                 = kagentGroupVersion.WithKind("Harness")
	gvrHarnesses               = kagentGroupVersion.WithResource("harnesses")
	gvkRemoteMCPServer         = kagentGroupVersion.WithKind(remoteMCPServerKind)
	gvrRemoteMCPServers        = kagentGroupVersion.WithResource("remotemcpservers")
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
