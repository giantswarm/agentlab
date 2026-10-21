package lab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/giantswarm/pagekit"

	"github.com/giantswarm/agentlab/internal/config"
)

// BrowserLogin runs the real OIDC authorization-code flow: opens the browser
// on Dex's own login screen, catches the callback on a fixed local port
// (pre-registered in Dex's redirectURIs, so it cannot be random), exchanges
// the code for tokens and writes kubeconfig.oidc.
//
// This is the flow to show a client — they see the Dex login page and type
// credentials for a user that exists nowhere but this cluster.
func BrowserLogin(cfg *config.Config) error {
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", config.BrowserCallbackPort)
	stateBuf := make([]byte, 16)
	if _, err := rand.Read(stateBuf); err != nil {
		return err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBuf)

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", browserCallback(state, codeCh))

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.BrowserCallbackPort))
	if err != nil {
		return fmt.Errorf("callback port busy: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	authURL := cfg.Issuer() + "/auth?" + url.Values{
		oauthClientID:    {config.KubernetesClientID},
		oauthRedirectURI: {redirect},
		"response_type":  {"code"},
		"scope":          {"openid email profile groups offline_access"},
		"state":          {state},
	}.Encode()

	fmt.Println("Opening the Dex login page in your browser...")
	fmt.Println("  users and passwords are in agentlab.yaml")
	fmt.Printf("  if it does not open: %s\n\n", authURL)
	OpenBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for the browser callback")
	}

	token, err := dexToken(cfg, config.KubernetesClientID, config.KubernetesClientSecret, url.Values{
		grantTypeParam:   {"authorization_code"},
		oauthCodeParam:   {code},
		oauthRedirectURI: {redirect},
	})
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	return saveLoginArtifacts(cfg, token)
}

// browserCallback answers Dex's redirect: it hands a code that comes with
// the expected state to codeCh and shows the outcome in the browser.
func browserCallback(state string, codeCh chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			renderBrowserFailure(w, "The sign-in was denied or failed", e)
			return
		}
		if q.Get("state") != state {
			renderBrowserFailure(w, "The sign-in response could not be verified", "")
			return
		}
		if q.Get(oauthCodeParam) == "" {
			renderBrowserFailure(w, "The sign-in response is incomplete", "")
			return
		}
		select {
		case codeCh <- q.Get(oauthCodeParam):
		default: // a repeated load of the callback; the first code counts
		}
		pagekit.Render(w, http.StatusOK, pagekit.Page{
			Title:   "Signed in to agentlab",
			Heading: "Signed in",
			Summary: "agentlab is now signed in",
			Message: "You can close this tab and return to the terminal.",
		})
	}
}

// renderBrowserFailure writes the page for a failed sign-in. errorCode is
// shown only when it is a registered OAuth error code.
func renderBrowserFailure(w http.ResponseWriter, summary, errorCode string) {
	pagekit.Render(w, http.StatusBadRequest, pagekit.Page{
		Title:   "Sign-in to agentlab failed",
		Heading: "Sign-in failed",
		Summary: summary,
		Message: "You can close this tab and run the login again in the terminal.",
		Detail:  pagekit.OAuthErrorCode(errorCode),
	})
}

// OpenBrowser hands a URL (or, on macOS/Linux, any path) to the OS opener.
// Fire-and-forget: a browser that fails to open is an inconvenience, not an
// error the flow can act on — every caller prints the URL as well.
func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case goosDarwin:
		cmd = exec.Command("open", url) // #nosec G204 -- fixed browser-opener command; the URL is lab-local
	case "windows":
		// The shell's own handler; `start` is a cmd.exe builtin, not a binary.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204 -- fixed browser-opener command; the URL is lab-local
	default:
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- fixed browser-opener command; the URL is lab-local
	}
	_ = cmd.Start()
}
