package lab

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pausedTask is a task in input-required asking to approve tools under the
// HITL extension, the way the controller relays it.
func pausedTask(tools ...string) *a2aTask {
	var calls []any
	for i, tool := range tools {
		calls = append(calls, map[string]any{nameKey: tool, "id": "adk-" + string(rune('a'+i))})
	}
	return &a2aTask{ID: testTaskID, Status: a2aTaskStatus{State: taskStateInputRequired, Message: &a2aMessage{
		Extensions: []string{hitlExtensionURI},
		Metadata:   map[string]any{hitlExtensionURI: map[string]any{fieldType: hitlToolApprovalType, fieldTools: calls}},
	}}}
}

// chainedApprovals answers each approval with the next settled task of the
// chain and advances a fake clock by step per round.
type chainedApprovals struct {
	chain    []*a2aTask
	clock    time.Time
	step     time.Duration
	approved int
	timeouts []time.Duration
}

func (c *chainedApprovals) approver() hitlApprover {
	return hitlApprover{
		approve: func(taskID string) error {
			if taskID != testTaskID {
				return errors.New("approved another task: " + taskID)
			}
			c.approved++
			return nil
		},
		settle: func(_ string, timeout time.Duration) (*a2aTask, error) {
			c.timeouts = append(c.timeouts, timeout)
			c.clock = c.clock.Add(c.step)
			if c.approved > len(c.chain) {
				return nil, errors.New("settled past the end of the chain")
			}
			return c.chain[c.approved-1], nil
		},
		now: func() time.Time { return c.clock },
	}
}

// TestApproveUntilSettled: the loop approves however many gated calls the
// model chains — one, or the seven rounds that a five-round budget failed on
// — until the task completes, and names every round with its tools.
func TestApproveUntilSettled(t *testing.T) {
	completed := &a2aTask{ID: testTaskID, Status: a2aTaskStatus{State: taskStateCompleted}}
	for name, tc := range map[string]struct {
		first *a2aTask
		chain []*a2aTask
		want  [][]string
	}{
		"one approval": {pausedTask(fakeTool), []*a2aTask{completed}, [][]string{{fakeTool}}},
		"chained approvals": {pausedTask(fakeTool), []*a2aTask{
			pausedTask(toolCallTool), pausedTask(toolCallTool, toolCallTool), pausedTask("describe_tool"),
			pausedTask(toolCallTool), pausedTask(toolCallTool), pausedTask(toolCallTool), completed,
		}, [][]string{{fakeTool}, {toolCallTool}, {toolCallTool, toolCallTool}, {"describe_tool"}, {toolCallTool}, {toolCallTool}, {toolCallTool}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := &chainedApprovals{chain: tc.chain, clock: time.Unix(0, 0), step: 20 * time.Second}
			task, approved, err := approveUntilSettled(tc.first, c.approver(), portalHITLTimeout)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status.State != taskStateCompleted || !reflect.DeepEqual(approved, tc.want) || c.approved != len(tc.want) {
				t.Errorf("task %s, approved %v (%d answers), want %v", task.Status.State, approved, c.approved, tc.want)
			}
		})
	}
}

// TestApproveUntilSettledFailures: past the timeout, on a pause without an
// approval request or a task that ends otherwise, the error names every
// round with its tools; each settle waits no longer than the time left.
func TestApproveUntilSettledFailures(t *testing.T) {
	endless := make([]*a2aTask, 100)
	for i := range endless {
		endless[i] = pausedTask(toolCallTool)
	}
	c := &chainedApprovals{chain: endless, clock: time.Unix(0, 0), step: 3 * time.Minute}
	_, approved, err := approveUntilSettled(pausedTask(fakeTool), c.approver(), portalHITLTimeout)
	if err == nil {
		t.Fatal("an endless chain must fail at the timeout")
	}
	for _, want := range []string{"round 1: " + fakeTool, "round 2: " + toolCallTool, "round 3: " + toolCallTool, portalHITLTimeout.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(approved) != 3 || !reflect.DeepEqual(c.timeouts, []time.Duration{portalHITLResumeTimeout, portalHITLResumeTimeout, 2 * time.Minute}) {
		t.Errorf("approved %v, settle timeouts %v", approved, c.timeouts)
	}

	failed := &a2aTask{ID: testTaskID, Status: a2aTaskStatus{State: taskStateRejected}}
	c = &chainedApprovals{chain: []*a2aTask{pausedTask(toolCallTool, "describe_tool"), failed}, clock: time.Unix(0, 0), step: time.Second}
	if _, _, err := approveUntilSettled(pausedTask(fakeTool), c.approver(), portalHITLTimeout); err == nil ||
		!strings.Contains(err.Error(), "ended "+taskStateRejected+" after 2 approval round(s) (round 1: "+fakeTool+"; round 2: "+toolCallTool+"+describe_tool)") {
		t.Errorf("a failed task: %v", err)
	}

	bare := &a2aTask{ID: testTaskID, Status: a2aTaskStatus{State: taskStateInputRequired, Message: &a2aMessage{}}}
	c = &chainedApprovals{chain: []*a2aTask{bare}, clock: time.Unix(0, 0), step: time.Second}
	if _, _, err := approveUntilSettled(pausedTask(fakeTool), c.approver(), portalHITLTimeout); err == nil ||
		!strings.Contains(err.Error(), "after 1 approval round(s) (round 1: "+fakeTool+")") {
		t.Errorf("a pause without a request: %v", err)
	}
}

func TestApprovalRounds(t *testing.T) {
	if got := approvalRounds(nil); got != "no rounds" {
		t.Errorf("no rounds = %q", got)
	}
	if got := approvalRounds([][]string{{fakeTool}, {toolCallTool, toolCallTool}}); got != "round 1: "+fakeTool+"; round 2: "+toolCallTool+"+"+toolCallTool {
		t.Errorf("rounds = %q", got)
	}
}
