package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

func completeConsentBrokerSignIn(ctx context.Context, configuration *config.Config, user *config.User, target string) error {
	browser, err := newConsentBrowser()
	if err != nil {
		return err
	}
	var reply *consentReply
	for range 24 {
		page, err := url.Parse(target)
		if err != nil {
			return err
		}
		origin := consentOrigin(target)
		if origin != consentOrigin(configuration.Issuer()) && origin != consentOrigin(configuration.MusterBaseURL()) && origin != consentOrigin(configuration.GatewayURL(componentMCPKubernetes)) {
			return fmt.Errorf("broker sign-in left the lab's configured issuers")
		}
		if reply == nil {
			reply, err = consentRequest(ctx, browser, http.MethodGet, target, nil, "")
			if err != nil {
				return err
			}
		}
		if reply.status == http.StatusFound || reply.status == http.StatusSeeOther {
			next, err := page.Parse(reply.header.Get("Location"))
			if err != nil {
				return err
			}
			target, reply = next.String(), nil
			continue
		}
		if reply.status != http.StatusOK {
			var failure struct {
				Code        string `json:"error"`
				Description string `json:"error_description"`
			}
			_ = json.Unmarshal(reply.body, &failure)
			return fmt.Errorf("broker sign-in stopped at %s with HTTP %d (%s: %s)", page.Path, reply.status, failure.Code, failure.Description)
		}
		if origin == consentOrigin(configuration.MusterBaseURL()) && page.Path == oauthProxyCallbackPath && signInCompleted(reply.body) {
			return nil
		}
		var form url.Values
		switch {
		case origin == consentOrigin(configuration.Issuer()) && strings.Contains(page.Path, "/auth/local"):
			form = url.Values{consentLoginParam: {user.Email}, passwordParam: {user.Password}}
		case origin == consentOrigin(configuration.Issuer()):
			if local := localConnectorLink(page, reply.body); local != "" {
				target, reply = local, nil
				continue
			}
			return fmt.Errorf("broker sign-in returned an unknown identity-provider page")
		default:
			form, err = consentForm(reply, origin+"/oauth/authorize")
			if err != nil {
				return fmt.Errorf("broker consent at %s: %w", page.Path, err)
			}
			note("approved downstream consent at %s", page.Host)
		}
		reply, err = consentRequest(ctx, browser, http.MethodPost, target, form, origin)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("broker sign-in exceeded 24 steps")
}
