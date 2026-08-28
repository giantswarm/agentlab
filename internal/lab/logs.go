package lab

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// logTargets maps each component to its `kubectl logs -f` args; the table also
// feeds cobra's ValidArgs via LogComponents, so dispatch and completion cannot
// drift.
var logTargets = map[string][]string{
	"dex":       {"-n", "dex", "logs", "-l", "app=dex", "-f"},
	"muster":    {"-n", platformNamespace, "logs", "-l", "app.kubernetes.io/name=muster", "-f"},
	"backstage": {"-n", "backstage", "logs", "-f", "deploy/backstage"},
}

// LogComponents lists what Logs accepts, for cobra's ValidArgs.
func LogComponents() []string { return slices.Sorted(maps.Keys(logTargets)) }

// Logs tails the given component's logs (kubectl logs -f passthrough).
func Logs(component string) error {
	args, ok := logTargets[component]
	if !ok {
		return fmt.Errorf("unknown component %q (%s)", component, strings.Join(LogComponents(), ", "))
	}
	return run("kubectl", args...)
}
