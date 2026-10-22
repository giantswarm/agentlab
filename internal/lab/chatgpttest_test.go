package lab

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart's model is a person's own login only without apiKeySecret, and
// counts only once kagent accepted it.
func TestJudgeChatGPTModel(t *testing.T) {
	model := func(secret, accepted string) *unstructured.Unstructured {
		spec := map[string]any{modelField: config.DefaultChatGPTSubscriptionModel, "openAI": map[string]any{credentialTypeKey: config.CredentialTypeChatGPT}}
		if secret != "" {
			spec["apiKeySecret"] = secret
		}
		obj := map[string]any{crSpec: spec}
		if accepted != "" {
			obj[fieldStatus] = map[string]any{crConditions: []any{map[string]any{fieldType: conditionAccepted, fieldStatus: accepted, fieldReason: "r", fieldMessage: "m"}}}
		}
		return &unstructured.Unstructured{Object: obj}
	}
	for name, tc := range map[string]struct {
		m    *unstructured.Unstructured
		want string
	}{
		"the person's login":     {model("", conditionTrue), ""},
		"the installation login": {model("kagent-chatgpt-login", conditionTrue), "apiKeySecret"},
		"not accepted":           {model("", "False"), "Accepted=False"},
		"no status":              {model("", ""), "no Accepted condition"},
		"an API key model":       {&unstructured.Unstructured{Object: map[string]any{crSpec: map[string]any{}}}, "credentialType"},
	} {
		t.Run(name, func(t *testing.T) {
			got := judgeChatGPTModel(tc.m)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("judgeChatGPTModel = %q, want %q", got, tc.want)
			}
		})
	}
}

// chatgptTurnOf is a turn ended in state with the gateway's requests.
func chatgptTurnOf(state a2a.TaskState, requests ...egressGatewayRequest) *chatgptTurn {
	return &chatgptTurn{session: "s", task: &a2a.Task{Status: a2a.TaskStatus{State: state}}, record: egressGatewayRecord{actor: "session-s", requests: requests}}
}

// backendMethod is the method of codex's requests to the backend.
const backendMethod = "POST"

var (
	backendForwarded = egressGatewayRequest{method: backendMethod, host: chatgptBackendHost, path: chatgptBackendPath + "responses", status: "200", endpoint: "1.2.3.4:443"}
	backendDenied    = egressGatewayRequest{method: backendMethod, host: chatgptBackendHost, path: chatgptBackendPath + "responses", status: "403", reason: "credential provider muster.giantswarm.io denied: the person holds no grant for the provider"}
	otherForwarded   = egressGatewayRequest{method: backendMethod, host: chatgptBackendHost, path: "/backend-api/plugins/x", status: "200", endpoint: "1.2.3.4:443"}
)

// The connected turn passes on forwarded backend requests and a completed
// task alone.
func TestJudgeConnectedTurn(t *testing.T) {
	for name, tc := range map[string]struct {
		turn *chatgptTurn
		want string
	}{
		"forwarded and completed": {chatgptTurnOf(a2a.TaskStateCompleted, backendForwarded), ""},
		"no backend request":      {chatgptTurnOf(a2a.TaskStateCompleted, otherForwarded), "recorded no request"},
		"one refused":             {chatgptTurnOf(a2a.TaskStateCompleted, backendForwarded, backendDenied), "refused a backend request"},
		"failed":                  {chatgptTurnOf(a2a.TaskStateFailed, backendForwarded), "ended failed"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := judgeConnectedTurn(tc.turn)
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("judgeConnectedTurn: %v, want %q", err, tc.want)
			}
		})
	}
}

// A refusal is the gateway's 403 with the credential provider's reason,
// nothing forwarded, and a turn that did not complete.
func TestJudgeRefusedTurn(t *testing.T) {
	policy := backendDenied
	policy.reason = egressPolicyDenied
	for name, tc := range map[string]struct {
		turn *chatgptTurn
		want string
	}{
		"a refusal":             {chatgptTurnOf(a2a.TaskStateFailed, backendDenied), ""},
		"forwarded":             {chatgptTurnOf(a2a.TaskStateFailed, backendDenied, backendForwarded), "forwarded a backend request"},
		"another reason":        {chatgptTurnOf(a2a.TaskStateFailed, policy), "another reason"},
		"no request":            {chatgptTurnOf(a2a.TaskStateFailed), "recorded no request"},
		"completed nonetheless": {chatgptTurnOf(a2a.TaskStateCompleted, backendDenied), "completed although"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := judgeRefusedTurn(tc.turn)
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("judgeRefusedTurn: %v, want %q", err, tc.want)
			}
		})
	}
}

// The sandbox passes with the coding proof's clean readings and no refresh
// token in a file or a variable.
func TestJudgeChatGPTSandbox(t *testing.T) {
	clean := func() codingProbe {
		return codingProbe{"env-tokens": "0", "data-tokens": "0", "model-key": authPlaceholder, "refresh-tokens": "0 0"}
	}
	for name, tc := range map[string]struct {
		change func(codingProbe)
		want   string
	}{
		"clean":                  {func(codingProbe) {}, ""},
		"a refresh token file":   {func(p codingProbe) { p["refresh-tokens"] = "1 0" }, "a refresh token"},
		"a refresh variable":     {func(p codingProbe) { p["refresh-tokens"] = "0 1" }, "a refresh token"},
		"no refresh reading":     {func(p codingProbe) { delete(p, "refresh-tokens") }, "no refresh-tokens reading"},
		"a bearer in the env":    {func(p codingProbe) { p["env-tokens"] = "1" }, "token-shaped"},
		"a model key of its own": {func(p codingProbe) { p["model-key"] = authOther }, "not kagent's"},
	} {
		t.Run(name, func(t *testing.T) {
			p := clean()
			tc.change(p)
			err := judgeChatGPTSandbox(p)
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("judgeChatGPTSandbox: %v, want %q", err, tc.want)
			}
		})
	}
}

// The probe prints counts, never a value: the refresh_token search lists
// file names into a count.
func TestChatGPTSandboxCommandPrintsCounts(t *testing.T) {
	cmd := chatgptSandboxCommand(codingRuntimes[config.CodingRuntimeCodex])
	if !strings.Contains(cmd, `refresh-tokens=$(timeout 60 grep -rlsE`) || !strings.Contains(cmd, "wc -l") {
		t.Errorf("the refresh_token search is not a count of files:\n%s", cmd)
	}
	if !strings.Contains(chatgptPlainProbePrompt(), chatgptBackendURL) {
		t.Errorf("the plain agent's probe does not request %s", chatgptBackendURL)
	}
}
