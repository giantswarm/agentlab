package lab

import (
	"encoding/json"
	"testing"
)

// TestAgentIDNames: kagent's agent id names the fixture in either form, with
// or without its namespace; another agent's id does not.
func TestAgentIDNames(t *testing.T) {
	for id, want := range map[string]bool{
		BackstageTestCostAgent:                       true,
		"kagent/" + BackstageTestCostAgent:           true,
		"kagent__NS__agentlab_backstage_test_cost":   true,
		"kagent/" + backstageTestAgent:               false,
		"kagent__NS__agentlab_backstage_test":        false,
		"kagent/other-" + BackstageTestCostAgent[6:]: false,
	} {
		if got := agentIDNames(id, BackstageTestCostAgent); got != want {
			t.Errorf("agentIDNames(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestReportedCost: the usage bag's costUsd is found on the task, its
// status message or an agent message of its history; a bag without one, or
// none, is no cost.
func TestReportedCost(t *testing.T) {
	bag := func(fields string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(`{"`+usageMetadataKey+`":`+fields+`}`), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	withCost := bag(`{"promptTokenCount":12,"costUsd":0.0123}`)
	for name, tc := range map[string]struct {
		task a2aTask
		cost float64
		ok   bool
	}{
		"task metadata":  {task: a2aTask{Metadata: withCost}, cost: 0.0123, ok: true},
		"status message": {task: a2aTask{Status: a2aTaskStatus{Message: &a2aMessage{Metadata: withCost}}}, cost: 0.0123, ok: true},
		"agent message":  {task: a2aTask{History: []a2aMessage{{}, {Metadata: withCost}}}, cost: 0.0123, ok: true},
		"usage, no cost": {task: a2aTask{Metadata: bag(`{"promptTokenCount":12}`)}},
		"no usage":       {task: a2aTask{}},
	} {
		cost, ok := reportedCost(&tc.task)
		if cost != tc.cost || ok != tc.ok {
			t.Errorf("%s: reportedCost = %v, %v; want %v, %v", name, cost, ok, tc.cost, tc.ok)
		}
	}
}

// TestSessionUsageAgentCost: the byAgent entry of the fixture with a cost is
// read off the route's JSON; an entry without costUsd (no turn reported one)
// and other agents' entries are not.
func TestSessionUsageAgentCost(t *testing.T) {
	var u portalSessionUsage
	raw := `{"totals":{"turns":3,"costUsd":0.05},"byAgent":[
		{"agentId":"kagent__NS__agentlab_backstage_test","turns":2,"costUsd":0.04},
		{"agentId":null,"turns":0},
		{"agentId":"kagent__NS__agentlab_backstage_test_cost","turns":1,"costUsd":0.01}]}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	id, cost := u.agentCost(BackstageTestCostAgent)
	if cost == nil || *cost != 0.01 || id != "kagent__NS__agentlab_backstage_test_cost" {
		t.Errorf("agentCost = %q, %v; want the fixture's 0.01", id, cost)
	}
	var uncosted portalSessionUsage
	if err := json.Unmarshal([]byte(`{"byAgent":[{"agentId":"kagent/`+BackstageTestCostAgent+`","turns":1}]}`), &uncosted); err != nil {
		t.Fatal(err)
	}
	if _, cost := uncosted.agentCost(BackstageTestCostAgent); cost != nil {
		t.Errorf("agentCost = %v for an entry without costUsd, want nil", *cost)
	}
}
