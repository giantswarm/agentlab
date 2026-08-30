package lab

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"agentlab/internal/config"
)

// labTLSTransport returns a transport that trusts the lab CA (certs/ca.crt)
// on top of the system pool, so it can talk to both Dex (lab TLS) and
// plain-HTTP services. Success is cached per process (the CA never changes
// within a run, and sharing the transport reuses connections), but errors are
// not: the TUI probes long before `agentlab up` has minted the CA and must
// pick it up as soon as the file exists.
var labTransport struct {
	sync.Mutex
	cached *http.Transport
}

func labTLSTransport() (*http.Transport, error) {
	labTransport.Lock()
	defer labTransport.Unlock()
	if labTransport.cached != nil {
		return labTransport.cached, nil
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("reading lab CA (run `agentlab up` first?): %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("certs/ca.crt contains no usable certificate")
	}
	labTransport.cached = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return labTransport.cached, nil
}

// labHTTPClient returns a client on the shared lab transport.
func labHTTPClient(timeout time.Duration) (*http.Client, error) {
	transport, err := labTLSTransport()
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// dexToken POSTs a token request (any grant type, in form) to the lab Dex and
// returns the raw id_token.
func dexToken(cfg *config.Config, clientID, clientSecret string, form url.Values) (string, error) {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.Issuer()+"/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		msg := strings.TrimSpace(string(body))
		if tok.Error != "" {
			msg = tok.Error + ": " + tok.ErrorDescription
		}
		return "", fmt.Errorf("%s", msg)
	}
	return tok.IDToken, nil
}

// grantTypePassword is the OAuth2 Resource Owner Password Credentials grant.
const grantTypePassword = "password"

// passwordParam is the password field name shared by Dex's token endpoint
// (ROPC form) and its login form.
const passwordParam = "password"

// passwordGrant performs the OAuth2 Resource Owner Password grant against the
// lab Dex (enabled by oauth2.passwordConnector: local) and returns the raw
// id_token. This is what makes headless/CI testing a single HTTP call.
func passwordGrant(cfg *config.Config, clientID, clientSecret, email, password, scope string) (string, error) {
	token, err := dexToken(cfg, clientID, clientSecret, url.Values{
		"grant_type":  {grantTypePassword},
		"username":    {email},
		passwordParam: {password},
		"scope":       {scope},
	})
	if err != nil {
		return "", fmt.Errorf("dex login failed for %s: %w", email, err)
	}
	return token, nil
}

// decodeJWTClaims returns the (unverified) payload of a JWT as a JSON object.
// Verification is the apiserver's and muster's job; this is display plumbing.
func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding JWT payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// httpUp reports whether a URL answers with a 2xx (the curl -sf equivalent).
func httpUp(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
