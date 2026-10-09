package lab

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// The workspace proofs' GitHub (githubfake_workspaces.go) holds what its
// fixture names: an App with an installation per owner, the owners'
// repositories with the fields a listing filters on, and the lab users with
// the repositories each reads or pushes to (docs/workspaces.md).

//go:embed templates/github-fixture.yaml
var defaultGitHubFixture []byte

// The permissions a fixture grants a user on a repository.
const (
	githubPermRead  = "read"
	githubPermWrite = "write"
)

// The owner types GitHub names.
const (
	githubOrganization = "Organization"
	githubUser         = "User"
)

type githubFixture struct {
	App          githubFixtureApp     `yaml:"app"`
	Owners       []githubFixtureOwner `yaml:"owners"`
	Repositories []githubFixtureRepo  `yaml:"repositories"`
	Users        []githubFixtureUser  `yaml:"users"`
}

type githubFixtureApp struct {
	ID       int64  `yaml:"id"`
	Slug     string `yaml:"slug"`
	ClientID string `yaml:"clientID"`
}

// githubFixtureOwner is an organization or a user account; Installation is
// the App's installation id on it, 0 for none.
type githubFixtureOwner struct {
	Login        string `yaml:"login"`
	Type         string `yaml:"type"`
	Installation int64  `yaml:"installation"`
}

// githubFixtureRepo is one repository on its default branch: Files, plus
// GeneratedMiB of incompressible content for the measurements.
type githubFixtureRepo struct {
	Owner         string            `yaml:"owner"`
	Name          string            `yaml:"name"`
	Private       bool              `yaml:"private"`
	Archived      bool              `yaml:"archived"`
	Fork          bool              `yaml:"fork"`
	Language      string            `yaml:"language"`
	Topics        []string          `yaml:"topics"`
	DefaultBranch string            `yaml:"defaultBranch"`
	PushedAt      time.Time         `yaml:"pushedAt"`
	GeneratedMiB  int               `yaml:"generatedMiB"`
	Files         map[string]string `yaml:"files"`
}

func (r githubFixtureRepo) fullName() string { return r.Owner + "/" + r.Name }

// githubFixtureUser is a lab user and, per repository (owner/name), read or
// write; a public repository needs no entry to be read.
type githubFixtureUser struct {
	Login        string            `yaml:"login"`
	Name         string            `yaml:"name"`
	Email        string            `yaml:"email"`
	Repositories map[string]string `yaml:"repositories"`
}

// loadGitHubFixture reads the fixture at path, the embedded default for "".
func loadGitHubFixture(path string) (*githubFixture, error) {
	raw := defaultGitHubFixture
	if path != "" {
		var err error
		if raw, err = os.ReadFile(path); err != nil { // #nosec G304 -- the fixture file the command names
			return nil, fmt.Errorf("reading the GitHub fixture: %w", err)
		}
	}
	var f githubFixture
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing the GitHub fixture: %w", err)
	}
	return &f, f.validate()
}

func (f *githubFixture) validate() error {
	if f.App.ID == 0 || f.App.ClientID == "" {
		return fmt.Errorf("the GitHub fixture's app needs an id and a clientID")
	}
	owners := map[string]bool{}
	installations := map[int64]bool{}
	for _, o := range f.Owners {
		switch {
		case o.Login == "" || owners[o.Login]:
			return fmt.Errorf("the GitHub fixture's owner %q is empty or repeated", o.Login)
		case o.Type != githubOrganization && o.Type != githubUser:
			return fmt.Errorf("owner %s: type %q is neither %s nor %s", o.Login, o.Type, githubOrganization, githubUser)
		case o.Installation != 0 && installations[o.Installation]:
			return fmt.Errorf("owner %s: installation %d is repeated", o.Login, o.Installation)
		}
		owners[o.Login] = true
		installations[o.Installation] = true
	}
	repos := map[string]bool{}
	for i, r := range f.Repositories {
		switch {
		case !owners[r.Owner]:
			return fmt.Errorf("repository %s: owner %q is not in the fixture's owners", r.fullName(), r.Owner)
		case r.Name == "" || repos[r.fullName()]:
			return fmt.Errorf("repository %s is unnamed or repeated", r.fullName())
		case len(r.Files) == 0 && r.GeneratedMiB == 0:
			return fmt.Errorf("repository %s has no files", r.fullName())
		}
		if r.DefaultBranch == "" {
			f.Repositories[i].DefaultBranch = "main"
		}
		repos[r.fullName()] = true
	}
	logins := map[string]bool{}
	for _, u := range f.Users {
		if u.Login == "" || logins[u.Login] {
			return fmt.Errorf("the GitHub fixture's user %q is empty or repeated", u.Login)
		}
		logins[u.Login] = true
		for repo, perm := range u.Repositories {
			if !repos[repo] {
				return fmt.Errorf("user %s: repository %s is not in the fixture", u.Login, repo)
			}
			if perm != githubPermRead && perm != githubPermWrite {
				return fmt.Errorf("user %s: permission %q on %s is neither %s nor %s", u.Login, perm, repo, githubPermRead, githubPermWrite)
			}
		}
	}
	return nil
}

