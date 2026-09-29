package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// Turn is `agentlab turn`: one conversation with an agent as a lab user, the
// way the surfaces drive it — the person's Dex id_token on the kagent
// controller's gRPC route through the edge. With no template it prints the
// roster the person sees (ListAgents as that person, or the gRPC status when
// the controller refuses). With a template and a prompt it creates a Session
// of the Agent that pairs the template with a Harness — the Agent named after
// the template, or with harness set the one pairing the template with that
// Harness — streams one turn, prints the answer and the terminal task state,
// and deletes the session — unless keep is set, which leaves the session for
// a later turn, or sessionID names one to continue — after suspending it to
// the snapshot store first when suspend is set, so the turn proves the
// restore. A turn that pauses for tool approval prints the request and
// stops, unless decide is "approve" or "reject": then every request is
// answered that way until the task settles.
func Turn(cfg *config.Config, email, template, harness, prompt, sessionID, decide, reason, eventsFile, shareWith string, shareTTL time.Duration, keep, suspend bool) error {
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no lab user %q in agentlab.yaml", email)
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer api.close()
	if eventsFile != "" {
		f, err := os.Create(eventsFile) // #nosec G304 -- a path the lab user names
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		api.events = f
	}
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()

	if template == "" {
		agents, templates, err := api.roster(ctx)
		if code := status.Code(err); err != nil && code != codes.Unknown {
			fmt.Printf("roster refused for %s: %s: %s\n", user.Email, code, status.Convert(err).Message())
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("roster for %s (%d agents):\n", user.Email, len(agents))
		byName := templatesByName(templates)
		for _, agent := range agents {
			fmt.Println(rosterEntry(listingOf(agent, byName)))
		}
		return nil
	}

	started := time.Now()
	var session *apiv1alpha1.Session
	created := sessionID == ""
	if created {
		agents, templates, err := api.roster(ctx)
		if err != nil {
			return err
		}
		agent, err := selectAgent(agents, templates, template, harness)
		if err != nil {
			return err
		}
		if session, err = api.createSession(ctx, agent.Name, uuid.NewString()); err != nil {
			return err
		}
		sessionID = session.GetId()
		fmt.Printf("session: %s of Agent %s on Harness %s (creator %s, %s)\n", sessionID, agent.Name, agent.Harness, session.GetCreator(), sessionState(session))
	} else {
		if session, err = api.getSession(ctx, sessionID); err != nil {
			return err
		}
		fmt.Printf("session: %s of Agent %s (continued)\n", sessionID, session.GetAgent().GetName())
		if suspend {
			suspended, err := api.suspendSession(ctx, sessionID)
			if err != nil {
				return err
			}
			fmt.Printf("suspended: %s (%s)\n", sessionID, sessionState(suspended))
			if session, err = api.resumeSession(ctx, sessionID); err != nil {
				return err
			}
			fmt.Printf("resumed: %s (%s)\n", sessionID, sessionState(session))
		}
	}
	turnAPI := api
	if shareWith != "" {
		shared, revoke, err := sharedTurnAPI(ctx, cfg, api, sessionID, shareWith, shareTTL)
		if err != nil {
			return err
		}
		defer revoke()
		defer shared.close()
		turnAPI = shared
	}
	t, err := turnAPI.decidedTurn(session, prompt, decide, reason)
	if err != nil {
		if created && !keep {
			api.removeSession(sessionID)
		}
		return fmt.Errorf("A2A turn on %s: %w", template, err)
	}
	fmt.Printf("state: %s (%s)\nelapsed: %s\nanswer: %s\n", stateName(t.state()), t.statesString(), time.Since(started).Round(time.Millisecond), t.text())
	if len(t.statusMeta) != 0 {
		if raw, err := json.Marshal(t.statusMeta); err == nil {
			fmt.Printf("status metadata: %s\n", raw)
		}
	}
	if keep {
		fmt.Printf("kept: %s (agentlab turn --session %s …)\n", sessionID, sessionID)
		return nil
	}
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rmCancel()
	if err := api.deleteSession(rmCtx, sessionID); err != nil {
		return err
	}
	fmt.Printf("deleted: %s\n", sessionID)
	return nil
}

// rosterEntry is one roster entry as `turn --list` prints it: the technical
// name, the template and the Harness the Agent pairs, the display name, and
// why the Agent cannot start a conversation when it cannot.
func rosterEntry(l agentListing) string {
	line := fmt.Sprintf("  %s/%s", l.Namespace, l.Name)
	if l.Template != "" {
		line += "  template=" + l.Template
	}
	if l.Harness != "" {
		line += "  harness=" + l.Harness
	}
	if l.DisplayName != "" {
		line += fmt.Sprintf("  %q", l.DisplayName)
	}
	if l.Unavailable != "" {
		line += "  unavailable: " + l.Unavailable
	}
	return line
}

// selectAgent is the Agent a conversation with the named template is created
// of: among the Agents referencing the template (or named after it), the one
// referencing the named Harness, else the one that reports Ready, as the
// roster reads it — the one the portal and Swarmgeist pick. A template the
// person's roster does not list, or one no Harness runs yet, is refused with
// the roster's reason.
func selectAgent(agents []*apiv1alpha1.Agent, templates []*apiv1alpha1.AgentTemplate, template, harness string) (agentListing, error) {
	byName := templatesByName(templates)
	var candidates []agentListing
	for _, agent := range agents {
		l := listingOf(agent, byName)
		if l.Template == template || (l.Template == "" && l.Name == template) {
			candidates = append(candidates, l)
		}
	}
	if len(candidates) == 0 {
		return agentListing{}, fmt.Errorf("AgentTemplate %s is not in this user's roster: no Agent pairs it with a Harness (agentlab turn --list)", template)
	}
	if harness != "" {
		for _, l := range candidates {
			if l.Harness == harness {
				if l.Unavailable != "" {
					return agentListing{}, fmt.Errorf("Agent %s cannot start a conversation: %s", l.Name, l.Unavailable)
				}
				return l, nil
			}
		}
		return agentListing{}, fmt.Errorf("no Agent pairs AgentTemplate %s with Harness %s (the roster has %s)", template, harness, agentNames(candidates))
	}
	for _, l := range candidates {
		if l.Unavailable == "" {
			return l, nil
		}
	}
	return agentListing{}, fmt.Errorf("Agent %s cannot start a conversation: %s", candidates[0].Name, candidates[0].Unavailable)
}

// agentNames words the candidates as `name (harness)`.
func agentNames(listings []agentListing) string {
	names := make([]string, 0, len(listings))
	for _, l := range listings {
		names = append(names, fmt.Sprintf("%s (Harness %s)", l.Name, l.Harness))
	}
	return strings.Join(names, ", ")
}

// decidedTurn drives one turn. Paused at input-required with no decision, it
// prints the approval request and returns the paused turn; with one, it
// answers until the task settles, which must then be completed.
func (a *kagentAPI) decidedTurn(session *apiv1alpha1.Session, prompt, decide, reason string) (*turn, error) {
	paused, err := a.turnOn(session, userMessage(prompt))
	if err != nil {
		return nil, err
	}
	if paused.state() != a2a.TaskStateInputRequired {
		if _, err := paused.completedText(); err != nil {
			return nil, err
		}
		return paused.turn, nil
	}
	if paused.approval != nil {
		fmt.Printf("paused for approval: %s\n", strings.Join(paused.approval.toolNames(), ", "))
	}
	if decide == "" {
		return paused.turn, nil
	}
	settled, decided, err := a.decideUntilSettled(session, paused, decide == "approve", reason)
	if err != nil {
		return nil, err
	}
	fmt.Printf("decisions: %d %s (%s)\n", len(decided), decide, strings.Join(decided, "; "))
	if _, err := settled.completedText(); err != nil {
		return nil, err
	}
	return settled, nil
}

// sharedTurnAPI is the kagent client of another lab user on the session: the
// owner creates a read-write share (expiring after ttl when one is given),
// and the other user's calls carry their own bearer plus the share token,
// which supplements their identity. The revoke func withdraws the share.
func sharedTurnAPI(ctx context.Context, cfg *config.Config, owner *kagentAPI, sessionID, email string, ttl time.Duration) (*kagentAPI, func(), error) {
	user := cfg.FindUser(email)
	if user == nil {
		return nil, nil, fmt.Errorf("no lab user %q in agentlab.yaml", email)
	}
	req := &apiv1alpha1.CreateSessionShareRequest{
		SessionId:  sessionID,
		Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE,
	}
	if ttl > 0 {
		req.Ttl = durationpb.New(ttl)
	}
	resp, err := owner.sessions.CreateSessionShare(owner.callCtx(ctx), req)
	if err != nil {
		return nil, nil, fmt.Errorf("sharing %s: %w", sessionID, err)
	}
	shareID := resp.GetShare().GetId()
	revoke := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := owner.sessions.RevokeSessionShare(owner.callCtx(rctx), &apiv1alpha1.RevokeSessionShareRequest{ShareId: shareID}); err != nil {
			note("revoking share %s: %v", shareID, err)
			return
		}
		fmt.Printf("share revoked: %s\n", shareID)
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		revoke()
		return nil, nil, err
	}
	shared, err := dialKagentAPI(cfg, token)
	if err != nil {
		revoke()
		return nil, nil, err
	}
	shared.extra = metadata.Pairs("x-share-token", resp.GetToken())
	expiry := "no expiry"
	if at := resp.GetShare().GetExpiresAt(); at != nil {
		expiry = "expires " + at.AsTime().UTC().Format(time.RFC3339)
	}
	fmt.Printf("shared: %s read-write with %s (share %s, %s)\n", sessionID, user.Email, shareID, expiry)
	return shared, revoke, nil
}
