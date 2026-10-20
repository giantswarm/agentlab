package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's apiserver verifies Dex tokens against a JWKS it caches for the
// max-age Dex sends (10 minutes) and fetches again for an unknown key ID only
// within 30 seconds of that expiry (docs/identity.md, "Key rotation"). After
// Dex rotates its signing key — on schedule, or at its first start after the
// key expired while the node was down — every token signed with the new key
// is Unauthorized until the cached set expires, up to 9 min 30 s after the
// apiserver's last fetch. ensureApiserverAcceptsDex tells that window from
// any other refusal and waits it out.

// apiserverAcceptsDex is the apiserver OIDC stage: every proof that presents
// a Dex token to the apiserver, to muster or to the Dev Portal runs it before
// its first one (platform-test, test, backstage-test, agents-test). A seam for
// the proofs' tests.
var apiserverAcceptsDex = ensureApiserverAcceptsDex

// checkApiserverOIDC runs the apiserver OIDC stage as a step of a proof
// without proofStages.
func checkApiserverOIDC(cfg *config.Config) error {
	step("%s: checking the apiserver accepts a fresh Dex token", stageApiserverOIDC)
	return apiserverAcceptsDex(context.Background(), cfg)
}

// apiserverJWKSWindow bounds the wait: Dex's max-age plus a margin for the
// first token after the expiry to make the fetch.
const apiserverJWKSWindow = 10*time.Minute + 30*time.Second

// apiserverJWKSPoll is how often a waiting check presents a new token.
const apiserverJWKSPoll = 15 * time.Second

// oidcVerdict is what a Dex token's answer at the apiserver means.
type oidcVerdict int

const (
	// oidcAccepted: the apiserver authenticated the token.
	oidcAccepted oidcVerdict = iota
	// oidcStaleKeys: Dex publishes the key the token is signed with and the
	// apiserver still refuses it — its cached JWKS predates the key.
	oidcStaleKeys
	// oidcRefused: any other failure, the lab's to report.
	oidcRefused
)

// judgeApiserverOIDC reads a whoami answer for a token signed with kid against
// the key IDs Dex publishes. Only an Unauthorized for a key Dex publishes is
// the apiserver's stale cache; an Unauthorized for a key Dex does not publish
// is Dex's state, and every other error is no authentication answer at all.
func judgeApiserverOIDC(kid string, published []string, whoamiErr error) oidcVerdict {
	switch {
	case whoamiErr == nil:
		return oidcAccepted
	case apierrors.IsUnauthorized(whoamiErr) && kid != "" && slices.Contains(published, kid):
		return oidcStaleKeys
	default:
		return oidcRefused
	}
}

// ensureApiserverAcceptsDex asks the apiserver who a fresh Dex token of the
// first configured user is. While the apiserver's cached JWKS predates Dex's
// signing key (judgeApiserverOIDC) it presents a new token every
// apiserverJWKSPoll until the cache expires and the token is accepted, for at
// most apiserverJWKSWindow; any other refusal fails at once.
func ensureApiserverAcceptsDex(ctx context.Context, cfg *config.Config) error {
	if len(cfg.Users) == 0 {
		return nil
	}
	user := cfg.Users[0]
	start := time.Now()
	waiting := false
	for {
		verdict, detail, err := apiserverOIDCCheck(ctx, cfg, user)
		if err != nil {
			return err
		}
		switch verdict {
		case oidcAccepted:
			if waiting {
				note("the apiserver fetched Dex's keys again and accepts a fresh Dex token (%s, after %s)", detail, time.Since(start).Round(time.Second))
			} else {
				note("the apiserver accepts a fresh Dex token (%s)", detail)
			}
			return nil
		case oidcRefused:
			return fmt.Errorf("the apiserver refuses a fresh Dex token of %s: %s", user.Email, detail)
		}
		if time.Since(start) > apiserverJWKSWindow {
			return fmt.Errorf("the apiserver still refuses Dex tokens signed with a key Dex publishes after %s, longer than its JWKS cache lives (%s): %s; `kubectl --kubeconfig state/kubeconfig get --raw /metrics | grep jwks_fetch` names its last fetch",
				time.Since(start).Round(time.Second), apiserverJWKSWindow, detail)
		}
		if !waiting {
			step("The apiserver refuses Dex tokens signed with a key Dex publishes (%s): Dex rotated its keys after the apiserver's last JWKS fetch, which it repeats once its cached set expires — waiting up to %s", detail, apiserverJWKSWindow)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(apiserverJWKSPoll):
		}
	}
}

// apiserverOIDCCheck mints a token for user at the apiserver's client, reads
// the key IDs Dex publishes and judges the apiserver's whoami answer; detail
// names the identity or the refusal.
func apiserverOIDCCheck(ctx context.Context, cfg *config.Config, user config.User) (oidcVerdict, string, error) {
	token, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret,
		user.Email, user.Password, "openid email profile groups")
	if err != nil {
		return 0, "", fmt.Errorf("could not get a Dex token for %s: %w", user.Email, err)
	}
	kid, err := jwtKeyID(token)
	if err != nil {
		return 0, "", fmt.Errorf("reading the Dex token's key ID: %w", err)
	}
	published, err := dexKeyIDs(cfg)
	if err != nil {
		return 0, "", err
	}
	rc, err := tokenConfig(token)
	if err != nil {
		return 0, "", err
	}
	readCtx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	username, _, whoamiErr := whoAmI(readCtx, rc)
	verdict := judgeApiserverOIDC(kid, published, whoamiErr)
	if verdict == oidcAccepted {
		return verdict, username, nil
	}
	return verdict, fmt.Sprintf("key %s, Dex publishes %s; %v", kid, strings.Join(published, ", "), whoamiErr), nil
}

// jwtKeyID is the kid of a compact JWT's header.
func jwtKeyID(token string) (string, error) {
	header, _, ok := strings.Cut(token, ".")
	if !ok {
		return "", fmt.Errorf("not a compact JWT")
	}
	var h struct {
		KeyID string `json:"kid"`
	}
	if err := decodeJWTPart(header, &h); err != nil {
		return "", err
	}
	return h.KeyID, nil
}

// dexKeyIDs are the key IDs of the lab Dex's JWKS, read from the issuer's keys
// endpoint, where the apiserver reads them.
func dexKeyIDs(cfg *config.Config) ([]string, error) {
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(cfg.Issuer(), "/") + "/keys"
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("reading Dex's keys at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading Dex's keys at %s: HTTP %d", url, resp.StatusCode)
	}
	return parseJWKSKeyIDs(body)
}

// parseJWKSKeyIDs lists the kid of every key of a JWKS document.
func parseJWKSKeyIDs(body []byte) ([]string, error) {
	var jwks struct {
		Keys []struct {
			KeyID string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("parsing Dex's JWKS: %w", err)
	}
	ids := make([]string, 0, len(jwks.Keys))
	for _, k := range jwks.Keys {
		ids = append(ids, k.KeyID)
	}
	return ids, nil
}
