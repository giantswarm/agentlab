package lab

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agentlab/internal/config"
)

// Turn is `agentlab turn`: one conversation with an agent as a lab user, the
// way the surfaces drive it — the person's Dex id_token on the kagent
// controller's gRPC route through the edge. With no template it prints the
// roster the person sees (ListAgentTemplates as that person, or the gRPC
// status when the controller refuses). With a template and a prompt it
// creates an AgentInstance on the platform Harness, streams one turn, prints
// the answer and the terminal task state, and deletes the instance.
func Turn(cfg *config.Config, email, template, prompt string) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()

	if template == "" {
		templates, err := api.listTemplates(ctx)
		if code := status.Code(err); err != nil && code != codes.Unknown {
			fmt.Printf("roster refused for %s: %s: %s\n", user.Email, code, status.Convert(err).Message())
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("roster for %s (%d templates):\n", user.Email, len(templates))
		for _, t := range templates {
			fmt.Println(rosterEntry(listingOf(t)))
		}
		return nil
	}

	started := time.Now()
	instance, err := api.createInstance(ctx, template, uuid.NewString())
	if err != nil {
		return err
	}
	fmt.Printf("instance: %s of %s (creator %s, %s)\n", instance.GetId(), template, instance.GetCreator(), instanceState(instance))
	t, err := api.completedTurnOnce(instance.GetId(), prompt)
	if err != nil {
		api.removeInstance(instance.GetId())
		return fmt.Errorf("A2A turn on %s: %w", template, err)
	}
	fmt.Printf("state: %s (%s)\nelapsed: %s\nanswer: %s\n", stateName(t.state()), t.statesString(), time.Since(started).Round(time.Millisecond), t.text())
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rmCancel()
	if err := api.deleteInstance(rmCtx, instance.GetId()); err != nil {
		return err
	}
	fmt.Printf("deleted: %s\n", instance.GetId())
	return nil
}

// rosterEntry is one roster entry as `turn --list` prints it: the technical
// name, the admitting Harness, the display name, and why the template cannot
// start a conversation when it cannot.
func rosterEntry(l templateListing) string {
	line := fmt.Sprintf("  %s/%s", l.Namespace, l.Name)
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
