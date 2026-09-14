package lab

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// storeDeployment is the chart's Deployment with the Secret store: the two
// store env entries, the keys volume only, the default strategy.
func storeDeployment() *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: klausGatewayComponent, Namespace: platformNamespace}}
	d.Spec.Template.Spec.ServiceAccountName = klausGatewayComponent
	d.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:  klausGatewayComponent,
		Image: "gsoci.azurecr.io/giantswarm/klaus-gateway:1.5.0",
		Env: []corev1.EnvVar{
			{Name: "KLAUS_GATEWAY_OBO_ENABLED", Value: "true"},
			{Name: oboStoreEnv, Value: oboStoreSecretBackend},
			{Name: oboStoreSecret, Value: klausGatewayLinksSecret},
		},
	}}
	d.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "obo-keys"}}
	return d
}

// TestAssertStoreDeployment: the Secret store's Deployment passes; the bolt
// store, a mounted store volume and the Recreate strategy are refused with
// the reason.
func TestAssertStoreDeployment(t *testing.T) {
	if err := assertStoreDeployment(storeDeployment()); err != nil {
		t.Fatalf("the Secret store's Deployment: %v", err)
	}
	cases := map[string]struct {
		mutate func(*appsv1.Deployment)
		want   string
	}{
		"bolt store":          {func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Env[1].Value = "bolt" }, "want secret"},
		"another link secret": {func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Env[2].Value = "some-other-secret" }, "want " + klausGatewayLinksSecret},
		"store volume": {func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Volumes = append(d.Spec.Template.Spec.Volumes, corev1.Volume{Name: oboStoreVolume})
		}, "Multi-Attach"},
		"recreate":     {func(d *appsv1.Deployment) { d.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType }, "Recreate"},
		"no container": {func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Name = "sidecar" }, "no container " + klausGatewayComponent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := storeDeployment()
			tc.mutate(d)
			if err := assertStoreDeployment(d); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
	if got := strategyName(storeDeployment()); got != "RollingUpdate" {
		t.Errorf("an unset strategy reads as %q, want RollingUpdate", got)
	}
}

// linksRole is the chart's Role on the link Secret.
func linksRole() *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: klausGatewayLinksSecret, Namespace: platformNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{klausGatewayLinksSecret}, Verbs: slices.Clone(linkSecretVerbs),
		}},
	}
}

// TestAssertLinksRole: exactly the link Secret's verbs on exactly that
// Secret; a wider grant is refused naming what widened.
func TestAssertLinksRole(t *testing.T) {
	if err := assertLinksRole(linksRole()); err != nil {
		t.Fatalf("the chart's Role: %v", err)
	}
	cases := map[string]struct {
		mutate func(*rbacv1.Role)
		want   string
	}{
		"create granted":       {func(r *rbacv1.Role) { r.Rules[0].Verbs = append(r.Rules[0].Verbs, "create") }, "no create"},
		"no resourceNames":     {func(r *rbacv1.Role) { r.Rules[0].ResourceNames = nil }, "resourceNames"},
		"another secret too":   {func(r *rbacv1.Role) { r.Rules[0].ResourceNames = append(r.Rules[0].ResourceNames, "one-more-secret") }, "resourceNames"},
		"configmaps":           {func(r *rbacv1.Role) { r.Rules[0].Resources = []string{"configmaps"} }, "secrets"},
		"a second rule":        {func(r *rbacv1.Role) { r.Rules = append(r.Rules, rbacv1.PolicyRule{}) }, "2 rules"},
		"verbs in other order": {func(r *rbacv1.Role) { r.Rules[0].Verbs = []string{linkVerbPatch, linkVerbGet, linkVerbUpdate} }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := linksRole()
			tc.mutate(r)
			err := assertLinksRole(r)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestAssertLinksRoleBinding: the gateway's ServiceAccount bound to the link
// Role; another subject or Role is refused.
func TestAssertLinksRoleBinding(t *testing.T) {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: klausGatewayLinksSecret, Namespace: platformNamespace},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: klausGatewayLinksSecret},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: klausGatewayComponent, Namespace: platformNamespace}},
	}
	if err := assertLinksRoleBinding(rb, klausGatewayComponent); err != nil {
		t.Fatalf("the chart's RoleBinding: %v", err)
	}
	if err := assertLinksRoleBinding(rb, "other-sa"); err == nil || !strings.Contains(err.Error(), "other-sa") {
		t.Fatalf("another ServiceAccount must be refused naming it, got %v", err)
	}
	rb.RoleRef.Name = "admin"
	if err := assertLinksRoleBinding(rb, klausGatewayComponent); err == nil || !strings.Contains(err.Error(), "want Role "+klausGatewayLinksSecret) {
		t.Fatalf("another Role must be refused, got %v", err)
	}
}

