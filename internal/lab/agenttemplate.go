package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// kagent API v2 (api.kagent.dev/v1alpha3) as the proofs read it.
//
// An agent's AgentTemplate in the kagent namespace — model, prompt, skills,
// tool bindings — is the portable half; the Agent named after it pairs the
// template (spec.templateRef) with the Harness that runs it (spec.harnessRef,
// the platform's Go ADK Harness kagentHarness). The controller compiles the
// Agent into a golden snapshot per revision; its readiness is the Agent's
// status: the conditions Accepted, ResolvedRefs, Compatible and Ready, the
// desired revision and the last one whose golden snapshot succeeded. The
// template's tools are whole RemoteMCPServers of the same namespace: the
// agent's own, named after it, carrying the toolset header (agent.go).
// Nothing here pins an API version: the resources resolve through discovery
// (gvrFor).

const (
	// kagentAPIGroup is the group of kagent's CRDs; the resource arguments
	// below are fully qualified with it so a same-named kind in another
	// group can never be meant.
	kagentAPIGroup          = "api.kagent.dev"
	agentTemplateResource   = "agenttemplates." + kagentAPIGroup
	agentResource           = "agents." + kagentAPIGroup
	harnessResource         = "harnesses." + kagentAPIGroup
	remoteMCPServerResource = "remotemcpservers." + kagentAPIGroup
	// kagentAPIVersion is the apiVersion of the objects the proofs write
	// themselves (the skills proof's AgentTemplate and Agent); a manifest
	// names its version, the reads never pin one.
	kagentAPIVersion        = kagentAPIGroup + "/v1alpha3"
	agentTemplateAPIVersion = kagentAPIVersion
	// remoteMCPServerKind is the kind an AgentTemplate's MCP tool binding names.
	remoteMCPServerKind = "RemoteMCPServer"
	// defaultModelConfig is the ModelConfig the kagent chart renders from
	// providers.<default>, referencing the Secret `agentlab up` fills
	// (anthropic.go): the throwaway agents' default.
	defaultModelConfig = "default-model-config"
)

// agentTemplate is the part of a kagent AgentTemplate the proofs read.
type agentTemplate struct {
	Metadata struct {
		Name        string            `json:"name"`
		Generation  int64             `json:"generation"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Description string `json:"description"`
		ModelConfig *struct {
			Name string `json:"name"`
		} `json:"modelConfig"`
		SystemPrompt string          `json:"systemPrompt"`
		Skills       []templateSkill `json:"skills"`
		Tools        []struct {
			MCP *struct {
				Server struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"server"`
				Tools           []string `json:"tools"`
				RequireApproval bool     `json:"requireApproval"`
			} `json:"mcp"`
		} `json:"tools"`
	} `json:"spec"`
}

// templateSkill is one spec.skills[] entry as the chart renders it:
// {name, source: {git: {url, commit} | oci, path}}.
type templateSkill struct {
	Name   string `json:"name"`
	Source struct {
		Git *struct {
			URL    string `json:"url"`
			Commit string `json:"commit"`
		} `json:"git"`
		OCI  string `json:"oci"`
		Path string `json:"path"`
	} `json:"source"`
}

