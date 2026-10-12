package lab

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// With the workspaces and the agents on, the values carry the whole rig:
// the manager's signing key by Secret reference, kagent's jwt admission of
// the manager's key set through its Service with the manager's base URL as
// the issuer, and the claude Harness with a snapshot location of its own.
func TestWorkspaceAdmissionValues(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("")
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)

	wm, _ := workspaceManagerBlock(t, values)
	grant, _ := wm["grant"].(map[string]any)
	wantSecretRef(t, "workspace-manager.grant.signingKey", grant["signingKey"], WorkspaceGrantSecretName, workspaceGrantSecretKey)

	kagent, _ := values["kagent"].(map[string]any)
	controller, _ := kagent["controller"].(map[string]any)
	session, _ := controller["session"].(map[string]any)
	admission, _ := session["admission"].(map[string]any)
	jwt, _ := admission["jwt"].(map[string]any)
	for key, want := range map[string]string{
		"jwksURL":  "http://workspace-manager.agent-platform.svc:8080/.well-known/jwks.json",
		"issuer":   "https://workspace-manager.127.0.0.1.nip.io:8443",
		"audience": workspaceGrantAudience,
	} {
		if jwt[key] != want {
			t.Errorf("kagent.controller.session.admission.jwt.%s = %v, want %s", key, jwt[key], want)
		}
	}
	// The lab's other controller settings stay beside it.
	if auth, _ := controller["auth"].(map[string]any); auth["mode"] != "trusted-proxy" {
		t.Errorf("kagent.controller.auth.mode = %v, want trusted-proxy", auth["mode"])
	}

	harnesses, _ := kagent["harnesses"].([]any)
	if len(harnesses) != 1 {
		t.Fatalf("kagent.harnesses = %v, want the claude Harness alone", harnesses)
	}
	claude, _ := harnesses[0].(map[string]any)
	if claude["name"] != claudeHarness || claude["runtime"] != "claude" || claude["snapshotLocation"] != claudeHarnessSnapshotLocation {
		t.Errorf("kagent.harnesses[0] = %v, want %s on the claude runtime at %s", claude, claudeHarness, claudeHarnessSnapshotLocation)
	}
	if platformHarness, _ := kagent["harness"].(map[string]any); platformHarness["snapshotLocation"] == claude["snapshotLocation"] {
		t.Errorf("the claude Harness shares the platform Harness's snapshot location %v", claude["snapshotLocation"])
	}
}

// Without the agents, or with the workspaces off, nothing of the rig
// renders: no signing key, no admission, no claude Harness.
func TestWorkspaceAdmissionInert(t *testing.T) {
	stubWorkspacesProbe(t, true)
	noAgents := workspaceLab("")
	noAgents.Platform.Agents = false
	off := workspaceLab("")
	off.Platform.Workspaces.Enabled = false
	for name, c := range map[string]*config.Config{"agents off": noAgents, "workspaces off": off} {
		t.Run(name, func(t *testing.T) {
			if rig := workspaceAdmissionValuesFor(c, workspacesChartCarries(c)); rig != nil {
				t.Fatalf("rig = %+v, want none", rig)
			}
			_, raw, _, _ := renderWorkspaceTemplates(t, c, nil)
			for _, s := range []string{"signingKey", "admission:", "harnesses:", WorkspaceGrantSecretName} {
				if strings.Contains(raw, s) {
					t.Errorf("the values carry %q:\n%s", s, excerptAround(raw, s))
				}
			}
		})
	}
	// A chart that takes no workspaces values gets no rig either: its
	// schema refuses the workspace-manager's grant and kagent knows no
	// manager to trust.
	stubWorkspacesProbe(t, false)
	cfg := workspaceLab("")
	if rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg)); rig != nil {
		t.Fatalf("rig on a chart without the workspaces values = %+v, want none", rig)
	}
}

// The key fixture: generated once as a PKCS #8 EC P-256 key, kept on the
// next call, and its key id the RFC 7638 thumbprint the manager publishes it
// under.
func TestEnsureWorkspaceGrantKey(t *testing.T) {
	t.Chdir(t.TempDir())
	path, err := EnsureWorkspaceGrantKey()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key's mode is %v, want 0600", info.Mode().Perm())
	}
	first, err := readWorkspaceGrantKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkspaceGrantKey(); err != nil {
		t.Fatal(err)
	}
	second, err := readWorkspaceGrantKey()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Error("a second call replaced the key")
	}
	raw, _ := os.ReadFile(filepath.Clean(path))
	if block, _ := pem.Decode(raw); block == nil || block.Type != pemTypeKey {
		t.Errorf("the key is not a PKCS #8 PEM: %q", raw)
	}
}

