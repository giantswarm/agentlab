package lab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestJudgeTurnGap pins the turn gap stage's verdict on the heartbeat: ticks
// in its own turn, none between the turns, ticks again in the next.
func TestJudgeTurnGap(t *testing.T) {
	ended := time.Date(2026, 10, 19, 17, 47, 8, 0, time.UTC)
	gap := turnGap{ended: ended, next: ended.Add(codingTurnGap)}
	tick := func(offset time.Duration, status string) string {
		return fmt.Sprintf("%d:%s", ended.Add(offset).Unix(), status)
	}
	frozen := []string{tick(-6*time.Second, "200"), tick(-3*time.Second, "200"), tick(5*time.Second, "200"), tick(codingTurnGap+time.Second, "200"), tick(codingTurnGap+4*time.Second, "200")}
	for name, tc := range map[string]struct {
		ticks []string
		want  string
	}{
		"frozen between turns":   {frozen, ""},
		"frozen, muster refused": {[]string{tick(-3*time.Second, "401"), tick(codingTurnGap, "401")}, ""},
		"ran in the gap":         {append(frozen, tick(time.Minute, "200")), "ticked 1 times in the"},
		"never ran":              {nil, "never ticked in the turn that started it"},
		"gone after the gap":     {frozen[:3], "not again in the next turn"},
	} {
		t.Run(name, func(t *testing.T) {
			ticks, err := parseHeartbeat(strings.Join(tc.ticks, " "))
			if err != nil {
				t.Fatalf("parseHeartbeat: %v", err)
			}
			verdict, err := judgeTurnGap(ticks, gap)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("judgeTurnGap: %v", err)
				}
				if !strings.Contains(verdict, "not once in the") {
					t.Errorf("the verdict %q does not word the gap", verdict)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("judgeTurnGap = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestParseHeartbeatRefusesAForeignLine(t *testing.T) {
	if _, err := parseHeartbeat("1760896028:200 cat: can't open"); err == nil {
		t.Error("parseHeartbeat accepted a line that is no tick")
	}
}
