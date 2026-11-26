package lab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab runs organizations on the Organization kind (security.giantswarm.io),
// which only the draft pull requests of giantswarm/giantswarm#39492 carry: the
// meta chart of giantswarm/agent-platform#1489, and agent-platform-operator and
// agent-manager as dev builds through a platform.valuesFiles overlay
// (docs/agents.md "The Organization kind prototype"). The two checks below stop
// a lab without them at once and name what is missing, in place of Helm's
// schema error or an organization that never becomes Ready.
const (
	// organizationKindDocs is the docs section that sets up the prototype.
	organizationKindDocs = `docs/agents.md "The Organization kind prototype"`
	// valuesOrganizations is the meta chart's key of the organizations, and
	// valuesOrganizationCRD its switch of the Organization CRD beneath it,
	// which the lab renders on (agent-platform-values.yaml.tmpl).
	valuesOrganizations   = "agentPlatformOrganizations"
	valuesOrganizationCRD = "organizationCRD"
	// agentPlatformOperator is the operator's component: its Deployment and
	// its ServiceAccount in the platform namespace.
	agentPlatformOperator = "agent-platform-operator"
	// organizationReadVerb is the read that each component of
	// organizationKindComponents needs on the Organizations: the operator's
	// informer lists them, and so does agent-manager's list_organizations.
	organizationReadVerb = "list"
	// defaultServiceAccount is the ServiceAccount of a pod whose template
	// names none.
	defaultServiceAccount = "default"
)

// organizationKindComponents are the components that read the Organizations:
// Deployments of the platform namespace, by the meta chart's component names.
var organizationKindComponents = []string{agentPlatformOperator, agentManagerMCPServer}

// checkOrganizationsChart refuses platform.organizations on a chart whose
// values schema has no agentPlatformOrganizations.organizationCRD, before
// anything renders: the lab renders that switch with the organizations, and
// the chart's schema would refuse it at the install with Helm's words only.
// The schema comes from the chart that the lab loads anyway
// (platformChartValuesOf). A chart that cannot be loaded, or that has no
// schema, is left to the render, which reports a real refusal itself.
func checkOrganizationsChart(cfg *config.Config) error {
	if !cfg.OrganizationsEnabled() {
		return nil
	}
	chart := platformChartFor(cfg)
	v, err := platformChartValuesOf(chart)
	if err != nil || len(v.schema) == 0 || v.carries(valuesOrganizations, valuesOrganizationCRD) {
		return nil
	}
	return fmt.Errorf("platform.organizations is on, but %s has no %s.%s in its values schema, so it would refuse the values the lab renders: "+
		"the lab runs organizations on the Organization kind, which no released chart carries yet. "+
		"Install the prototype's chart (%s), or turn organizations off: `agentlab configure --organizations=false`",
		chart, valuesOrganizations, valuesOrganizationCRD, organizationKindDocs)
}

// componentAccess is what one component's ServiceAccount may do on the
// Organizations.
type componentAccess struct {
	// component is the Deployment's name, image its first container's image.
	component, image string
	// namespace and serviceAccount are the ServiceAccount its pods run as.
	namespace, serviceAccount string
	// mayRead reports whether that ServiceAccount may read the Organizations
	// (organizationReadVerb, cluster-wide).
	mayRead bool
}

// componentAccessOf reads a component's Deployment: the ServiceAccount its
// pods run as (the namespace's default when the template names none) and
// its image, which names the release.
func componentAccessOf(d *appsv1.Deployment) componentAccess {
	c := componentAccess{component: d.Name, namespace: d.Namespace, serviceAccount: d.Spec.Template.Spec.ServiceAccountName}
	if c.serviceAccount == "" {
		c.serviceAccount = defaultServiceAccount
	}
	if containers := d.Spec.Template.Spec.Containers; len(containers) > 0 {
		c.image = containers[0].Image
	}
	return c
}

// user is the user name that the component's ServiceAccount authenticates as.
func (c componentAccess) user() string {
	return "system:serviceaccount:" + c.namespace + ":" + c.serviceAccount
}

// organizationKindProblem words the components whose ServiceAccount may not
// read the Organizations, with the overlay that installs the builds that may;
// "" when every component may.
func organizationKindProblem(access []componentAccess) string {
	var denied []string
	for _, c := range access {
		if !c.mayRead {
			denied = append(denied, fmt.Sprintf("%s (image %s): ServiceAccount %s/%s", c.component, c.image, c.namespace, c.serviceAccount))
		}
	}
	if len(denied) == 0 {
		return ""
	}
	return fmt.Sprintf("%s and %s must serve the Organization kind, but these may not %s %s:\n\n%s\n\n"+
		"Their released charts carry no RBAC on the Organization kind; the prototype's dev builds do, chart and image together. "+
		"Point both components at those builds with the platform.valuesFiles overlay of %s "+
		"(components.%s.versionRange and components.%s.versionRange), then `agentlab platform`",
		agentPlatformOperator, agentManagerMCPServer, organizationReadVerb, organizationResource,
		indent(strings.Join(denied, "\n"), "  "), organizationKindDocs, agentPlatformOperator, agentManagerMCPServer)
}

// checkOrganizationKindServed fails, naming what is missing, when the
// apiserver serves no Organization kind, or when agent-platform-operator or
// agent-manager may not read the Organizations as their ServiceAccounts (a
// SelfSubjectAccessReview as each, canI): a released component of the
// chart's range reconciles another kind, so an organization would never be
// Ready, and agent-manager's organization tools would find none.
func checkOrganizationKindServed() error {
	if _, err := gvrFor(organizationResource); err != nil {
		return fmt.Errorf("the apiserver serves no %s: the installed chart does not install the Organization CRD (%s.%s.enabled, giantswarm/agent-platform#1489) — %s: %w",
			organizationResource, valuesOrganizations, valuesOrganizationCRD, organizationKindDocs, err)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	access := make([]componentAccess, 0, len(organizationKindComponents))
	for _, name := range organizationKindComponents {
		d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading Deployment %s/%s, which serves the Organization kind: %w", platformNamespace, name, err)
		}
		c := componentAccessOf(d)
		as, err := asUserConfig(c.user())
		if err != nil {
			return err
		}
		if c.mayRead, err = canI(ctx, as, organizationReadVerb, organizationResource, ""); err != nil {
			return fmt.Errorf("whether %s may %s %s: %w", c.user(), organizationReadVerb, organizationResource, err)
		}
		access = append(access, c)
	}
	if problem := organizationKindProblem(access); problem != "" {
		return errors.New(problem)
	}
	return nil
}
