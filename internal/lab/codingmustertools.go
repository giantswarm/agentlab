package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// selectMusterTools gives the agent its tool selection on muster, as a
// platform coding agent binds muster's meta-tools: the Generic chart's
// muster.tools on the agent's HelmRelease, written by the lab as the admin
// (agent-manager writes no selection), then the template rendering it and
// the Agent Ready again. Without a selection it does nothing.
func (p *codingProof) selectMusterTools() error {
	if len(p.musterTools) == 0 {
		return nil
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	tools, err := json.Marshal(p.musterTools)
	if err != nil {
		return err
	}
	body := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}},"spec":{"values":{"muster":{"tools":%s}}}}`, fluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339Nano), tools)
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if err := patchObject(ctx, gvr, kagentNamespace, codingTestAgent, types.MergePatchType, body); err != nil {
		return fmt.Errorf("selecting muster tools on the HelmRelease %s/%s: %w", kagentNamespace, codingTestAgent, err)
	}
	var selected []string
	waitFor(int(p.timeout/pollInterval), pollInterval, func() bool {
		t, err := readAgentTemplate(codingTestAgent)
		selected = nil
		if err == nil {
			selected = t.musterTools()
		}
		return slices.Equal(selected, p.musterTools)
	})
	if !slices.Equal(selected, p.musterTools) {
		return fmt.Errorf("agent %s selects %v on muster after %s, wanted %v (%s)", codingTestAgent, selected, p.timeout, p.musterTools, p.line)
	}
	boot, err := waitAgentReadyOn(codingTestAgent, p.rt.name, p.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s is not Ready on Harness %s after its tool selection: %s", codingTestAgent, p.rt.name, boot.reason)
	}
	note("agent %s selects %d tools on muster: %s", codingTestAgent, len(selected), strings.Join(selected, ", "))
	return nil
}

// musterTools is the tool selection of the template's MCP binding; nil when
// it binds the server whole.
func (t *agentTemplate) musterTools() []string {
	for _, tool := range t.Spec.Tools {
		if tool.MCP != nil {
			return tool.MCP.Tools
		}
	}
	return nil
}
