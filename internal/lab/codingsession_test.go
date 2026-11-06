package lab

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

func TestCodingSessionBindsEveryRuntime(t *testing.T) {
	for runtime, settings := range codingRuntimes {
		t.Run(runtime, func(t *testing.T) {
			controller := newFakeKagent()
			const sessionID = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
			controller.sessions[sessionID] = &apiv1alpha1.Session{
				Id: sessionID, Creator: fakeToken, Agent: agentRef(codingTestAgent),
				State:     apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
				Providers: []*apiv1alpha1.SessionProvider{{Hostname: componentMuster, Audience: componentMuster, Scheme: "Bearer"}},
			}
			api := controller.serve(t, fakeToken)
			answer, err := json.Marshal(map[string]any{"session": map[string]string{"id": sessionID}})
			if err != nil {
				t.Fatal(err)
			}
			server, captured := fakeMuster(t, agentManagerToolPrefix+"start_session", string(answer), false)
			muster := &musterSession{client: server.Client(), url: server.URL, token: fakeToken}
			proof := codingProof{rt: settings, timeout: time.Second, muster: muster, api: api, sessions: &sessionSet{api: api, manager: muster}}
			session, err := proof.createSession(t.Context(), codingTestAgent)
			if err != nil {
				t.Fatal(err)
			}
			if session.GetId() != sessionID || !slices.Equal(proof.sessions.ids, []string{sessionID}) {
				t.Fatalf("Session %s, tracked %v, want %s", session.GetId(), proof.sessions.ids, sessionID)
			}
			if (*captured)[nameKey] != codingTestAgent || (*captured)[namespaceKey] != kagentNamespace || (*captured)[startSessionRequestIDArg] == "" {
				t.Errorf("start_session arguments = %v", *captured)
			}
			if len(controller.calls["CreateSession"]) != 0 {
				t.Error("created an unbound Session through the controller")
			}
		})
	}
}

func TestCodingSessionRefusesMissingActorProvider(t *testing.T) {
	for name, providers := range map[string][]*apiv1alpha1.SessionProvider{
		"no providers": nil,
		"no audience":  {{Hostname: componentMuster}},
	} {
		t.Run(name, func(t *testing.T) {
			controller := newFakeKagent()
			const sessionID = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
			controller.sessions[sessionID] = &apiv1alpha1.Session{
				Id: sessionID, State: apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, Providers: providers,
			}
			api := controller.serve(t, fakeToken)
			server, _ := fakeMuster(t, agentManagerToolPrefix+"start_session", `{"session":{"id":"`+sessionID+`"}}`, false)
			muster := &musterSession{client: server.Client(), url: server.URL, token: fakeToken}
			proof := codingProof{rt: codingRuntimes[claudeRuntime], timeout: time.Second, muster: muster, api: api, sessions: &sessionSet{api: api, manager: muster}}
			_, err := proof.createSession(t.Context(), codingTestAgent)
			if err == nil || !strings.Contains(err.Error(), "agent-manager did not bind it") {
				t.Fatalf("createSession = %v, want missing actor binding", err)
			}
			if !slices.Equal(proof.sessions.ids, []string{sessionID}) {
				t.Errorf("tracked Sessions = %v, want refused Session %s", proof.sessions.ids, sessionID)
			}
		})
	}
}

func TestCodingSessionManagerRefusal(t *testing.T) {
	controller := newFakeKagent()
	api := controller.serve(t, fakeToken)
	server, _ := fakeMuster(t, agentManagerToolPrefix+"start_session", "binding refused", true)
	muster := &musterSession{client: server.Client(), url: server.URL, token: fakeToken}
	proof := codingProof{rt: codingRuntimes[claudeRuntime], timeout: time.Second, muster: muster, api: api, sessions: &sessionSet{api: api, manager: muster}}
	_, err := proof.createSession(t.Context(), codingTestAgent)
	if err == nil || !strings.Contains(err.Error(), "binding refused") {
		t.Fatalf("createSession = %v, want manager refusal", err)
	}
	if len(proof.sessions.ids) != 0 || len(controller.calls["CreateSession"]) != 0 {
		t.Fatalf("manager refusal created Sessions: %v, controller calls %v", proof.sessions.ids, controller.calls)
	}
}
