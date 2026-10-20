package lab

import (
	"strings"
	"testing"
	"time"
)

// The manager's threshold: absent, unparsable or outside the range refused,
// inside it taken.
func TestJudgeRefreshThreshold(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want time.Duration
		err  string
	}{
		{args: []string{"--listen=:8080"}, err: "0.8.2"},
		{args: []string{"--sync-refresh-after=soon"}, err: "--sync-refresh-after=soon"},
		{args: []string{"--sync-refresh-after=1h"}, err: "--workspaces-refresh-after 1m"},
		{args: []string{"--sync-refresh-after=10s"}, err: "30s to 5m"},
		{args: []string{"--listen=:8080", "--sync-refresh-after=1m0s"}, want: time.Minute},
		{args: []string{"--sync-refresh-after=30s"}, want: 30 * time.Second},
		{args: []string{"--sync-refresh-after=5m"}, want: 5 * time.Minute},
	} {
		got, err := judgeRefreshThreshold(tc.args)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%v: err = %v, want one naming %q", tc.args, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%v = %s, %v; want %s", tc.args, got, err, tc.want)
		}
	}
}

// Nothing started, changed or requested: the workspace stays at its last
// sync; a running, a requested or a newer cycle is the failure.
func TestJudgeQuiet(t *testing.T) {
	at := time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC)
	last := &syncLastSync{Time: at, Result: syncResultSucceed, Trigger: "change", Job: "sync-1"}
	same := *last
	if err := judgeQuiet(last, syncWorkspace{LastSync: &same}); err != nil {
		t.Errorf("unchanged: %v", err)
	}
	running := syncWorkspace{LastSync: &same, ActiveSync: &struct {
		Job string `json:"job"`
	}{Job: "sync-4"}}
	requested := syncWorkspace{LastSync: &same, SyncRequest: &struct {
		RequestedAt time.Time `json:"requestedAt"`
	}{RequestedAt: at.Add(time.Minute)}}
	newer := syncWorkspace{LastSync: &syncLastSync{Time: at.Add(time.Minute), Result: syncResultSucceed, Trigger: syncTriggerRequest, Job: "sync-3"}}
	for name, w := range map[string]syncWorkspace{"running": running, "requested": requested, "newer": newer, "never synced": {}} {
		if err := judgeQuiet(last, w); err == nil {
			t.Errorf("%s: judged quiet", name)
		}
	}
}

// The cycle after a start past the threshold: requested, ending after the
// answer.
func TestJudgeRefreshCycle(t *testing.T) {
	const job, waited = "sync-2", "waited"
	answered := time.Date(2026, 10, 15, 7, 2, 0, 0, time.UTC)
	ok := &syncLastSync{Time: answered.Add(5 * time.Second), Trigger: syncTriggerRequest, Job: job}
	if err := judgeRefreshCycle(ok, answered); err != nil {
		t.Errorf("requested, ended after the answer: %v", err)
	}
	for name, tc := range map[string]struct {
		r    *syncLastSync
		want string
	}{
		"no cycle":       {nil, "no cycle"},
		"changed":        {&syncLastSync{Time: ok.Time, Trigger: "change", Job: job}, `want "request"`},
		"ended before":   {&syncLastSync{Time: answered.Add(-time.Second), Trigger: syncTriggerRequest, Job: job}, waited},
		"ended together": {&syncLastSync{Time: answered, Trigger: syncTriggerRequest, Job: job}, waited},
	} {
		if err := judgeRefreshCycle(tc.r, answered); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

// The configure flag's help reads the part's range as a person types it.
func TestRefreshThresholdWords(t *testing.T) {
	if got := RefreshThresholdRange(); got != "30s to 5m" {
		t.Errorf("range = %q", got)
	}
	if got := RefreshThresholdExample(); got != "1m" {
		t.Errorf("example = %q", got)
	}
	if got := formatThreshold(90 * time.Second); got != "1m30s" {
		t.Errorf("90s = %q", got)
	}
}
