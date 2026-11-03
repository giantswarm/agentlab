package lab

import (
	"slices"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// muster's post-login redirect allowlist names the host-mode gateway's
// landing on the lab's own port block, so a second lab's proof returns to its
// own gateway.
func TestMusterAllowsTheGatewayLanding(t *testing.T) {
	stubWorkspacesProbe(t, true)
	for _, tc := range []struct {
		name    string
		apiPort int
		want    string
	}{
		{"the first lab", config.DefaultAPIServerPort, "http://127.0.0.1:18090/connectors/complete"},
		{"a second lab", config.DefaultAPIServerPort + 1, "http://127.0.0.1:18100/connectors/complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workspaceLab("")
			cfg.APIServerPort = tc.apiPort
			values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
			muster, _ := values["muster"].(map[string]any)
			inner, _ := muster["muster"].(map[string]any)
			oauth, _ := inner["oauth"].(map[string]any)
			client, _ := oauth["mcpClient"].(map[string]any)
			allow, _ := client["postLoginRedirectAllowlist"].([]any)
			if !slices.Equal(allow, []any{tc.want}) {
				t.Errorf("muster.muster.oauth.mcpClient.postLoginRedirectAllowlist = %v, want [%s]", allow, tc.want)
			}
			if got := klausGatewayLanding(gatewayTestPort(cfg)); got != tc.want {
				t.Errorf("klausGatewayLanding = %s, want %s", got, tc.want)
			}
		})
	}
}

// The sign-in prompt is the person's ephemeral with Sign in and Not now: not
// another person's, not a Connect prompt without Not now, not one before the
// turn, and the last of several.
func TestSignInPromptIn(t *testing.T) {
	button := func(id string) map[string]any { return map[string]any{slackKeyActionID: id} }
	actions := func(ids ...string) []map[string]any {
		var elements []any
		for _, id := range ids {
			elements = append(elements, button(id))
		}
		return []map[string]any{{slackKeyElements: elements}}
	}
	msgs := []slackMessage{
		{TS: "1", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
		{TS: "2", Method: slackPostMessage, Text: "Waiting for <@U1> to sign in"},
		{TS: "3", Method: slackPostEphemeral, Recipient: "U2", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
		{TS: "4", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect)},
		{TS: "5", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
	}
	if m, ok := signInPromptIn(msgs, 1, "U1"); !ok || m.TS != "5" {
		t.Errorf("signInPromptIn after 1 = %s, %v; want the prompt 5", m.TS, ok)
	}
	if _, ok := signInPromptIn(msgs[:5], 1, "U2"); !ok {
		t.Error("signInPromptIn for U2 found nothing; want the prompt 3")
	}
	if m, ok := signInPromptIn(msgs[:4], 1, "U1"); ok {
		t.Errorf("signInPromptIn after 1 without 5 = %s; want none: 4 has no Not now, 1 is before the turn", m.TS)
	}
}

func TestLinkRedirect(t *testing.T) {
	link := "https://muster.example/oauth/proxy/start?s=1&redirect=http%3A%2F%2F127.0.0.1%3A18090%2Fconnectors%2Fcomplete%3Fs%3Dabc"
	if got, want := linkRedirect(link), "http://127.0.0.1:18090/connectors/complete?s=abc"; got != want {
		t.Errorf("linkRedirect = %q, want %q", got, want)
	}
	if got := linkRedirect("https://muster.example/oauth/proxy/start?s=1"); got != "" {
		t.Errorf("linkRedirect of a link without one = %q, want empty", got)
	}
}
