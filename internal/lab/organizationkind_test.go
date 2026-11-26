package lab

import (
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// A chart whose schema has no agentPlatformOrganizations.organizationCRD is
// refused with organizations on, naming the prototype's docs and the switch
// that turns organizations off; the prototype's chart, organizations off, and
// a chart the lab cannot read pass to the render.
func TestCheckOrganizationsChart(t *testing.T) {
	// The released charts' block: closed, with no CRD switch.
	released := []byte(`{"properties":{"agentPlatformOrganizations":{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"}}}}}`)
	prototype := []byte(`{"properties":{"agentPlatformOrganizations":{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"organizationCRD":{"type":"object"}}}}}`)
	for _, tc := range []struct {
		name          string
		schema        []byte
		loadErr       error
		organizations bool
		refused       bool
	}{
		{name: "a released chart", schema: released, organizations: true, refused: true},
		{name: "the prototype's chart", schema: prototype, organizations: true},
		{name: "organizations off", schema: released},
		{name: "a chart that cannot be read", loadErr: errors.New("registry unreachable"), organizations: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubPlatformChartValues(t, platformChartValues{schema: tc.schema}, tc.loadErr)
			cfg := config.Default()
			cfg.Platform.Organizations.Enabled = tc.organizations
			err := checkOrganizationsChart(cfg)
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("not refused")
			}
			for _, want := range []string{`docs/agents.md "The Organization kind prototype"`, "agentlab configure --organizations=false"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q:\n%s", want, err)
				}
			}
		})
	}
}

// The check reads each component's Deployment for the ServiceAccount it
// reviews and the image it names, and the problem names each component whose
// ServiceAccount may not read the Organizations, its image, and the overlay
// of the prototype's docs; nothing when both may.
func TestOrganizationKindProblem(t *testing.T) {
	deployment := func(name, serviceAccount, image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				ServiceAccountName: serviceAccount,
				Containers:         []corev1.Container{{Name: name, Image: image}},
			}}},
		}
	}
	operator := componentAccessOf(deployment(agentPlatformOperator, agentPlatformOperator, "gsoci.azurecr.io/giantswarm/agent-platform-operator:0.4.3"))
	manager := componentAccessOf(deployment(agentManagerMCPServer, "", "gsoci.azurecr.io/giantswarm/agent-manager:3.5.2"))
	if got, want := manager.user(), "system:serviceaccount:"+platformNamespace+":default"; got != want {
		t.Errorf("a template without a ServiceAccount is reviewed as %q, want the namespace's default %q", got, want)
	}
	for _, tc := range []struct {
		name               string
		operator, manager  bool
		named, notNamed    []string
		wantNoProblemAtAll bool
	}{
		{name: "both may read", operator: true, manager: true, wantNoProblemAtAll: true},
		{name: "the operator may not", manager: true,
			named:    []string{platformNamespace + "/" + agentPlatformOperator, "agent-platform-operator:0.4.3"},
			notNamed: []string{platformNamespace + "/default", "agent-manager:3.5.2"}},
		{name: "agent-manager may not", operator: true,
			named:    []string{platformNamespace + "/default", "agent-manager:3.5.2"},
			notNamed: []string{platformNamespace + "/" + agentPlatformOperator, "agent-platform-operator:0.4.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, m := operator, manager
			o.mayRead, m.mayRead = tc.operator, tc.manager
			problem := organizationKindProblem([]componentAccess{o, m})
			if tc.wantNoProblemAtAll {
				if problem != "" {
					t.Fatalf("a problem where both may read:\n%s", problem)
				}
				return
			}
			for _, want := range append([]string{"platform.valuesFiles", `docs/agents.md "The Organization kind prototype"`}, tc.named...) {
				if !strings.Contains(problem, want) {
					t.Errorf("the problem does not name %q:\n%s", want, problem)
				}
			}
			for _, never := range tc.notNamed {
				if strings.Contains(problem, never) {
					t.Errorf("the problem names %q, which may read:\n%s", never, problem)
				}
			}
		})
	}
}
