package lab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The agent-manager verdicts pass on agent-manager's refusal for the reason
// they prove only: an answer, muster's own refusal, another code or a
// refusal for another reason fails them.
func TestRefusedAs(t *testing.T) {
	envelope := func(isError bool, text string) *toolEnvelope {
		env := &toolEnvelope{IsError: isError}
		env.Content = append(env.Content, struct {
			Text string `json:"text"`
		}{Text: text})
		return env
	}
	const wrongNamespace = `invalid_request: namespace agents-agentlab-a is the namespace of organization agentlab-a: pass organization "agentlab-a" instead of namespace`
	want := `pass organization "agentlab-a"`
	text, err := refusedAs(envelope(true, wrongNamespace), codeInvalidRequest, want)
	require.NoError(t, err)
	require.Equal(t, wrongNamespace, text)
	for name, env := range map[string]*toolEnvelope{
		"answered":                 envelope(false, `{"name":"agentlab-organizations-tool","organization":"agentlab-a"}`),
		"muster's refusal":         envelope(true, "tool x_agent-manager_create_agent not found"),
		"another code":             envelope(true, `forbidden: create HelmRelease in agents-agentlab-a`),
		"another code, same words": envelope(true, `forbidden: pass organization "agentlab-a" instead of namespace`),
		"another invalid argument": envelope(true, "invalid_request: name is required"),
		"no organization named":    envelope(true, "invalid_request: namespace agents-agentlab-a is an organization's namespace: pass the organization's name as organization instead of namespace"),
	} {
		_, err := refusedAs(env, codeInvalidRequest, want)
		require.Error(t, err, name)
	}
}
