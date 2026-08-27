package lab

import "fmt"

// Logs tails the given component's logs (kubectl logs -f passthrough).
func Logs(component string) error {
	switch component {
	case "dex":
		return run("kubectl", "-n", "dex", "logs", "-l", "app=dex", "-f")
	case "muster":
		return run("kubectl", "-n", platformNamespace, "logs", "-l", "app.kubernetes.io/name=muster", "-f")
	case "backstage":
		return run("kubectl", "-n", "backstage", "logs", "-f", "deploy/backstage")
	default:
		return fmt.Errorf("unknown component %q (dex, muster, backstage)", component)
	}
}
