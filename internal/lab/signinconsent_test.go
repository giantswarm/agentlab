package lab

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApproveConsentPage(t *testing.T) {
	var origin, consentID, decision string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><form method="POST" action="/oauth/authorize">
<input type="hidden" name="consent_id" value="c-1"><button name="decision" value="approve">Allow</button>
<button name="decision" value="deny">Deny</button></form></html>`)
	})
	mux.HandleFunc("POST /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		origin, consentID, decision = r.Header.Get("Origin"), r.FormValue("consent_id"), r.FormValue("decision")
		http.Redirect(w, r, "/dex/auth/local/login", http.StatusFound)
	})
	mux.HandleFunc("GET /dex/auth/local/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "login form")
	})
	mux.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<form method="POST" action="/x"><input name="login"></form>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := &http.Client{Jar: jar}

	get := func(path string) (*http.Response, []byte) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		require.NoError(t, err)
		resp, err := browser.Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp, body
	}

	t.Run("a consent page is approved and its redirect followed", func(t *testing.T) {
		resp, body := get("/oauth/authorize")
		resp, body, err := approveConsentPage(browser, resp, body)
		require.NoError(t, err)
		require.Equal(t, "/dex/auth/local/login", resp.Request.URL.Path)
		require.Equal(t, "login form", string(body))
		require.Equal(t, server.URL, origin)
		require.Equal(t, "c-1", consentID)
		require.Equal(t, consentApprove, decision)
	})

	t.Run("any other page comes back as it was", func(t *testing.T) {
		resp, body := get("/plain")
		got, gotBody, err := approveConsentPage(browser, resp, body)
		require.NoError(t, err)
		require.Same(t, resp, got)
		require.Equal(t, body, gotBody)
	})
}
