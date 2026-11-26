package lab

import (
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
// ServiceAccount may not read the Organizations, or whose image is no build
// of the shape of giantswarm/organization-operator#673 (an earlier build of
// the prototype's branch, on the same release line too, or a release), its
// image, and the overlay of the prototype's docs; nothing when both may read
// and serve the shape, and nothing for a tag that cannot tell.
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
	const (
		// The first dev builds of the shape, and builds of the prototype's
		// branch before them: the operator's on the same release line.
		operatorImage        = "gsoci.azurecr.io/giantswarm/agent-platform-operator:0.4.9-r1b5e09bdt20261126140120hdee58c4"
		managerImage         = "gsoci.azurecr.io/giantswarm/agent-manager:3.5.11-r1b5e09bdt20261126135049h6f0d54a"
		earlierOperatorImage = "gsoci.azurecr.io/giantswarm/agent-platform-operator:0.4.9-r1b5e09bdt20261126115358h34fb3f4"
		earlierManagerImage  = "gsoci.azurecr.io/giantswarm/agent-manager:3.5.9-r1b5e09bdt20261125120439h31a58c6"
		releasedManagerImage = "gsoci.azurecr.io/giantswarm/agent-manager:3.5.12"
	)
	operator := componentAccessOf(deployment(agentPlatformOperator, agentPlatformOperator, operatorImage))
	manager := componentAccessOf(deployment(agentManagerMCPServer, "", managerImage))
	if got, want := manager.user(), "system:serviceaccount:"+platformNamespace+":default"; got != want {
		t.Errorf("a template without a ServiceAccount is reviewed as %q, want the namespace's default %q", got, want)
	}
	for _, tc := range []struct {
		name                        string
		operator, manager           bool
		operatorImage, managerImage string
		named, notNamed             []string
		wantNoProblemAtAll          bool
	}{
		{name: "both may read", operator: true, manager: true, wantNoProblemAtAll: true},
		{name: "the operator may not", manager: true,
			named:    []string{platformNamespace + "/" + agentPlatformOperator, operatorImage},
			notNamed: []string{platformNamespace + "/default", managerImage}},
		{name: "agent-manager may not", operator: true,
			named:    []string{platformNamespace + "/default", managerImage},
			notNamed: []string{platformNamespace + "/" + agentPlatformOperator, operatorImage}},
		{name: "an earlier build of the operator", operator: true, manager: true, operatorImage: earlierOperatorImage,
			named:    []string{earlierOperatorImage, "before 0.4.9-r1b5e09bdt20261126140120hdee58c4", earlierSpecPath},
			notNamed: []string{managerImage}},
		{name: "an earlier build of agent-manager", operator: true, manager: true, managerImage: earlierManagerImage,
			named:    []string{earlierManagerImage, "before 3.5.11-r1b5e09bdt20261126135049h6f0d54a", earlierSpecPath},
			notNamed: []string{operatorImage}},
		{name: "a release of agent-manager, which may read", operator: true, manager: true, managerImage: releasedManagerImage,
			named:    []string{releasedManagerImage, "no release serves the shape"},
			notNamed: []string{operatorImage}},
		{name: "another branch's build and a local build are left to the run", operator: true, manager: true, wantNoProblemAtAll: true,
			operatorImage: "gsoci.azurecr.io/giantswarm/agent-platform-operator:0.4.9-r89373ccct20261126135429h3109993", managerImage: "localhost/agent-manager:dev-1a2b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, m := operator, manager
			o.mayRead, m.mayRead = tc.operator, tc.manager
			if tc.operatorImage != "" {
				o.image = tc.operatorImage
			}
			if tc.managerImage != "" {
				m.image = tc.managerImage
			}
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

// earlierSpecPath is the Agent Platform's block in the Organization's shape
// before giantswarm/organization-operator#673.
const earlierSpecPath = "spec.agentPlatform"

// The served Organization CRD must carry the shape of
// giantswarm/organization-operator#673: one with spec.agentPlatform, the
// earlier shape, is refused naming the chart branch that installs the
// current one; the current shape, and a schema that cannot tell, pass.
func TestOrganizationCRDShapeProblem(t *testing.T) {
	// The lab's version of the Organization kind.
	const version = "v1alpha1"
	properties := func(p map[string]any) map[string]any { return map[string]any{"properties": p} }
	crd := func(specProperties map[string]any) *unstructured.Unstructured {
		schema := map[string]any{}
		if specProperties != nil {
			schema = properties(map[string]any{crSpec: properties(specProperties)})
		}
		return &unstructured.Unstructured{Object: map[string]any{crSpec: map[string]any{
			"versions": []any{map[string]any{nameKey: version, "served": true, "schema": map[string]any{"openAPIV3Schema": schema}}},
		}}}
	}
	block := properties(map[string]any{})
	for name, tc := range map[string]struct {
		crd   *unstructured.Unstructured
		named []string
	}{
		"the current shape": {crd: crd(map[string]any{organizationSpecCapabilitiesKey: properties(map[string]any{organizationCapabilityAgentPlatform: block})})},
		"the earlier shape": {crd: crd(map[string]any{organizationCapabilityAgentPlatform: block}),
			named: []string{earlierSpecPath, "giantswarm/organization-operator#673", "organization-kind-chart", "giantswarm/agent-platform#1489", organizationKindDocs}},
		"no Agent Platform at all":         {crd: crd(map[string]any{}), named: []string{"no spec.capabilities.agentPlatform", "organization-kind-chart"}},
		"a schema without spec properties": {crd: crd(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			problem := organizationCRDShapeProblem(tc.crd)
			if len(tc.named) == 0 {
				if problem != "" {
					t.Fatalf("refused:\n%s", problem)
				}
				return
			}
			for _, want := range tc.named {
				if !strings.Contains(problem, want) {
					t.Errorf("the problem does not name %q:\n%s", want, problem)
				}
			}
		})
	}
}