// TestStoreReadyLinks: the count off the gateway's record, a pod prefix
// tolerated, another backend or a record without the count refused.
func TestStoreReadyLinks(t *testing.T) {
	logs := `{"time":"2026-09-14T12:00:00Z","level":"INFO","msg":"klaus-gateway starting","version":"1.5.0","git_sha":"8623ed4"}
klaus-gateway-abc {"time":"2026-09-14T12:00:01Z","level":"INFO","msg":"obo link store ready","backend":"secret","secret":"agent-platform/klaus-gateway-obo-links","links":2}
{"time":"2026-09-14T12:00:02Z","level":"INFO","msg":"web adapter started"}`
	if got, err := storeReadyLinks(logs); err != nil || got != 2 {
		t.Fatalf("links = %d, %v; want 2", got, err)
	}
	if got := gatewayVersion(logs); got != "klaus-gateway 1.5.0 (8623ed4)" {
		t.Errorf("version %q", got)
	}
	if _, err := storeReadyLinks(`{"msg":"web adapter started"}`); err == nil || !strings.Contains(err.Error(), "no") {
		t.Fatalf("no record: %v", err)
	}
	if _, err := storeReadyLinks(`{"msg":"obo link store ready","backend":"bolt"}`); err == nil || !strings.Contains(err.Error(), "links count") {
		t.Fatalf("no count: %v", err)
	}
	if _, err := storeReadyLinks(`{"msg":"obo link store ready","backend":"bolt","links":1}`); err == nil || !strings.Contains(err.Error(), "bolt") {
		t.Fatalf("another backend: %v", err)
	}
}

// TestProofLinks: two records under the proof's prefix and the run's suffix,
// the person's identity in the first; read back they compare equal, a
// changed field does not.
func TestProofLinks(t *testing.T) {
	user := config.Default().AdminUser()
	links := proofLinks(user, "r1")
	if len(links) != 2 {
		t.Fatalf("%d links", len(links))
	}
	first, ok := links[klausGatewayLinkPrefix+"r1-1"]
	if !ok || first.Email != user.Email || first.Sub != "agentlab:"+user.Username || first.RefreshToken == "" {
		t.Fatalf("first link %+v (%v)", first, ok)
	}
	for id := range links {
		if !strings.HasPrefix(id, klausGatewayLinkPrefix) {
			t.Errorf("id %q lacks the proof's prefix", id)
		}
	}
	mem := musterlink.NewMemStore()
	for id, l := range links {
		mem.Put(id, l)
	}
	if err := readBackLinks(mem, links); err != nil {
		t.Fatalf("read back from a store holding them: %v", err)
	}
	changed := &musterlink.Link{Sub: first.Sub, Email: first.Email, RefreshToken: "rotated", LinkedAt: first.LinkedAt}
	if sameLink(first, changed) {
		t.Error("a rotated refresh token must not compare equal")
	}
	mem.Put(klausGatewayLinkPrefix+"r1-1", changed)
	if err := readBackLinks(mem, links); err == nil || !strings.Contains(err.Error(), "read back differently") {
		t.Fatalf("a changed record must be reported, got %v", err)
	}
	mem.Put(klausGatewayLinkPrefix+"r1-1", first)
	mem.Delete(klausGatewayLinkPrefix + "r1-2")
	if err := readBackLinks(mem, links); err == nil || !strings.Contains(err.Error(), "not in the store") {
		t.Fatalf("a missing record must be reported, got %v", err)
	}
	if !first.LinkedAt.Equal(first.LinkedAt.Truncate(time.Second)) {
		t.Error("LinkedAt is truncated to the second so the JSON round trip compares equal")
	}
}

// TestPodReady: Running with Ready True, and nothing else.
func TestPodReady(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if !podReady(pod) {
		t.Fatal("a Running, Ready pod")
	}
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if podReady(pod) {
		t.Fatal("Ready False")
	}
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	if podReady(pod) {
		t.Fatal("a terminating pod is not the replacement")
	}
}
