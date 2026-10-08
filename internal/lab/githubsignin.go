package lab

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab Dex's `github` connector (dex.yaml.tmpl, docs/github-signin.md):
// GitHub as a second identity source next to the static users, through a
// GitHub App whose callback is the lab Dex's own callback URL. The client id
// is public and comes from agentlab.yaml; the client secret is the Secret
// platform.githubSignIn.secret names in the Dex namespace, which the
// operator's secret tooling places (`beekeeper secret copy <ref> --to-secret
// kind-<cluster>/dex/<name>/client-secret`) and the Dex pod reads as the
// environment variable the rendered connector references. agentlab reads
// which keys the Secret carries and never a value: nothing of the client
// secret reaches agentlab.yaml, state/, a log line or a process's argv.

// gitHubSignInConnectorID is the connector's id, the `connector_id` claim a
// token signed in through it carries. Not an installation's
// `giantswarm-github`: Backstage's sign-in resolver looks that one up in the
// catalog, which the lab ingests nothing from GitHub into, and falls through
// to the email's local part for any other id (backstage-catalog.yaml.tmpl).
const gitHubSignInConnectorID = "github"

// gitHubSignInValues is what dex.yaml.tmpl renders for the connector.
type gitHubSignInValues struct {
	ConnectorID string
	ClientID    string
	// SecretName is the client Secret in the Dex namespace and SecretKey the
	// key the pod's SecretEnv variable reads; the Dex config references the
	// variable, never the value.
	SecretName, SecretKey, SecretEnv string
	// SecretVersion is the Secret's resourceVersion while ApplyDex knows it,
	// stamped on the pod template so a rotated client secret rolls Dex,
	// which reads the variable at start. Empty in an offline render.
	SecretVersion string
	RedirectURI   string
	Orgs          []string
}

// gitHubSignInValuesFor is the connector as agentlab.yaml configures it,
// nil while the sign-in is off.
func gitHubSignInValuesFor(cfg *config.Config) *gitHubSignInValues {
	if !cfg.GitHubSignInEnabled() {
		return nil
	}
	s := cfg.Platform.GitHubSignIn
	return &gitHubSignInValues{
		ConnectorID: gitHubSignInConnectorID,
		ClientID:    s.ClientID,
		SecretName:  s.ClientSecret().Name,
		SecretKey:   config.GitHubClientSecretKey,
		SecretEnv:   config.GitHubSignInSecretEnv,
		RedirectURI: cfg.GitHubSignInCallbackURL(),
		Orgs:        s.Orgs,
	}
}

// gitHubSignInFor is the connector ApplyDex renders: the configured values
// once the client Secret is in place, with its version. Without the Secret,
// or with its key missing, the connector stays out of this apply and the lab
// keeps its local users: a pod whose variable has no Secret behind it would
// not start, and Dex is every login. The run says which Secret and key it
// looked for and how to place it; the next up, platform or reload renders
// the connector in. Nil while the sign-in is off.
func gitHubSignInFor(cfg *config.Config) (*gitHubSignInValues, error) {
	values := gitHubSignInValuesFor(cfg)
	if values == nil {
		return nil, nil
	}
	ref := cfg.Platform.GitHubSignIn.ClientSecret()
	obj, err := getObject(context.Background(), gvrSecrets, ref.Namespace, ref.Name)
	var missing []string
	switch {
	case apierrors.IsNotFound(err):
		missing = []string{"the Secret itself"}
	case err != nil:
		return nil, err
	default:
		missing = missingSecretKeys(obj, config.GitHubClientSecretKey)
	}
	if len(missing) > 0 {
		warn("platform.githubSignIn is on but Secret %s/%s lacks %s -- Dex keeps its local users; the GitHub connector follows on the next up, platform or reload", ref.Namespace, ref.Name, strings.Join(missing, " and "))
		warn("  register (or reuse) a GitHub App with the callback URL %s and place its client secret, which agentlab never reads:", values.RedirectURI)
		warn("  beekeeper secret copy <ref to the client secret> --to-secret kind-%s/%s/%s/%s", cfg.ClusterName, ref.Namespace, ref.Name, config.GitHubClientSecretKey)
		return nil, nil
	}
	values.SecretVersion = obj.GetResourceVersion()
	return values, nil
}
