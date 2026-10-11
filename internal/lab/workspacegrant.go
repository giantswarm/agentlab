package lab

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// A Session on a workspace volume is admitted with a workspace grant: a
// short-lived JWT the workspace-manager signs (authorize_workspace) and
// kagent's controller verifies against the manager's published key set
// before it mounts the volume. The platform renders neither half on its
// own: the manager needs a signing key in a Secret (grant.signingKey), and
// kagent's jwt Session admission needs the key set's URL, the issuer and the
// audience (controller.session.admission.jwt). The lab supplies both
// (HACKS.md U31):
//
//   - The signing key is the lab's own, an EC P-256 key generated once under
//     certs/workspace-grant/ like the lab's other credentials, placed as the
//     Secret agent-platform/workspace-manager-grant ahead of the install and
//     named in workspace-manager.grant.signingKey. The manager publishes its
//     public half at /.well-known/jwks.json, under the key's RFC 7638
//     thumbprint as its key id.
//   - kagent's admission trusts that key set through the manager's Service,
//     with the manager's base URL as the issuer (the grant's iss) and
//     `kagent` as the audience (the manager's default grant.audience).
//
// The workspaces proof signs its grants with the same key, in the manager's
// claims, so a grant it presents verifies exactly where a manager's would:
// through the key set the manager serves.

const (
	// workspaceGrantDir holds the lab's grant signing key, next to the CA.
	workspaceGrantDir = "certs/workspace-grant" // #nosec G101 -- a directory, not a credential
	// workspaceGrantKeyFile is the PEM private key (PKCS #8), the file the
	// Secret's entry is read from.
	workspaceGrantKeyFile = "grant.pem"
	// WorkspaceGrantSecretName and workspaceGrantSecretKey are the Secret in
	// the platform namespace the manager reads its signing key from, and the
	// entry that holds it.
	WorkspaceGrantSecretName = "workspace-manager-grant" // #nosec G101 -- Secret NAME, not a credential
	workspaceGrantSecretKey  = "grant"
	// workspaceGrantAudience is the grant's aud: the manager's default
	// grant.audience, the installation's kagent.
	workspaceGrantAudience = "kagent"
	// workspaceManagerJWKSPath is where the manager publishes the grant's
	// verification keys.
	workspaceManagerJWKSPath = "/.well-known/jwks.json"
	// workspaceGrantSessionDirectory and workspaceGrantMirrors are the
	// mounts a grant allows, as the manager writes them: the Session's own
	// directory read-write, `${SESSION_ID}` standing for the Session kagent
	// creates, and the mirrors read-only.
	workspaceGrantSessionDirectory = "sessions/${SESSION_ID}"
	workspaceGrantMirrors          = "mirrors"
)

// workspaceAdmissionValues is what the values template renders for the
// rig: the manager's signing key Secret, kagent's jwt admission, and the
// claude Harness a coding Session runs on.
type workspaceAdmissionValues struct {
	Secret, SecretKey          string
	JWKSURL, Issuer, Audience  string
	ClaudeHarness              string
	ClaudeHarnessSnapshotStore string
}

// workspaceAdmissionValuesFor is the rig for a lab with the workspaces and
// the agents on and a chart that takes the workspaces values; nil
// otherwise.
func workspaceAdmissionValuesFor(cfg *config.Config, workspacesChart bool) *workspaceAdmissionValues {
	if !cfg.WorkspacesEnabled() || !workspacesChart || !cfg.Platform.Agents {
		return nil
	}
	return &workspaceAdmissionValues{
		Secret:                     WorkspaceGrantSecretName,
		SecretKey:                  workspaceGrantSecretKey,
		JWKSURL:                    workspaceManagerServiceURL + workspaceManagerJWKSPath,
		Issuer:                     cfg.WorkspaceManagerBaseURL(),
		Audience:                   workspaceGrantAudience,
		ClaudeHarness:              claudeHarness,
		ClaudeHarnessSnapshotStore: claudeHarnessSnapshotLocation,
	}
}

// workspaceManagerServiceURL is the manager's Service in the platform
// namespace, where kagent's controller reads the key set: in-cluster, so
// no TLS and no edge.
const workspaceManagerServiceURL = "http://workspace-manager." + platformNamespace + ".svc:8080"

