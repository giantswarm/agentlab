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
		Image: KlausGatewayImageDefault,
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

// TestProofLinks: two records under the proof's prefix, the person's (the
// cached id_token and its expiry) filed under the Slack user the in-cluster
// turn is sent as; read back they compare equal, a changed field does not.
func TestProofLinks(t *testing.T) {
	user := config.Default().AdminUser()
	id := linkedIdentity{subject: testSubject, expiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	person := slackUserPrefix + "R1C"
	links := proofLinks(id.link(user.Email, "id-token"), person)
	if len(links) != 2 {
		t.Fatalf("%d links", len(links))
	}
	first, ok := links[person]
	if !ok || first.Email != user.Email || first.Sub != testSubject || first.IDToken != "id-token" || !first.Expiry.Equal(id.expiry) {
		t.Fatalf("the person's link %+v (%v)", first, ok)
	}
	for id, l := range links {
		if !strings.HasPrefix(id, slackUserPrefix) || l.RefreshToken == "" {
			t.Errorf("link %q: %+v", id, l)
		}
	}
	mem := musterlink.NewMemStore()
	for id, l := range links {
		if err := mem.Put(id, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := readBackLinks(mem, links); err != nil {
		t.Fatalf("read back from a store holding them: %v", err)
	}
	for name, changed := range map[string]*musterlink.Link{
		"a rotated refresh token": {Sub: first.Sub, Email: first.Email, RefreshToken: "rotated", LinkedAt: first.LinkedAt, IDToken: first.IDToken, Expiry: first.Expiry},
		"a refreshed id_token":    {Sub: first.Sub, Email: first.Email, RefreshToken: first.RefreshToken, LinkedAt: first.LinkedAt, IDToken: "new", Expiry: first.Expiry.Add(time.Hour)},
	} {
		if sameLink(first, changed) {
			t.Errorf("%s must not compare equal", name)
		}
	}
	if err := mem.Put(person, &musterlink.Link{Sub: first.Sub, Email: first.Email, RefreshToken: "rotated", LinkedAt: first.LinkedAt}); err != nil {
		t.Fatal(err)
	}
	if err := readBackLinks(mem, links); err == nil || !strings.Contains(err.Error(), "read back differently") {
		t.Fatalf("a changed record must be reported, got %v", err)
	}
	if err := mem.Put(person, first); err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(person + "D"); err != nil {
		t.Fatal(err)
	}
	if err := readBackLinks(mem, links); err == nil || !strings.Contains(err.Error(), "not in the store") {
		t.Fatalf("a missing record must be reported, got %v", err)
	}
	if !first.LinkedAt.Equal(first.LinkedAt.Truncate(time.Second)) {
		t.Error("LinkedAt is truncated to the second so the JSON round trip compares equal")
	}
}

// TestAssertFakeSlackAPI: the component must call the Slack Web API through
// the lab's Service; a Deployment without the lab's patch is told to re-run
// `agentlab platform`, and the patch sets exactly that variable.
func TestAssertFakeSlackAPI(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: klausGatewayComponent}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: klausGatewayComponent}}
	if err := assertFakeSlackAPI(d); err == nil || !strings.Contains(err.Error(), "agentlab platform") {
		t.Errorf("unpatched: %v", err)
	}
	d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: slackAPIBaseEnv, Value: klausGatewaySlackAPIBase}}
	if err := assertFakeSlackAPI(d); err != nil {
		t.Error(err)
	}
	patch := string(slackAPIPatch().Patch)
	if !strings.Contains(patch, "name: "+slackAPIBaseEnv) || !strings.Contains(patch, "value: "+klausGatewaySlackAPIBase) || slackAPIPatch().Target[nameKey] != klausGatewayComponent {
		t.Errorf("patch = %s", patch)
	}
	if klausGatewaySlackAPIBase != "http://agentlab-slack-api.agent-platform.svc.cluster.local/api" {
		t.Errorf("base = %s", klausGatewaySlackAPIBase)
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
