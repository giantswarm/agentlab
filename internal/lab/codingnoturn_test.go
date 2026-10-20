package lab

import (
	"strings"
	"testing"
	"time"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// TestJudgeNoTurn pins the no turn stage's verdict on the heartbeat: the
// ticks while the actor ran with no turn must all be the egress gateway's
// 403, at least one, and the next turn must reach muster again.
func TestJudgeNoTurn(t *testing.T) {
	resumed := time.Unix(1_800_000_000, 0)
	w := noTurnWindow{resumed: resumed, suspended: resumed.Add(codingNoTurnWindow), next: resumed.Add(codingNoTurnWindow + time.Second)}
	at := func(offset time.Duration, status string) heartbeatTick {
		return heartbeatTick{at: resumed.Add(offset), status: status}
	}
	before := []heartbeatTick{at(-2*time.Minute, "200"), at(-time.Minute, "200")}
	refused := []heartbeatTick{at(3*time.Second, "403"), at(6*time.Second, "403"), at(9*time.Second, "403")}
	reopened := []heartbeatTick{at(codingNoTurnWindow+10*time.Second, "200")}

	for _, test := range []struct {
		name  string
		ticks []heartbeatTick
		want  string
	}{
		{name: "refused, then reopened", ticks: concat(before, refused, reopened)},
		{name: "never ran", ticks: concat(before, reopened), want: "never ticked"},
		{name: "muster answered", ticks: concat(before, refused, []heartbeatTick{at(12*time.Second, "401")}, reopened), want: "wanted 403"},
		{name: "served as the person", ticks: concat(before, []heartbeatTick{at(3*time.Second, "200")}, reopened), want: "wanted 403"},
		{name: "not reopened", ticks: concat(before, refused, []heartbeatTick{at(codingNoTurnWindow+10*time.Second, "403")}), want: "did not open"},
		{name: "the next turn's call in the send's second", ticks: concat(before, refused, []heartbeatTick{at(codingNoTurnWindow+time.Second, "200")}, reopened)},
		{name: "served in the suspend's second", ticks: concat(before, refused, []heartbeatTick{at(codingNoTurnWindow-time.Second, "200")}, reopened), want: "wanted 403"},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict, err := judgeNoTurn(test.ticks, w)
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("unexpected failure: %v", err)
			case test.want == "" && !strings.Contains(verdict, "403×3"):
				t.Fatalf("verdict %q does not count the refusals", verdict)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("got %v (verdict %q), wanted a failure naming %q", err, verdict, test.want)
			}
		})
	}
}

func concat(parts ...[]heartbeatTick) []heartbeatTick {
	var all []heartbeatTick
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

func TestPolicyAllowsHost(t *testing.T) {
	const muster = "muster.agent-platform.svc.cluster.local"
	policy := &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{
		{Https: &ateapi.HTTPSRule{Hostnames: []string{"api.github.com"}}},
	}}
	if policyAllowsHost(policy, muster) {
		t.Fatal("a policy without muster's host allows it")
	}
	policy.Rules = append(policy.Rules, &ateapi.EgressRule{Http: &ateapi.HTTPRule{Hostnames: []string{muster}}})
	if !policyAllowsHost(policy, muster) {
		t.Fatal("an HTTP rule on muster's host is not found")
	}
}