// claudeHarness is the lab's coding Harness: Claude Code run headless by the
// kagent line's claude-harness image (the kagent chart's harnesses[]), its
// snapshots beside the platform Harness's in the lab's in-cluster store.
const (
	claudeHarness                 = "claude"
	claudeHarnessSnapshotLocation = "s3://ate-snapshots/claude"
)

// EnsureWorkspaceGrantKey generates the lab's grant signing key once and
// returns its path; an existing key is kept, so grants and the published
// key id stay stable across installs.
func EnsureWorkspaceGrantKey() (string, error) {
	path := filepath.Join(workspaceGrantDir, workspaceGrantKeyFile)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(workspaceGrantDir, 0o750); err != nil {
		return "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	if err := writePEM(path, "PRIVATE KEY", der, 0o600); err != nil {
		return "", err
	}
	fmt.Printf("Generated the workspace grant signing key in %s\n", workspaceGrantDir)
	return path, nil
}

// readWorkspaceGrantKey reads the lab's grant signing key.
func readWorkspaceGrantKey() (*ecdsa.PrivateKey, error) {
	path := filepath.Join(workspaceGrantDir, workspaceGrantKeyFile)
	raw, err := os.ReadFile(path) // #nosec G304 -- the lab's own credentials directory
	if err != nil {
		return nil, fmt.Errorf("reading the workspace grant signing key: %w (`agentlab platform` generates it with the workspaces on)", err)
	}
	return parseWorkspaceGrantKey(raw)
}

// parseWorkspaceGrantKey parses a PEM EC P-256 private key, PKCS #8 or SEC 1.
func parseWorkspaceGrantKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("the workspace grant signing key is no PEM")
	}
	var key any
	var err error
	if block.Type == "EC PRIVATE KEY" {
		key, err = x509.ParseECPrivateKey(block.Bytes)
	} else {
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("parsing the workspace grant signing key: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("the workspace grant signing key is not an EC P-256 key")
	}
	return ec, nil
}

// ecJWK is a public EC key as a JWKS carries it.
type ecJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// workspaceGrantJWK is the public half of key with its RFC 7638 thumbprint
// as the key id, the id the manager publishes it under.
func workspaceGrantJWK(key *ecdsa.PublicKey) ecJWK {
	size := (key.Curve.Params().BitSize + 7) / 8
	jwk := ecJWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, size))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, size))),
	}
	// The thumbprint's input: the required members in lexicographic order,
	// no whitespace.
	canonical := `{"crv":"` + jwk.Crv + `","kty":"` + jwk.Kty + `","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	jwk.Kid = base64.RawURLEncoding.EncodeToString(sum[:])
	jwk.Alg = "ES256"
	return jwk
}

// publishedWorkspaceGrantKey finds key among the key set's: the key id it is
// published under, or an error naming what the set holds.
func publishedWorkspaceGrantKey(jwks []byte, key *ecdsa.PublicKey) (string, error) {
	var set struct {
		Keys []ecJWK `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		return "", fmt.Errorf("the key set is no JWKS: %w", err)
	}
	want := workspaceGrantJWK(key)
	var kids []string
	for _, k := range set.Keys {
		if k.Kty == want.Kty && k.Crv == want.Crv && k.X == want.X && k.Y == want.Y {
			return k.Kid, nil
		}
		kids = append(kids, k.Kid)
	}
	return "", fmt.Errorf("the key set's %d keys (%v) do not include the lab's grant key %s", len(set.Keys), kids, want.Kid)
}

// workspaceGrant is a grant's payload in the manager's claims, which are
// kagent's jwt admission's.
type workspaceGrant struct {
	Issuer    string              `json:"iss"`
	Subject   string              `json:"sub"`
	Audience  []string            `json:"aud"`
	IssuedAt  int64               `json:"iat"`
	NotBefore int64               `json:"nbf"`
	Expiry    int64               `json:"exp"`
	ID        string              `json:"jti"`
	Workspace map[string]string   `json:"workspace"`
	Volume    workspaceVolume     `json:"volume"`
	Mounts    []workspaceMount    `json:"mounts"`
	Changed   []string            `json:"changed,omitempty"`
	Providers []map[string]string `json:"providers,omitempty"`
}

