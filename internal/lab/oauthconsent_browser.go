package lab

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

type consentReply struct {
	status  int
	header  http.Header
	body    []byte
	cookies []*http.Cookie
}

func consentBrowserOn(transport http.RoundTripper) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func consentRequest(ctx context.Context, browser *http.Client, method, target string, form url.Values, origin string) (*consentReply, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := browser.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return &consentReply{response.StatusCode, response.Header, data, response.Cookies()}, nil
}

func consentForm(reply *consentReply, endpoint string) (url.Values, error) {
	if reply.status != http.StatusOK || !strings.Contains(reply.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		return nil, fmt.Errorf("expected HTTP 200 consent page with framing protection, got %d", reply.status)
	}
	for _, form := range htmlForms(reply.body) {
		if form.method == htmlPost && form.action == endpoint && form.fields.Get("consent_id") != "" {
			return url.Values{"consent_id": {form.fields.Get("consent_id")}, "decision": {consentApprove}}, nil
		}
	}
	return nil, fmt.Errorf("no consent form targeting the public authorization endpoint")
}

func (flow *consentProofFlow) page(ctx context.Context, prompt string) (*consentReply, url.Values, error) {
	reply, err := consentRequest(ctx, flow.browser, http.MethodGet, flow.authorizationURL(prompt), nil, "")
	if err != nil {
		return nil, nil, err
	}
	form, err := consentForm(reply, flow.metadata.Authorization)
	return reply, form, err
}

func (flow *consentProofFlow) answer(ctx context.Context, form url.Values, origin string) (*consentReply, error) {
	return consentRequest(ctx, flow.browser, http.MethodPost, flow.metadata.Authorization, form, origin)
}

func consentOrigin(endpoint string) string {
	parsed, _ := url.Parse(endpoint)
	return parsed.Scheme + "://" + parsed.Host
}

func expectConsentStatus(reply *consentReply, err error, status int) error {
	if err != nil {
		return err
	}
	if reply.status != status {
		return fmt.Errorf("expected HTTP %d, got %d", status, reply.status)
	}
	return nil
}

func (flow *consentProofFlow) checkConsentGate(ctx context.Context) error {
	blocked, err := url.Parse(flow.authorizationURL(""))
	if err != nil {
		return err
	}
	parameters := blocked.Query()
	parameters.Set("client_id", "https://100.64.0.1/client.json")
	blocked.RawQuery = parameters.Encode()
	rejected, err := consentRequest(ctx, flow.browser, http.MethodGet, blocked.String(), nil, "")
	if err := expectConsentStatus(rejected, err, http.StatusBadRequest); err != nil {
		return err
	}
	if rejected.header.Get("Location") != "" || !strings.Contains(string(rejected.body), "invalid_client") {
		return fmt.Errorf("blocked CIMD address did not receive a local invalid_client error")
	}
	silent, err := consentRequest(ctx, flow.browser, http.MethodGet, flow.authorizationURL("none"), nil, "")
	if err := expectConsentStatus(silent, err, http.StatusFound); err != nil {
		return err
	}
	if err := flow.checkErrorRedirect(silent, "consent_required"); err != nil {
		return err
	}
	page, form, err := flow.page(ctx, "")
	if err != nil {
		return err
	}
	for _, cookie := range page.cookies {
		if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || strings.Contains(cookie.Name, "mcp_oauth_flow") {
			return fmt.Errorf("consent page set an unsafe or premature transaction cookie")
		}
	}
	form.Set("decision", "deny")
	denied, err := flow.answer(ctx, form, consentOrigin(flow.metadata.Authorization))
	if err := expectConsentStatus(denied, err, http.StatusFound); err != nil {
		return err
	}
	if err := flow.checkErrorRedirect(denied, "access_denied"); err != nil {
		return err
	}
	_, form, err = flow.page(ctx, "")
	if err != nil {
		return err
	}
	foreign, err := flow.answer(ctx, form, "https://attacker.invalid")
	if err := expectConsentStatus(foreign, err, http.StatusForbidden); err != nil {
		return err
	}
	approved, err := flow.answer(ctx, form, consentOrigin(flow.metadata.Authorization))
	if err := expectConsentStatus(approved, err, http.StatusFound); err != nil {
		return err
	}
	flow.upstream = approved.header.Get("Location")
	replayed, err := flow.answer(ctx, form, consentOrigin(flow.metadata.Authorization))
	if err := expectConsentStatus(replayed, err, http.StatusBadRequest); err != nil {
		return err
	}
	otherBrowser, err := consentBrowserOn(flow.browser.Transport)
	if err != nil {
		return err
	}
	upstream, err := url.Parse(flow.upstream)
	if err != nil {
		return err
	}
	callback := flow.metadata.Issuer + "/oauth/callback?code=unusable&state=" + url.QueryEscape(upstream.Query().Get("state"))
	rejected, err = consentRequest(ctx, otherBrowser, http.MethodGet, callback, nil, "")
	return expectConsentStatus(rejected, err, http.StatusBadRequest)
}

