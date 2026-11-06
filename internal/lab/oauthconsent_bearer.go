package lab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

func (flow *consentProofFlow) checkInvalidBearers(ctx context.Context, configuration *config.Config, user *config.User, base string) error {
	identityToken, err := passwordGrant(configuration, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, consentProofScopes)
	if err != nil {
		return err
	}
	unsignedBearer := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"person"}`)) + "."
	bearers := []struct{ name, value string }{
		{"OIDC ID token", identityToken},
		{"unknown opaque bearer", rand.Text()},
		{"unsigned JWT", unsignedBearer},
	}
	if flow.refreshToken != "" {
		bearers = append(bearers, struct{ name, value string }{"refresh token", flow.refreshToken})
	}
	for _, bearer := range bearers {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bearer-rejection","version":"1"}}}`))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+bearer.value)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := flow.browser.Do(request)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "invalid_token") {
			return fmt.Errorf("%s was not rejected as invalid_token (HTTP %d)", bearer.name, response.StatusCode)
		}
		note("%s: HTTP 401 invalid_token", bearer.name)
	}
	return nil
}
