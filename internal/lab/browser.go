package lab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"dexlab/internal/config"
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
		fmt.Fprint(w, "<html><body style='font-family:sans-serif;padding:3rem'>"+
			"<h2>Logged in.</h2><p>You can close this tab and go back to the terminal.</p>"+
			"</body></html>")
		codeCh <- q.Get("code")
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.BrowserCallbackPort))
	if err != nil {
		return fmt.Errorf("callback port busy: %w", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	authURL := cfg.Issuer() + "/auth?" + url.Values{
		"client_id":     {"kubernetes"},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {"openid email profile groups offline_access"},
		"state":         {state},
	}.Encode()

	fmt.Println("Opening the Dex login page in your browser...")
	fmt.Println("  users and passwords are in dexlab.yaml")
	fmt.Printf("  if it does not open: %s\n\n", authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for the browser callback")
	}

	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirect},
	}
	req, err := http.NewRequest(http.MethodPost, cfg.Issuer()+"/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth("kubernetes", config.KubernetesClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return fmt.Errorf("token exchange failed: %s", strings.TrimSpace(string(body)))
	}

	claims, err := decodeJWTClaims(tok.IDToken)
	if err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println("id_token claims:")
	fmt.Println(string(pretty))

	if err := os.WriteFile(".token", []byte(tok.IDToken), 0o600); err != nil {
		return err
	}
	if err := writeTokenKubeconfig(cfg, tok.IDToken, "kubeconfig.oidc"); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	fmt.Printf("\nLogged in as %v.\n", claims["email"])
	fmt.Printf("  export KUBECONFIG=%s/kubeconfig.oidc\n", cwd)
	fmt.Println("  kubectl auth whoami")
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