// agentObject is the part of a kagent Agent the proofs read: the template
// and the Harness it pairs (a reference each; an inline spec reads as no
// reference) and the controller's status.
type agentObject struct {
	Metadata struct {
		Name        string            `json:"name"`
		Generation  int64             `json:"generation"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		TemplateRef *struct {
			Name string `json:"name"`
		} `json:"templateRef"`
		HarnessRef *struct {
			Name string `json:"name"`
		} `json:"harnessRef"`
		Template map[string]any `json:"template"`
		Harness  map[string]any `json:"harness"`
	} `json:"spec"`
	Status agentStatus `json:"status"`
}

// agentStatus is the controller's report on an Agent: compilation and
// preparation as conditions, the revision the controller wants and the last
// one whose golden snapshot succeeded, and compile warnings.
type agentStatus struct {
	ObservedGeneration       int64               `json:"observedGeneration"`
	DesiredRevision          string              `json:"desiredRevision"`
	LatestSuccessfulRevision string              `json:"latestSuccessfulRevision"`
	Warnings                 []string            `json:"warnings"`
	Conditions               []templateCondition `json:"conditions"`
}

// templateCondition is one condition of an Agent's status: Accepted,
// ResolvedRefs, Compatible, Ready — and of a HelmRelease's status.
type templateCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// String is the condition the way the evidence quotes it:
// `Ready=False ActorTemplatePending: waiting for the ActorTemplate golden snapshot`.
func (c templateCondition) String() string {
	return fmt.Sprintf("%s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message)
}

// templateName is the AgentTemplate the Agent references, "" for a template
// written inline.
func (a *agentObject) templateName() string {
	if a.Spec.TemplateRef == nil {
		return ""
	}
	return a.Spec.TemplateRef.Name
}

// harnessName is the Harness the Agent references, "" for a Harness written
// inline.
func (a *agentObject) harnessName() string {
	if a.Spec.HarnessRef == nil {
		return ""
	}
	return a.Spec.HarnessRef.Name
}

// condition is one condition's status ("True", "False", "Unknown", or ""
// while it is not there yet) and message.
func (s *agentStatus) condition(condType string) (status, message string) {
	for _, c := range s.Conditions {
		if c.Type == condType {
			return c.Status, c.Message
		}
	}
	return "", ""
}

// mcpServer is the RemoteMCPServer the template's first MCP tool binding
// names, "" when the template binds no MCP server at all.
func (t *agentTemplate) mcpServer() string {
	for _, tool := range t.Spec.Tools {
		if tool.MCP != nil {
			return tool.MCP.Server.Name
		}
	}
	return ""
}

// skill is the template's skill of that name, nil when it carries none.
func (t *agentTemplate) skill(name string) *templateSkill {
	for i := range t.Spec.Skills {
		if t.Spec.Skills[i].Name == name {
			return &t.Spec.Skills[i]
		}
	}
	return nil
}

// requiresApproval reports whether the template's binding of the named
// RemoteMCPServer carries requireApproval: every tool call through it pauses
// the task for the person's decision.
func (t *agentTemplate) requiresApproval(server string) bool {
	for _, tool := range t.Spec.Tools {
		if tool.MCP != nil && tool.MCP.Server.Name == server {
			return tool.MCP.RequireApproval
		}
	}
	return false
}

// chartLabel is the helm.sh/chart label the release stamped on the template
// (the chart version the render came from), "" for a template no chart wrote.
func (t *agentTemplate) chartLabel() string {
	return t.Metadata.Labels["helm.sh/chart"]
}

// agentTemplateFrom reads the part of an AgentTemplate the proofs look at off
// the object as the apiserver returned it.
func agentTemplateFrom(obj *unstructured.Unstructured) (*agentTemplate, error) {
	var t agentTemplate
	if err := decodeObject(obj, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// agentFrom reads the part of an Agent the proofs look at off the object as
// the apiserver returned it.
func agentFrom(obj *unstructured.Unstructured) (*agentObject, error) {
	var a agentObject
	if err := decodeObject(obj, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func decodeObject(obj *unstructured.Unstructured, into any) error {
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// readAgentTemplate reads one AgentTemplate of the kagent namespace; a
// missing one is the apiserver's NotFound.
func readAgentTemplate(name string) (*agentTemplate, error) {
	obj, err := readKagentObject(agentTemplateResource, name)
	if err != nil {
		return nil, err
	}
	t, err := agentTemplateFrom(obj)
	if err != nil {
		return nil, fmt.Errorf("parsing AgentTemplate %s: %w", name, err)
	}
	return t, nil
}

// readAgent reads one Agent of the kagent namespace; a missing one is the
// apiserver's NotFound.
func readAgent(name string) (*agentObject, error) {
	obj, err := readKagentObject(agentResource, name)
	if err != nil {
		return nil, err
	}
	a, err := agentFrom(obj)
	if err != nil {
		return nil, fmt.Errorf("parsing Agent %s: %w", name, err)
	}
	return a, nil
}

// readKagentObject reads one object of the given resource (a kubectl resource
// argument) in the kagent namespace, bounded by kubeReadTimeout.
func readKagentObject(resourceArg, name string) (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(resourceArg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, kagentNamespace, name)
}

// agentTemplateExists reports whether an agent's AgentTemplate is there; a
// read that fails counts as absent.
func agentTemplateExists(name string) bool {
	_, err := readKagentObject(agentTemplateResource, name)
	return err == nil
}

// waitAgentTemplate waits for an agent's AgentTemplate to exist — the
// render of its HelmRelease, seconds after the release is written — and
// returns it.
func waitAgentTemplate(name string) (*agentTemplate, error) {
	var t *agentTemplate
	var lastErr error
	found := waitFor(int(time.Minute/pollInterval), pollInterval, func() bool {
		t, lastErr = readAgentTemplate(name)
		return lastErr == nil
	})
	if !found {
		return nil, fmt.Errorf("no AgentTemplate %s within 1 min (the HelmRelease's render): %w;\ncheck `kubectl -n %s get %s %s -o yaml`", name, lastErr, kagentNamespace, fluxHelmReleaseResource, name)
	}
	return t, nil
}

// agentTemplateManagers lists the field managers on an agent's AgentTemplate
// — who wrote it.
func agentTemplateManagers(name string) ([]string, error) {
	obj, err := readKagentObject(agentTemplateResource, name)
	if err != nil {
		return nil, err
	}
	var managers []string
	for _, entry := range obj.GetManagedFields() {
		managers = append(managers, entry.Manager)
	}
	return managers, nil
}
