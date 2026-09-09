package lab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// kagent API v2 (kagent.dev/v1alpha3) as the proofs read it.
//
// An agent is an AgentTemplate in the kagent namespace — model, prompt, tool
// bindings — admitted by the Harness whose selector its harnessLabel matches
// (the platform's Go ADK Harness, kagentHarness) and compiled by the
// controller into a golden snapshot per revision; the template's readiness is
// status.harnesses[].conditions, per Harness. Its tools are whole
// RemoteMCPServers of the same namespace: the shared `muster` the
// connectivity chart renders (no header, the caller's bearer alone), or, for
// an agent that declares a toolset, agent-manager's per-agent copy
// `muster-<agent>` carrying X-Muster-Toolset in headersFrom. Nothing here
// pins an API version: the resources resolve through discovery (gvrFor).

const (
	// agentTemplateResource and remoteMCPServerResource are the resource
	// arguments, fully qualified so a same-named kind in another group can
	// never be meant.
	agentTemplateResource   = "agenttemplates.kagent.dev"
	remoteMCPServerResource = "remotemcpservers.kagent.dev"
	// agentTemplateAPIVersion is the apiVersion of the AgentTemplates and
	// RemoteMCPServers the proofs write themselves; a manifest names its
	// version, the reads never pin one.
	agentTemplateAPIVersion = "kagent.dev/v1alpha3"
	// harnessLabel selects the Harness that admits an AgentTemplate
	// (allowedAgentTemplates.selector.matchLabels).
	harnessLabel = "kagent.dev/harness"
	// remoteMCPServerKind is the kind an AgentTemplate's MCP tool binding names.
	remoteMCPServerKind = "RemoteMCPServer"
	// toolsetCarrierPrefix prefixes the per-agent RemoteMCPServer agent-manager
	// writes for an agent with a toolset: muster-<agent>.
	toolsetCarrierPrefix = componentMuster + "-"
)

