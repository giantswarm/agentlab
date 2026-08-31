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
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code") == "" {
			http.Error(w, "no code in callback", http.StatusBadRequest)
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, "<html><body style='font-family:sans-serif;padding:3rem'>"+
			"<h2>Logged in.</h2><p>You can close this tab and go back to the terminal.</p>"+
			"</body></html>")
		codeCh <- q.Get("code")
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.BrowserCallbackPort))
	if err != nil {
		return fmt.Errorf("callback port busy: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	authURL := cfg.Issuer() + "/auth?" + url.Values{
		"client_id":     {config.KubernetesClientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {"openid email profile groups offline_access"},
		"state":         {state},
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
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirect},
	})
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	return saveLoginArtifacts(cfg, token)
}

// OpenBrowser hands a URL (or, on macOS/Linux, any path) to the OS opener.
// Fire-and-forget: a browser that fails to open is an inconvenience, not an
// error the flow can act on.
func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url) // #nosec G204 -- fixed browser-opener command; the URL is lab-local
	default:
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- fixed browser-opener command; the URL is lab-local
	}
	_ = cmd.Start()
}