// RFC 7638's own example is an RSA key; the EC thumbprint is checked against
// the canonical member order computed independently.
func TestWorkspaceGrantJWKThumbprint(t *testing.T) {
	key := testGrantKey(t)
	jwk := workspaceGrantJWK(&key.PublicKey)
	canonical, err := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{"P-256", "EC", jwk.X, jwk.Y})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); jwk.Kid != want {
		t.Errorf("kid = %s, want %s", jwk.Kid, want)
	}
	if x, _ := base64.RawURLEncoding.DecodeString(jwk.X); len(x) != 32 {
		t.Errorf("x is %d bytes, want 32", len(x))
	}
}

// The lab's key is found in a key set beside another key, under the id the
// set gives it; a set without it names what it holds.
func TestPublishedWorkspaceGrantKey(t *testing.T) {
	key, other := testGrantKey(t), testGrantKey(t)
	jwk, otherJWK := workspaceGrantJWK(&key.PublicKey), workspaceGrantJWK(&other.PublicKey)
	jwk.Kid = "published-id"
	set, _ := json.Marshal(map[string]any{"keys": []ecJWK{otherJWK, jwk}})
	kid, err := publishedWorkspaceGrantKey(set, &key.PublicKey)
	if err != nil || kid != "published-id" {
		t.Fatalf("kid %q, err %v; want published-id", kid, err)
	}
	set, _ = json.Marshal(map[string]any{"keys": []ecJWK{otherJWK}})
	if _, err := publishedWorkspaceGrantKey(set, &key.PublicKey); err == nil || !strings.Contains(err.Error(), otherJWK.Kid) {
		t.Errorf("err = %v, want the set's key ids named", err)
	}
}

