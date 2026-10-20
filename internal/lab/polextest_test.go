package lab

import (
	"testing"
)

func TestPolicyExceptionsOf(t *testing.T) {
	manifest := `
apiVersion: kyverno.io/v2
kind: PolicyException
metadata:
  name: substrate-workers
  namespace: policy-exceptions
spec: {}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: not-an-exception
---
apiVersion: example.io/v1
kind: PolicyException
metadata:
  name: another-group
`
	got, err := policyExceptionsOf("agent-platform-connectivity", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].obj.GetName() != "substrate-workers" || got[0].source != "agent-platform-connectivity" {
		t.Fatalf("want the one kyverno.io PolicyException, got %+v", got)
	}
}

func TestWithKyvernoPolicies(t *testing.T) {
	const namespace = "elsewhere"
	values := map[string]any{"kyvernoPolicies": map[string]any{"enabled": false, "policyExceptionNamespace": namespace}}
	withKyvernoPolicies(values)
	block := values["kyvernoPolicies"].(map[string]any)
	if block["enabled"] != true || block["policyExceptionNamespace"] != namespace {
		t.Fatalf("want enabled with the namespace kept, got %v", block)
	}
	empty := map[string]any{}
	withKyvernoPolicies(empty)
	if empty["kyvernoPolicies"].(map[string]any)["enabled"] != true {
		t.Fatalf("want the block created, got %v", empty)
	}
}

func TestPolexVerdictString(t *testing.T) {
	v := polexVerdict{source: "agent-platform-connectivity", namespace: "policy-exceptions", name: "substrate-atelet"}
	if got, want := v.String(), "policy-exceptions/substrate-atelet (agent-platform-connectivity): admitted"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