func (flow *consentProofFlow) checkErrorRedirect(reply *consentReply, code string) error {
	target, err := url.Parse(reply.header.Get("Location"))
	if err != nil || target.Scheme+"://"+target.Host+target.Path != consentProofRedirect || target.Query().Get("error") != code || target.Query().Get(oauthStateParam) != flow.state || target.Query().Get("iss") != flow.metadata.Issuer || target.Query().Get(oauthCodeParam) != "" {
		return fmt.Errorf("OAuth error response failed callback, error, state or issuer validation")
	}
	return nil
}

func (flow *consentProofFlow) authenticate(ctx context.Context, user *config.User) (string, error) {
	return flow.followProvider(ctx, user, flow.upstream)
}

func (flow *consentProofFlow) followProvider(ctx context.Context, user *config.User, target string) (string, error) {
	for range 20 {
		location, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		if location.Scheme+"://"+location.Host+location.Path == consentProofRedirect {
			if location.Query().Get(oauthStateParam) != flow.state || location.Query().Get("iss") != flow.metadata.Issuer || location.Query().Get(oauthCodeParam) == "" {
				return "", fmt.Errorf("authorization response failed code, state or issuer validation")
			}
			return location.Query().Get(oauthCodeParam), nil
		}
		if consentOrigin(target) != flow.providerOrigin && consentOrigin(target) != consentOrigin(flow.metadata.Issuer) {
			return "", fmt.Errorf("sign-in left the configured identity provider and authorization server")
		}
		reply, err := consentRequest(ctx, flow.browser, http.MethodGet, target, nil, "")
		if err != nil {
			return "", err
		}
		if reply.status == http.StatusFound || reply.status == http.StatusSeeOther {
			next, err := location.Parse(reply.header.Get("Location"))
			if err != nil {
				return "", err
			}
			target = next.String()
			continue
		}
		if reply.status != http.StatusOK || consentOrigin(target) != flow.providerOrigin {
			return "", fmt.Errorf("sign-in stopped at %s with HTTP %d", location.Path, reply.status)
		}
		if local := localConnectorLink(location, reply.body); local != "" && !strings.Contains(location.Path, "/auth/local") {
			target = local
			continue
		}
		if !strings.Contains(location.Path, "/auth/local") {
			return "", fmt.Errorf("identity provider returned an unrecognized form")
		}
		reply, err = consentRequest(ctx, flow.browser, http.MethodPost, target, url.Values{consentLoginParam: {user.Email}, passwordParam: {user.Password}}, consentOrigin(target))
		if err := expectConsentStatus(reply, err, http.StatusSeeOther); err != nil && (reply == nil || reply.status != http.StatusFound) {
			return "", fmt.Errorf("identity provider login refused")
		}
		next, err := location.Parse(reply.header.Get("Location"))
		if err != nil {
			return "", err
		}
		target = next.String()
	}
	return "", fmt.Errorf("sign-in exceeded 20 redirects")
}

func (flow *consentProofFlow) checkRemembered(ctx context.Context) error {
	reply, err := consentRequest(ctx, flow.browser, http.MethodGet, flow.authorizationURL(""), nil, "")
	if err := expectConsentStatus(reply, err, http.StatusFound); err != nil {
		return err
	}
	_, _, err = flow.page(ctx, "consent")
	return err
}
