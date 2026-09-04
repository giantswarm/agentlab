package lab

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// logCmd is the kubectl subcommand every log target shares.
const logCmd = "logs"

// logTargets maps each component to its `kubectl logs -f` args; the table also
// feeds cobra's ValidArgs via LogComponents, so dispatch and completion cannot
// drift.
var logTargets = map[string][]string{
	componentDex:     {"-n", componentDex, logCmd, "-l", "app=dex", "-f"},
	componentMuster:  {"-n", platformNamespace, logCmd, "-l", "app.kubernetes.io/name=muster", "-f"},
	"backstage":      {"-n", platformNamespace, logCmd, "-f", "deploy/backstage"},
	"prometheus":     {"-n", observabilityNamespace, logCmd, "-l", "app.kubernetes.io/name=prometheus", "-f"},
	"mcp-prometheus": {"-n", observabilityNamespace, logCmd, "-f", "deploy/" + mcpPrometheusRelease},
}

// LogComponents lists what Logs accepts, for cobra's ValidArgs.
func LogComponents() []string { return slices.Sorted(maps.Keys(logTargets)) }

// Logs tails the given component's logs (kubectl logs -f passthrough, against
// the lab cluster whatever the shell's kubeconfig says).
func Logs(cfg *config.Config, component string) error {
	args, ok := logTargets[component]
	if !ok {
		return fmt.Errorf("unknown component %q (%s)", component, strings.Join(LogComponents(), ", "))
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	return run("kubectl", args...)
}
