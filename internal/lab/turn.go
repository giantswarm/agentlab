package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// Turn is `agentlab turn`: one conversation with an agent as a lab user, the
// way the surfaces drive it — the person's Dex id_token on the kagent
// controller's gRPC route through the edge. With no template it prints the
// roster the person sees (ListAgentTemplates as that person, or the gRPC
// status when the controller refuses). With a template and a prompt it
// creates an AgentInstance on the Harness that admits the template and
// reports it Ready — the one the portal and Swarmgeist pick — or on the
// named harness, streams one turn, prints the answer and the terminal task
// state, and deletes the instance — unless keep is set, which leaves the
// instance for a later turn, or instanceID names one to continue — after
// suspending it to the snapshot store first when suspend is set, so the turn
// proves the restore.
func Turn(cfg *config.Config, email, template, harness, prompt, instanceID string, keep, suspend bool) error {
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
	created := instanceID == ""
	if created {
		if harness == "" {
			templates, err := api.listTemplates(ctx)
			if err != nil {
				return err
			}
			if harness, err = admittingHarness(templates, template); err != nil {
				return err
			}
		}
		instance, err := api.createInstanceOn(ctx, harness, template, uuid.NewString())
		if err != nil {
			return err
		}
		instanceID = instance.GetId()
		fmt.Printf("instance: %s of %s on Harness %s (creator %s, %s)\n", instanceID, template, harness, instance.GetCreator(), instanceState(instance))
	} else {
		fmt.Printf("instance: %s (continued)\n", instanceID)
		if suspend {
			instance, err := api.suspendInstance(ctx, instanceID)
			if err != nil {
				return err
			}
			fmt.Printf("suspended: %s (%s)\n", instanceID, instanceState(instance))
			if instance, err = api.resumeInstance(ctx, instanceID); err != nil {
				return err
			}
			fmt.Printf("resumed: %s (%s)\n", instanceID, instanceState(instance))
		}
	}
	t, err := api.completedTurnOnce(instanceID, prompt)
	if err != nil {
		if created && !keep {
			api.removeInstance(instanceID)
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
		fmt.Printf("kept: %s (agentlab turn --instance %s …)\n", instanceID, instanceID)
		return nil
	}
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rmCancel()
	if err := api.deleteInstance(rmCtx, instanceID); err != nil {
		return err
	}
	fmt.Printf("deleted: %s\n", instanceID)
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

// admittingHarness is the Harness a conversation with the named template is
// created on: the admitting Harness that reports it Ready, as the roster
// reads it. A template the person's roster does not list, or one no Harness
// runs yet, is refused with the roster's reason.
func admittingHarness(templates []*apiv1alpha1.AgentTemplate, name string) (string, error) {
	for _, t := range templates {
		if t.GetRef().GetName() != name {
			continue
		}
		listing := listingOf(t)
		if listing.Unavailable != "" {
			return "", fmt.Errorf("AgentTemplate %s cannot start a conversation: %s", name, listing.Unavailable)
		}
		return listing.Harness, nil
	}
	return "", fmt.Errorf("AgentTemplate %s is not in this user's roster (agentlab turn --list)", name)
}
