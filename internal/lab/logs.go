package lab

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/giantswarm/agentlab/internal/config"
)

// componentPrometheus is the lab Prometheus as a log component.
const componentPrometheus = "prometheus"

// logTarget is where a component's logs come from: its namespace and the
// target streamLogs resolves to pods — a `deploy/<name>` or a label selector.
type logTarget struct {
	namespace, target string
}

// logTargets maps each component to its log target; the table also feeds
// cobra's ValidArgs via LogComponents, so dispatch and completion cannot
// drift.
var logTargets = map[string]logTarget{
	componentDex:         {componentDex, "app=dex"},
	componentMuster:      {platformNamespace, "app.kubernetes.io/name=muster"},
	componentBackstage:   {platformNamespace, "deploy/" + componentBackstage},
	componentPrometheus:  {observabilityNamespace, "app.kubernetes.io/name=prometheus"},
	mcpPrometheusRelease: {observabilityNamespace, "deploy/" + mcpPrometheusRelease},
}

// LogComponents lists what Logs accepts, for cobra's ValidArgs.
func LogComponents() []string { return slices.Sorted(maps.Keys(logTargets)) }

// Logs tails the given component's logs (`kubectl logs -f` of its pods,
// through the embedded client against the lab cluster whatever the shell's
// kubeconfig says) until every stream ends or the user interrupts with
// Ctrl-C, which ends the command cleanly.
func Logs(cfg *config.Config, component string) error {
	target, ok := logTargets[component]
	if !ok {
		return fmt.Errorf("unknown component %q (%s)", component, strings.Join(LogComponents(), ", "))
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return streamLogs(ctx, target.namespace, target.target, os.Stdout)
}
