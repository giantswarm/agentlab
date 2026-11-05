package lab

import (
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// A read the authorizer allows answers the Secret or NotFound; a refusal is
// a 403 and nothing else: an answered or otherwise failed refused read fails.
func TestReadVerdict(t *testing.T) {
	gr := corev1.Resource("secrets")
	forbidden := apierrors.NewForbidden(gr, "s", errors.New("no"))
	notFound := apierrors.NewNotFound(gr, "s")
	allowed := secretRead{namespace: kagentNamespace, name: "s", allowed: true}
	refused := secretRead{namespace: "org-lab", name: "s"}
	for name, c := range map[string]struct {
		read secretRead
		err  error
		ok   bool
	}{
		"allowed, answered":   {allowed, nil, true},
		"allowed, not found":  {allowed, notFound, true},
		"allowed, forbidden":  {allowed, forbidden, false},
		"refused, forbidden":  {refused, forbidden, true},
		"refused, answered":   {refused, nil, false},
		"refused, not found":  {refused, notFound, false},
		"list, forbidden":     {secretRead{}, forbidden, true},
		"refused, other fail": {refused, errors.New("connection refused"), false},
	} {
		_, err := readVerdict(c.read, c.err)
		if (err == nil) != c.ok {
			t.Errorf("%s: verdict error %v, want ok=%v", name, err, c.ok)
		}
	}
}

// The hooks' identity: its ServiceAccount and the RBAC named after it, hook
// annotations dropped; the Jobs, another identity's RBAC and other kinds left out.
func TestHookRBAC(t *testing.T) {
	manifests := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: agent-platform-connectivity-hooks
  namespace: agent-platform
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-platform-connectivity-hooks-substrate-bootstrap
  namespace: ate-system
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
rules:
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [egress-mitm-ca-pool]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: agent-manager-migrate
  namespace: agent-platform
---
apiVersion: batch/v1
kind: Job
metadata:
  name: agent-platform-connectivity-hooks-job
  namespace: agent-platform
`
	objs, err := hookRBAC(manifests)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName())
		if len(o.GetAnnotations()) != 0 {
			t.Errorf("%s keeps annotations %v", o.GetName(), o.GetAnnotations())
		}
	}
	want := []string{
		"ServiceAccount/agent-platform/agent-platform-connectivity-hooks",
		"Role/ate-system/agent-platform-connectivity-hooks-substrate-bootstrap",
	}
	if !slices.Equal(got, want) {
		t.Errorf("hookRBAC = %v, want %v", got, want)
	}
}

func TestSecretReadString(t *testing.T) {
	if got := (secretRead{}).String(); got != "list secrets -A" {
		t.Errorf("a list reads %q", got)
	}
	if got := (secretRead{namespace: "org-lab", name: "agentlab-kubeconfig"}).String(); !strings.HasSuffix(got, "org-lab/agentlab-kubeconfig") {
		t.Errorf("a get reads %q", got)
	}
}