// --- the credentials the lab generates ---------------------------------------

// githubFakeCredentialsDir holds what the lab generates for the fake: its
// TLS pair from the lab CA, the App's private key (the provider signs its
// JWTs with it) and public key (the fake checks them with it), and the
// OAuth client secret. Like the CA key, nothing in it leaves the machine.
const githubFakeCredentialsDir = "certs/github-fake" // #nosec G101 -- a directory, not a credential

// The files of githubFakeCredentialsDir.
const (
	githubFakeTLSCert      = "tls.crt"
	githubFakeTLSKey       = "tls.key"
	githubFakeAppKey       = "app.pem"
	githubFakeAppPublicKey = "app.pub"
	githubFakeClientSecret = "client-secret"
)

// githubFakeHost is the fake's name under the platform domain: the lab CA's
// name constraints admit nothing outside it but loopback.
func githubFakeHost(domain string) string { return "github." + domain }

// EnsureGitHubFakeCredentials generates githubFakeCredentialsDir for the
// platform domain: the TLS leaf re-minted whenever leaf policy says so (the
// CA rotated, expiry, the domain), the App key pair and the client secret
// once. It returns the directory.
func EnsureGitHubFakeCredentials(domain string) (string, error) {
	if err := os.MkdirAll(githubFakeCredentialsDir, 0o750); err != nil {
		return "", err
	}
	caCert, caKey, err := loadCA()
	if err != nil {
		return "", err
	}
	certPath := filepath.Join(githubFakeCredentialsDir, githubFakeTLSCert)
	dns := []string{githubFakeHost(domain)}
	if reason := leafRemintReason(readCertFile(certPath), caCert, dns); reason != "" {
		if err := mintLeaf(caCert, caKey, dns[0], dns, []net.IP{net.ParseIP("127.0.0.1")},
			certPath, filepath.Join(githubFakeCredentialsDir, githubFakeTLSKey)); err != nil {
			return "", err
		}
		fmt.Printf("Generated %s (%s; SAN: DNS:%s, IP:127.0.0.1)\n", certPath, reason, dns[0])
	}
	keyPath := filepath.Join(githubFakeCredentialsDir, githubFakeAppKey)
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", err
		}
		pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			return "", err
		}
		// GitHub hands an App's key out as PKCS #1, which providers parse.
		if err := writePEM(keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), 0o600); err != nil {
			return "", err
		}
		if err := writePEM(filepath.Join(githubFakeCredentialsDir, githubFakeAppPublicKey), "PUBLIC KEY", pub, 0o644); err != nil {
			return "", err
		}
		fmt.Printf("Generated the fake GitHub App's key pair in %s\n", githubFakeCredentialsDir)
	}
	secretPath := filepath.Join(githubFakeCredentialsDir, githubFakeClientSecret)
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		if err := os.WriteFile(secretPath, []byte(randomHex(20)), 0o600); err != nil {
			return "", err
		}
		fmt.Printf("Generated the fake GitHub App's client secret in %s\n", githubFakeCredentialsDir)
	}
	return githubFakeCredentialsDir, nil
}

// randomHex is n random bytes, hex-encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fixtureUser is the user of login; ok false for none.
func (f *githubFixture) fixtureUser(login string) (githubFixtureUser, bool) {
	i := slices.IndexFunc(f.Users, func(u githubFixtureUser) bool { return u.Login == login })
	if i < 0 {
		return githubFixtureUser{}, false
	}
	return f.Users[i], true
}
