package lab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab runs organizations on the Organization kind (security.giantswarm.io),
// in the shape agreed on giantswarm/organization-operator#673
// (spec.capabilities.agentPlatform, the agentPlatform entry of
// status.namespaces, the AgentPlatform conditions), which only the draft pull
// requests of giantswarm/giantswarm#39492 carry: the meta chart of
// giantswarm/agent-platform#1489, and agent-platform-operator and
// agent-manager as dev builds through a platform.valuesFiles overlay
// (docs/agents.md "The Organization kind prototype"). The two checks below stop
// a lab without them, or with a build of the earlier shape, at once and name
// what is missing, in place of Helm's schema error or an organization that
// never becomes Ready.
const (
	// organizationKindDocs is the docs section that sets up the prototype.
	organizationKindDocs = `docs/agents.md "The Organization kind prototype"`
	// organizationShapeContract is where the Organization's shape that the
	// lab reads and writes was agreed; organizationKindChart is the chart
	// pull request that installs its CRD, from organizationKindChartBranch.
	organizationShapeContract   = "giantswarm/organization-operator#673"
	organizationKindChart       = "giantswarm/agent-platform#1489"
	organizationKindChartBranch = "organization-kind-chart"
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

// organizationShapeFloors are the releases from which each component reads
// and writes the shape of organizationShapeContract: the dev builds of
// giantswarm/agent-platform-operator#69 from dee58c4
// (0.4.9-r1b5e09bdt20261126140120hdee58c4) and of giantswarm/agent-manager#475
// from 6f0d54a (3.5.11-r1b5e09bdt20261126135049h6f0d54a). Their earlier
// builds, which hold the RBAC on the Organizations too, read
// spec.agentPlatform and status.agentPlatform.
var organizationShapeFloors = map[string]*semver.Version{
	agentPlatformOperator: semver.MustParse("0.4.9"),
	agentManagerMCPServer: semver.MustParse("3.5.11"),
}

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

// earlierShape is the component's floor of organizationShapeFloors when its
// image is a build from before it: a tag that is a semantic version whose
// release (the prerelease of a dev build left out, so that every dev build of
// the floor's release passes) is below the floor. A tag that is no semantic
// version (a local build) is left to the run, nil.
func (c componentAccess) earlierShape() *semver.Version {
	floor, ok := organizationShapeFloors[c.component]
	if !ok {
		return nil
	}
	ref, _, _ := strings.Cut(c.image, "@")
	_, tag, ok := strings.Cut(ref[strings.LastIndex(ref, "/")+1:], ":")
	if !ok {
		return nil
	}
	v, err := semver.StrictNewVersion(strings.TrimPrefix(tag, "v"))
	if err != nil {
		return nil
	}
	if release := semver.New(v.Major(), v.Minor(), v.Patch(), "", ""); release.LessThan(floor) {
		return floor
	}
	return nil
}

// organizationKindProblem words the components whose ServiceAccount may not
// read the Organizations, and those whose build reads the earlier shape, with
// the overlay that installs the builds that serve the shape; "" when every
// component does.
func organizationKindProblem(access []componentAccess) string {
	var problems []string
	for _, c := range access {
		switch floor := c.earlierShape(); {
		case !c.mayRead:
			problems = append(problems, fmt.Sprintf("%s (image %s): ServiceAccount %s/%s may not %s %s",
				c.component, c.image, c.namespace, c.serviceAccount, organizationReadVerb, organizationResource))
		case floor != nil:
			problems = append(problems, fmt.Sprintf("%s (image %s): a build before %s, which reads the earlier shape (spec.agentPlatform, status.agentPlatform, the conditions Ready and Orphaned)",
				c.component, c.image, floor))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return fmt.Sprintf("%s and %s must serve the Organization kind in the shape of %s (spec.capabilities.agentPlatform, the agentPlatform entry of status.namespaces, the AgentPlatform conditions), but:\n\n%s\n\n"+
		"Their released charts carry no RBAC on the Organization kind, and their earlier dev builds read the earlier shape; the prototype's current dev builds serve it, chart and image together. "+
		"Point both components at those builds with the platform.valuesFiles overlay of %s "+
		"(components.%s.versionRange and components.%s.versionRange), then `agentlab platform`",
		agentPlatformOperator, agentManagerMCPServer, organizationShapeContract,
		indent(strings.Join(problems, "\n"), "  "), organizationKindDocs, agentPlatformOperator, agentManagerMCPServer)
}

// organizationCRDShapeProblem words why the served Organization CRD does not
// carry the shape of organizationShapeContract: the schema of the lab's
// version names the properties of spec, and spec.capabilities.agentPlatform
// is not among them. "" when it is, and when the schema cannot tell (no such
// version, no properties of spec).
func organizationCRDShapeProblem(crd *unstructured.Unstructured) string {
	_, version, _ := strings.Cut(organizationAPIVersion, "/")
	versions, _, _ := unstructured.NestedSlice(crd.Object, crSpec, "versions")
	for _, v := range versions {
		served, ok := v.(map[string]any)
		if !ok || served[nameKey] != version {
			continue
		}
		raw, found, _ := unstructured.NestedFieldNoCopy(served, "schema", "openAPIV3Schema", "properties", crSpec, "properties")
		spec, ok := raw.(map[string]any)
		if !found || !ok {
			return ""
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(spec, organizationSpecCapabilitiesKey, "properties", organizationCapabilityAgentPlatform); found {
			return ""
		}
		has := "no spec.capabilities.agentPlatform"
		if _, earlier := spec[organizationCapabilityAgentPlatform]; earlier {
			has = "the earlier shape, spec.agentPlatform"
		}
		return fmt.Sprintf("the installed Organization CRD (%s %s) has %s, but the lab writes spec.capabilities.agentPlatform and reads the agentPlatform entry of status.namespaces and the AgentPlatform conditions, the shape of %s. "+
			"Install the chart of branch %s at its current head (%s), then `agentlab platform` — %s",
			organizationResource, version, has, organizationShapeContract, organizationKindChartBranch, organizationKindChart, organizationKindDocs)
	}
	return ""
}

// checkOrganizationKindServed fails, naming what is missing, when the
// apiserver serves no Organization kind or one of the earlier shape, or when
// agent-platform-operator or agent-manager may not read the Organizations as
// their ServiceAccounts (a SelfSubjectAccessReview as each, canI) or is a
// build of the earlier shape: a released component of the chart's range
// reconciles another kind, and an earlier build reads other fields, so an
// organization would never be Ready, and agent-manager's organization tools
// would find none.
func checkOrganizationKindServed() error {
	if _, err := gvrFor(organizationResource); err != nil {
		return fmt.Errorf("the apiserver serves no %s: the installed chart does not install the Organization CRD (%s.%s.enabled, %s) — %s: %w",
			organizationResource, valuesOrganizations, valuesOrganizationCRD, organizationKindChart, organizationKindDocs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	crd, err := getObject(ctx, gvrCRDs, "", organizationResource)
	if err != nil {
		return err
	}
	if problem := organizationCRDShapeProblem(crd); problem != "" {
		return errors.New(problem)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
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
