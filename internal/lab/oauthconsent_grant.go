package lab

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

func (flow *consentProofFlow) checkRefresh(ctx context.Context, configuration *config.Config, user *config.User, base string, aggregated bool) error {
	previous := flow.refreshToken
	if previous == "" {
		return fmt.Errorf("the public client's grant has no refresh token")
	}
	accessToken, err := flow.token(ctx, url.Values{
		grantTypeParam: {"refresh_token"}, "refresh_token": {previous},
		oauthClientID: {consentProofClientID}, oauthResourceParam: {flow.resource},
	})
	if err != nil {
		return err
	}
	if flow.refreshToken == "" || flow.refreshToken == previous {
		return fmt.Errorf("the public client's refresh token was not rotated")
	}
	return consentMCPRead(ctx, configuration, user, base, accessToken, aggregated)
}

func (flow *consentProofFlow) checkPKCE(ctx context.Context, user *config.User) error {
	negative := newConsentProofFlow(flow.browser, flow.metadata, flow.resource)
	negative.providerOrigin = flow.providerOrigin
	_, form, err := negative.page(ctx, "consent")
	if err != nil {
		return err
	}
	approved, err := negative.answer(ctx, form, consentOrigin(flow.metadata.Authorization))
	if err := expectConsentStatus(approved, err, http.StatusFound); err != nil {
		return err
	}
	negative.upstream = approved.header.Get("Location")
	code, err := negative.authenticate(ctx, user)
	if err != nil {
		return err
	}
	rejected, err := consentRequest(ctx, flow.browser, http.MethodPost, flow.metadata.Token, url.Values{
		grantTypeParam: {oauthAuthorizationCodeGrant}, oauthClientID: {consentProofClientID},
		oauthRedirectURI: {consentProofRedirect}, oauthCodeParam: {code},
		"code_verifier": {rand.Text() + rand.Text()}, oauthResourceParam: {flow.resource},
	}, "")
	if err := expectConsentStatus(rejected, err, http.StatusBadRequest); err != nil {
		return err
	}
	if !strings.Contains(string(rejected.body), "invalid_grant") {
		return fmt.Errorf("incorrect PKCE verifier did not receive invalid_grant")
	}
	return nil
}