type workspaceVolume struct {
	Driver string `json:"driver"`
	Handle string `json:"handle"`
}

type workspaceMount struct {
	SubPath  string `json:"sub_path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// newWorkspaceGrant is the grant the manager would sign for subject on the
// volume: the Session's own directory read-write and the mirrors read-only,
// valid for ttl from now.
func newWorkspaceGrant(issuer, subject, workspace string, volume workspaceVolume, now time.Time, ttl time.Duration) workspaceGrant {
	issued := now.UTC().Truncate(time.Second)
	return workspaceGrant{
		Issuer: issuer, Subject: subject, Audience: []string{workspaceGrantAudience},
		IssuedAt: issued.Unix(), NotBefore: issued.Unix(), Expiry: issued.Add(ttl).Unix(),
		ID:        randomHex(16),
		Workspace: map[string]string{"namespace": kagentNamespace, "name": workspace},
		Volume:    volume,
		Mounts: []workspaceMount{
			{SubPath: workspaceGrantSessionDirectory},
			{SubPath: workspaceGrantMirrors, ReadOnly: true},
		},
	}
}

// signWorkspaceGrant signs the grant with ES256 under kid, as a compact JWS.
func signWorkspaceGrant(key *ecdsa.PrivateKey, kid string, grant workspaceGrant) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS's ES256 signature: r and s, each left-padded to 32 bytes.
	sig := append(fixedBytes(r, 32), fixedBytes(s, 32)...)
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func fixedBytes(n *big.Int, size int) []byte {
	return n.FillBytes(make([]byte, size))
}

// workspaceGrantKeyUp places the lab's grant signing key, generated once, as
// the manager's Secret.
func workspaceGrantKeyUp() error {
	path, err := EnsureWorkspaceGrantKey()
	if err != nil {
		return err
	}
	return ensureSecretFromFiles(platformNamespace, WorkspaceGrantSecretName, map[string]string{workspaceGrantSecretKey: path})
}

// hint is the platform-up summary of the rig.
func (r *workspaceAdmissionValues) hint() string {
	return fmt.Sprintf("  A Session on a workspace volume: kagent's jwt admission trusts the workspace-manager's key set (%s, issuer %s, audience %s),\n"+
		"  signed with the lab's key (%s, the Secret %s/%s); the claude Harness %s runs it. Proof: `agentlab workspaces-test`.",
		r.JWKSURL, r.Issuer, r.Audience, filepath.Join(workspaceGrantDir, workspaceGrantKeyFile), platformNamespace, r.Secret, r.ClaudeHarness)
}

// readSessionAdmission words the live rig for the workspaces status line:
// what kagent's admission trusts, whether the manager's grant key Secret is
// there, and the claude Harness's readiness.
func readSessionAdmission(ctx context.Context) string {
	k, err := labKube()
	if err != nil {
		return ""
	}
	cm, err := k.clientset.CoreV1().ConfigMaps(kagentNamespace).Get(ctx, kagentControllerConfigMap, metav1.GetOptions{})
	if err != nil || cm.Data[admissionJWKSURLKey] == "" {
		return "Session admission off (kagent's jwt admission unset)"
	}
	key := "no grant key Secret " + WorkspaceGrantSecretName
	if _, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, WorkspaceGrantSecretName, metav1.GetOptions{}); err == nil {
		key = "grant key Secret " + WorkspaceGrantSecretName
	}
	harness := "no Harness " + claudeHarness
	if gvr, err := gvrFor("harnesses.api.kagent.dev"); err == nil {
		if obj, err := getObject(ctx, gvr, kagentNamespace, claudeHarness); err == nil {
			var h struct {
				Status agentStatus `json:"status"`
			}
			ready := stateNotRead
			if decodeObject(obj, &h) == nil {
				ready, _ = h.Status.condition("Ready")
			}
			harness = "Harness " + claudeHarness + " Ready=" + ready
		}
	}
	return fmt.Sprintf("Session admission: kagent trusts %s (issuer %s), %s, %s",
		cm.Data[admissionJWKSURLKey], cm.Data[admissionIssuerKey], key, harness)
}
