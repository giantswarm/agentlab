package lab

import (
	"testing"
	"time"
)

func TestCallAnswered(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"the fixture's core tools", `{"total_tools": 31, "filtered_count": 30, "tools": [{"name": "core_workflow_list"}]}`, true},
		{"the answer as an escaped JSON string", `{"isError":false,"content":[{"text":"{\n  \"total_tools\": 31,\n  \"filtered_count\": 30"}]}`, true},
		{"the reply holding the count line", `"filtered_count": 30,`, true},
		{"a reply naming the field without a count", "I could not read filtered_count: the tool did not run.", false},
		{"refused as outside the toolset", `Error: tool "x_lab-oauth-fixture_list_core_tools" is outside the toolset [server:lab-oauth-fixture] bound to the session`, false},
		{"declined", "Error: The tool did not run: the person answered decline to signing in to lab-oauth-fixture.", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := callAnswered(tc.text); got != tc.want {
				t.Errorf("callAnswered(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestFixtureCallsIn(t *testing.T) {
	since := time.Date(2026, 10, 27, 18, 44, 0, 0, time.UTC)
	logs := `{"time":"2026-10-27T18:43:59Z","msg":"tool dispatched","subject":"CiRjNGNl...","tool":"x_lab-oauth-fixture_list_core_tools","outcome":"ok"}
{"time":"2026-10-27T18:44:45Z","msg":"tool dispatched","subject":"CiRjNGNl...","tool":"x_lab-oauth-fixture_list_core_tools","outcome":"ok"}
{"time":"2026-10-27T18:44:46Z","msg":"tool dispatched","subject":"CiRjNGNl...","tool":"x_lab-oauth-fixture_list_core_tools","outcome":"error"}
{"time":"2026-10-27T18:44:47Z","msg":"tool dispatched","subject":"Ab12...","tool":"x_lab-oauth-fixture_list_core_tools","outcome":"ok"}
{"time":"2026-10-27T18:44:48Z","msg":"call_tool refused: tool \\"x_lab-oauth-fixture_list_core_tools\\" is outside the toolset","subject":"CiRjNGNl...","tool":"x_lab-oauth-fixture_list_core_tools"}
{"time":"2026-10-27T18:44:49Z","msg":"tools/call request","subject":"CiRjNGNl...","tool":"list_core_tools"}`
	got := fixtureCallsIn(logs, since, "CiRjNGNlYTBhMS01ODk5")
	if len(got) != 1 || got[0] != "2026-10-27T18:44:45Z" {
		t.Errorf("fixtureCallsIn = %v, want the one successful dispatch as the person after the instant", got)
	}
}