// agentTemplate is the part of a kagent AgentTemplate the proofs read.
type agentTemplate struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ModelConfig *struct {
			Name string `json:"name"`
		} `json:"modelConfig"`
		SystemPrompt string `json:"systemPrompt"`
		Tools        []struct {
			MCP *struct {
				Server struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"server"`
				Tools []string `json:"tools"`
			} `json:"mcp"`
		} `json:"tools"`
	} `json:"spec"`
	Status struct {
		Harnesses []harnessStatus `json:"harnesses"`
	} `json:"status"`
}

// harnessStatus is one Harness's view of an AgentTemplate
// (status.harnesses[]): admission, resolution, compatibility and readiness as
// conditions, the revision the controller wants and the last one whose golden
// snapshot succeeded, and compile warnings.
type harnessStatus struct {
	Harness                  string   `json:"harness"`
	DesiredRevision          string   `json:"desiredRevision"`
	LatestSuccessfulRevision string   `json:"latestSuccessfulRevision"`
	Warnings                 []string `json:"warnings"`
	Conditions               []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

// harnessCondition is `{.status.harnesses[?(@.harness=="<h>")].conditions[?(@.type=="<t>")]}`:
// the condition's status ("True", "False", "Unknown", or "" when the Harness
// or the condition is not there yet) and its message.
func (t *agentTemplate) harnessCondition(harness, condType string) (status, message string) {
	for _, h := range t.Status.Harnesses {
		if h.Harness != harness {
			continue
		}
		for _, c := range h.Conditions {
			if c.Type == condType {
				return c.Status, c.Message
			}
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

// agentTemplateFrom reads the part of an AgentTemplate the proofs look at off
// the object as the apiserver returned it.
func agentTemplateFrom(obj *unstructured.Unstructured) (*agentTemplate, error) {
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	var t agentTemplate
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// readAgentTemplate reads one AgentTemplate of the kagent namespace.
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

// agentTemplateExists reports whether an agent's AgentTemplate is there; a
// read that fails counts as absent.
func agentTemplateExists(name string) bool {
	_, err := readKagentObject(agentTemplateResource, name)
	return err == nil
}

// toolsetCarrierName is the per-agent RemoteMCPServer of an agent with a
// toolset.
func toolsetCarrierName(agent string) string {
	return toolsetCarrierPrefix + agent
}

// waitAgentTemplate waits for an agent's AgentTemplate to exist (agent-manager
// writes it synchronously; the portal's apply path within a scaffolder task)
// and returns it.
func waitAgentTemplate(name string) (*agentTemplate, error) {
	var t *agentTemplate
	var lastErr error
	found := waitFor(20, 3*time.Second, func() bool {
		t, lastErr = readAgentTemplate(name)
		return lastErr == nil
	})
	if !found {
		return nil, fmt.Errorf("no AgentTemplate %s within 1 min: %w", name, lastErr)
	}
	return t, nil
}

// waitAgentTemplateReady waits for the Harness's Ready condition on the
// AgentTemplate — the controller has compiled the revision and taken its
// golden snapshot (≈ 10 s with the runtime image cached, ≈ 40 s cold) — and
// returns the template. The deadline reports the last Ready and Accepted
// conditions seen, and the compile warnings.
func waitAgentTemplateReady(name, harness string, timeout time.Duration) (*agentTemplate, error) {
	var t *agentTemplate
	var lastErr error
	ready := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		t, lastErr = readAgentTemplate(name)
		if lastErr != nil {
			return false
		}
		status, _ := t.harnessCondition(harness, conditionReady)
		return status == conditionTrue
	})
	if ready {
		return t, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("AgentTemplate %s: %w", name, lastErr)
	}
	readyStatus, readyMsg := t.harnessCondition(harness, conditionReady)
	acceptedStatus, acceptedMsg := t.harnessCondition(harness, "Accepted")
	var warnings []string
	for _, h := range t.Status.Harnesses {
		if h.Harness == harness {
			warnings = h.Warnings
		}
	}
	return nil, fmt.Errorf("AgentTemplate %s never became Ready on Harness %s within %s (Ready=%q %s; Accepted=%q %s; warnings %v);\ncheck `kubectl -n %s get %s %s -o yaml` and the label %s=%s the Harness admits",
		name, harness, timeout, readyStatus, readyMsg, acceptedStatus, acceptedMsg, warnings, kagentNamespace, agentTemplateResource, name, harnessLabel, harness)
}

// waitAgentTemplateGone waits for the AgentTemplate and its toolset carrier to
// be gone after a delete.
func waitAgentTemplateGone(name string) error {
	gone := waitFor(20, 3*time.Second, func() bool {
		if agentTemplateExists(name) {
			return false
		}
		_, err := readKagentObject(remoteMCPServerResource, toolsetCarrierName(name))
		return err != nil
	})
	if !gone {
		return fmt.Errorf("AgentTemplate %s or its carrier %s is still there 1 min after the delete", name, toolsetCarrierName(name))
	}
	return nil
}

// deleteAgentTemplate removes an agent's AgentTemplate and its toolset
// carrier without waiting (`--ignore-not-found --wait=false`); best effort,
// for the cleanup paths.
func deleteAgentTemplate(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	for _, target := range []struct{ resource, name string }{
		{agentTemplateResource, name},
		{remoteMCPServerResource, toolsetCarrierName(name)},
	} {
		gvr, err := gvrFor(target.resource)
		if err != nil {
			continue
		}
		_ = deleteObject(ctx, gvr, kagentNamespace, target.name, 0)
	}
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

// toolsetHeaderOf is the X-Muster-Toolset an agent's runtime sends, read off
// what the template binds: "" for the shared muster server (no toolset, the
// caller's bearer alone), the header's value on a per-agent carrier — the
// literal value agent-manager writes, or the Secret key an older carrier
// names. An error when the template binds no MCP server at all, or the
// carrier is missing or carries no such header.
func toolsetHeaderOf(t *agentTemplate) (string, error) {
	server := t.mcpServer()
	switch server {
	case "":
		return "", fmt.Errorf("no MCP server binding (spec.tools)")
	case componentMuster:
		return "", nil
	}
	rms, err := readKagentObject(remoteMCPServerResource, server)
	if err != nil {
		return "", fmt.Errorf("the bound RemoteMCPServer %s: %w", server, err)
	}
	return remoteMCPServerHeader(rms, toolsetHeader)
}

// remoteMCPServerHeader is the value of one spec.headersFrom entry of a
// RemoteMCPServer: its literal value, or the Secret key it names, decoded.
func remoteMCPServerHeader(rms *unstructured.Unstructured, name string) (string, error) {
	headers, _, _ := unstructured.NestedSlice(rms.Object, "spec", "headersFrom")
	for _, h := range headers {
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if got, _, _ := unstructured.NestedString(m, nameKey); got != name {
			continue
		}
		if value, found, _ := unstructured.NestedString(m, "value"); found {
			return value, nil
		}
		kind, _, _ := unstructured.NestedString(m, "valueFrom", fieldTypeKey)
		secret, _, _ := unstructured.NestedString(m, "valueFrom", nameKey)
		key, _, _ := unstructured.NestedString(m, "valueFrom", "key")
		if kind != kindSecret {
			return "", fmt.Errorf("RemoteMCPServer %s takes %s from a %s (%s/%s), not a value the proof can read", rms.GetName(), name, kind, secret, key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		obj, err := getObject(ctx, gvrSecrets, rms.GetNamespace(), secret)
		if err != nil {
			return "", fmt.Errorf("RemoteMCPServer %s takes %s from Secret %s/%s: %w", rms.GetName(), name, secret, key, err)
		}
		encoded, _, _ := unstructured.NestedString(obj.Object, "data", key)
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("secret %s key %s: %w", secret, key, err)
		}
		return string(value), nil
	}
	return "", fmt.Errorf("RemoteMCPServer %s carries no %s header (headersFrom names %v)", rms.GetName(), name, headerNames(headers))
}

// headerNames lists the names of a headersFrom slice, for an error.
func headerNames(headers []any) []string {
	var names []string
	for _, h := range headers {
		if m, ok := h.(map[string]any); ok {
			if n, _, _ := unstructured.NestedString(m, nameKey); n != "" {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

// fieldTypeKey is the `type` key of a headersFrom valueFrom (Secret | ConfigMap).
const fieldTypeKey = "type"