// A signed grant is a compact ES256 JWS: the header names the key id, the
// payload carries the manager's claims, and the signature verifies with the
// public key.
func TestSignWorkspaceGrant(t *testing.T) {
	key := testGrantKey(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	grant := newWorkspaceGrant("https://wm.example", "admin@lab.local", "ws", workspaceVolume{Driver: "nfs.csi.k8s.io", Handle: "h#1"}, now, 5*time.Minute)
	token, err := signWorkspaceGrant(key, "kid-1", grant)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var header map[string]string
	decodeSegment(t, parts[0], &header)
	if header["alg"] != "ES256" || header["kid"] != "kid-1" || header["typ"] != "JWT" {
		t.Errorf("header = %v", header)
	}
	var claims map[string]any
	decodeSegment(t, parts[1], &claims)
	if claims["iss"] != "https://wm.example" || claims["sub"] != "admin@lab.local" || claims["exp"] != float64(now.Add(5*time.Minute).Unix()) {
		t.Errorf("claims = %v", claims)
	}
	if aud, _ := claims["aud"].([]any); len(aud) != 1 || aud[0] != workspaceGrantAudience {
		t.Errorf("aud = %v, want [kagent]", claims["aud"])
	}
	if v, _ := claims["volume"].(map[string]any); v["driver"] != "nfs.csi.k8s.io" || v["handle"] != "h#1" {
		t.Errorf("volume = %v", claims["volume"])
	}
	mounts, _ := json.Marshal(claims["mounts"])
	if string(mounts) != `[{"sub_path":"sessions/${SESSION_ID}"},{"read_only":true,"sub_path":"mirrors"}]` {
		t.Errorf("mounts = %s", mounts)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %d bytes, %v", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Error("the signature does not verify with the public key")
	}
}

// The kagent controller's admission settings match the rig's, or every
// difference is named.
func TestAdmissionMatches(t *testing.T) {
	rig := &workspaceAdmissionValues{JWKSURL: "http://wm/.well-known/jwks.json", Issuer: "https://wm", Audience: workspaceGrantAudience}
	good := map[string]string{admissionJWKSURLKey: rig.JWKSURL, admissionIssuerKey: rig.Issuer, admissionAudienceKey: rig.Audience}
	if err := admissionMatches(good, rig); err != nil {
		t.Fatal(err)
	}
	if err := admissionMatches(map[string]string{admissionAudienceKey: workspaceGrantAudience}, rig); err == nil ||
		!strings.Contains(err.Error(), admissionJWKSURLKey) || !strings.Contains(err.Error(), admissionIssuerKey) {
		t.Errorf("err = %v, want both unset keys named", err)
	}
}

// The probe's marker lines parse whatever the agent wraps them in, and the
// verdicts tell a Session on the volume from one without.
func TestWorkspacesHarnessProbe(t *testing.T) {
	reply := "Here is the output:\n```\n" +
		"AGENTLAB_PROBE pwd=/workspace\n" +
		"AGENTLAB_PROBE mirrors=agentlab-org/platform-api.git \n" +
		"AGENTLAB_PROBE clones=agentlab-org/platform-api \n" +
		"AGENTLAB_PROBE mirrors-write=refused\n" +
		"AGENTLAB_PROBE own-write=session\n```"
	p, err := parseWorkspacesHarnessProbe(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.checkOnVolume(); err != nil {
		t.Error(err)
	}
	if err := p.checkWithoutVolume(); err == nil {
		t.Error("a Session on the volume passed as one without")
	}
	if _, err := p.checkClones(); err == nil {
		t.Error("a reply without the clone readings passed the session directory")
	}
	plain, err := parseWorkspacesHarnessProbe("AGENTLAB_PROBE pwd=/data/workspace\nAGENTLAB_PROBE mirrors=\nAGENTLAB_PROBE clones=\nAGENTLAB_PROBE mirrors-write=absent\nAGENTLAB_PROBE own-write=session")
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.checkWithoutVolume(); err != nil {
		t.Error(err)
	}
	if err := plain.checkOnVolume(); err == nil {
		t.Error("a Session without a volume passed as one on it")
	}
	writable := p
	writable.mirrorsWrite = "accepted"
	if err := writable.checkOnVolume(); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("err = %v, want the writable mirrors named", err)
	}
	if _, err := parseWorkspacesHarnessProbe("I could not run the command."); err == nil {
		t.Error("a reply without marker lines parsed")
	}
}

// The session directory's verdict: the agent's own clone of the mirror and
// the setup clone, each failure named with what git said.
func TestWorkspacesHarnessProbeClones(t *testing.T) {
	const base = "AGENTLAB_PROBE pwd=/workspace\nAGENTLAB_PROBE mirrors=agentlab-org/platform-api.git\nAGENTLAB_PROBE clones=agentlab-org/platform-api\n" +
		"AGENTLAB_PROBE mirrors-write=refused\nAGENTLAB_PROBE id=65532:65532\n"
	for _, tc := range []struct {
		name, lines string
		want        []string // in the reading, or in the error
		fails       bool
	}{
		{
			name:  "both clones",
			lines: "AGENTLAB_PROBE own-clone=rc0 head=ec17e3f Cloning into './agentlab-own-clone'... done.\nAGENTLAB_PROBE setup-clone=ec17e3f \n",
			want:  []string{"as uid:gid 65532:65532 at ec17e3f", "the setup clone ./agentlab-org/platform-api at ec17e3f"},
		},
		{
			name:  "own clone refused",
			lines: "AGENTLAB_PROBE own-clone=rc128 head= Cloning into './agentlab-own-clone'... fatal: detected dubious ownership in repository at '/mirrors/agentlab-org/platform-api.git'\nAGENTLAB_PROBE setup-clone=ec17e3f\n",
			want:  []string{"failed with exit 128", "dubious ownership", "65532:65532"},
			fails: true,
		},
		{
			name:  "no setup clone",
			lines: "AGENTLAB_PROBE own-clone=rc0 head=ec17e3f Cloning into './agentlab-own-clone'... done.\nAGENTLAB_PROBE setup-clone=fatal: cannot change to './agentlab-org/platform-api': No such file or directory\n",
			want:  []string{"holds no setup clone", "cannot change to"},
			fails: true,
		},
		{
			name:  "no readings",
			lines: "",
			want:  []string{"relayed no reading of its own clone", "holds no setup clone"},
			fails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseWorkspacesHarnessProbe(base + tc.lines + "AGENTLAB_PROBE own-write=session")
			if err != nil {
				t.Fatal(err)
			}
			if err := p.checkOnVolume(); err != nil {
				t.Fatal(err)
			}
			reading, err := p.checkClones()
			got := reading
			if err != nil {
				got = err.Error()
			}
			if (err != nil) != tc.fails {
				t.Fatalf("checkClones() = %q, %v; want a failure %t", reading, err, tc.fails)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not name %q", got, w)
				}
			}
		})
	}
}

// The session directory arrived with kagent 1.9.0, its prereleases
// included; a version that does not parse is held to it.
func TestSendsSessionDirectory(t *testing.T) {
	for version, want := range map[string]bool{
		"v1.8.1": false, "1.8.2-rc.1": false, "v1.9.0-rc.1": true, "v1.9.0": true,
		"v1.10.0": true, "v1.10.0-rc.8": true, "": true, "dev": true,
	} {
		if got := sendsSessionDirectory(version); got != want {
			t.Errorf("sendsSessionDirectory(%q) = %t, want %t", version, got, want)
		}
	}
}

func testGrantKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseWorkspaceGrantKey(pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func decodeSegment(t *testing.T, segment string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}
