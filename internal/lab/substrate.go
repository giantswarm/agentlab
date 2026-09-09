package lab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// Substrate (kagent-dev/substrate) is kagent's actor runtime: WorkerPools of
// sandboxed (gVisor) worker pods that ate-controller schedules actors onto,
// ate-api-server as their control plane, atelet as the per-node agent and
// atenet as the actors' ingress/egress data plane. The dev channel's kagent
// (kagent main, API v2) runs every agent as a Substrate actor and cannot
// start without it, so the lab installs Substrate as cluster infrastructure
// ahead of the platform when platform.substrate.enabled says so
// (config.SubstrateEnabled — implied by the dev channel); the released
// kagent ignores it. docs/platform.md "Dev channel".
//
// Not a meta-chart component (yet): Substrate needs an imperative bootstrap
// the chart does not render — the CA and JWT pools its signers and
// ate-api-server mount, the trust anchor atenet reads and ate-api-server's
// authentication config (upstream: `kubectl-ate admin make-ca-pool` /
// `make-jwt-pool` plus a shell step). The lab performs that bootstrap itself
// (substratepools.go is the Go port of the two commands) and creates every
// bootstrap object BEFORE the chart, so the install is one waited
// upgrade-or-install instead of upstream's install / bootstrap / re-install
// dance. The gates it needs from the apiserver (PodCertificateRequest,
// ClusterTrustBundle, certificates.k8s.io/v1beta1) are in the lab's kind
// config for every cluster (kind-config.yaml.tmpl).
//
// Pinned like the observability charts: Go consts, bumped deliberately, with
// a lab run.
const (
	substrateVersion       = "0.0.26"
	substrateChartsRepo    = "oci://ghcr.io/kagent-dev/substrate/helm"
	substrateImageRegistry = "ghcr.io/kagent-dev/substrate"
)

// The namespaces and releases. ate-system is hardcoded in the chart
// (ate-controller's Role names it), so the release must land there;
// podcertificate-controller-system is rendered by the chart — pre-created
// here for the two pools its signer mounts, then adopted by Helm
// (helmInstallOptions.TakeOwnership).
const (
	substrateNamespace         = "ate-system"
	podCertControllerNamespace = "podcertificate-controller-system"
	substrateCRDsRelease       = "substrate-crds"
	substrateRelease           = "substrate"
	substrateValuesTemplate    = "substrate-values.yaml.tmpl"
)

// The bootstrap objects the chart mounts but does not render.
const (
	// actorIDCACertsSecret carries the actor-identity CA's root as PEM
	// (caCertKey); atenet-egress trusts actor certificates against it.
	actorIDCACertsSecret = "actor-id-ca-certs" // #nosec G101 -- a Secret's name, not a credential
	// ateAPIAuthConfigMap is ate-api-server's authentication.yaml.
	ateAPIAuthConfigMap = "ate-api-authentication"
	// ateAPIAudience is the audience of the ServiceAccount tokens actors and
	// kagent present to ate-api-server (the chart's Service name).
	ateAPIAudience = "api.ate-system.svc"
	// substratePoolID is the id of the initial CA / signing key of every
	// pool, as upstream's bootstrap passes it (--ca-id=1, --key-id=1).
	substratePoolID = "1"
	// actorIDCAPool is the pool whose root becomes actorIDCACertsSecret.
	actorIDCAPool = "actor-id-ca-pool"
)

// substratePool is one of the four pool Secrets: two CA pools for the
// podcertificate-controller's signers (service DNS and pod identity), the
// JWT authority pool ate-api-server mints actor identity tokens from and the
// CA pool it signs actor certificates with.
type substratePool struct {
	namespace, name string
	// jwt marks the JWT authority pool; the others are CA pools.
	jwt bool
}

var substratePools = []substratePool{
	{namespace: podCertControllerNamespace, name: "service-dns-ca-pool"},
	{namespace: podCertControllerNamespace, name: "pod-identity-ca-pool"},
	{namespace: substrateNamespace, name: "actor-id-jwt-pool", jwt: true},
	{namespace: substrateNamespace, name: actorIDCAPool},
}

// podCertificateAPIPath is the API Substrate cannot start a pod without: the
// PodCertificateRequest API behind the certificates.k8s.io/v1beta1 gate.
const podCertificateAPIPath = "/apis/certificates.k8s.io/v1beta1"

// ateomGVisorImage is the worker image the WorkerPool the dev channel's
// kagent creates names (`workerImage:`, no `image:` line in any render), so
// the preload lists it explicitly — pulled once here instead of at the first
// actor's boot.
const ateomGVisorImage = substrateImageRegistry + "/ateom-gvisor:v" + substrateVersion

// The SandboxConfig the chart ships, which every gVisor WorkerPool names.
const (
	sandboxConfigResource = "sandboxconfigs.ate.dev"
	defaultSandboxConfig  = "gvisor-default"
)

// substrateInstallTimeout bounds each of the two upgrade-or-installs and
// their kstatus wait (the POC lab took under two minutes for the whole
// bootstrap, images warm).
const substrateInstallTimeout = 5 * time.Minute

// substrateUninstallTimeout bounds the waited uninstall on platform-down.
const substrateUninstallTimeout = 5 * time.Minute

// substrateUp installs Substrate: the preflight on the apiserver gates, the
// namespaces, the bootstrap objects (idempotent — an existing pool is never
// regenerated), the images, then substrate-crds and substrate in one waited
// upgrade-or-install each, and the SandboxConfig check. Idempotent: a re-run
// on a live cluster is a no-op revision.
func substrateUp(cfg *config.Config) error {
	step("Installing Substrate %s (kagent's actor runtime)", substrateVersion)
	ctx := context.Background()
	if err := preflightPodCertificateAPI(ctx); err != nil {
		return err
	}
	for _, ns := range []string{substrateNamespace, podCertControllerNamespace} {
		if err := ensureNamespace(ns); err != nil {
			return err
		}
	}
	created, err := ensureSubstratePools(ctx)
	if err != nil {
		return err
	}
	note("CA/JWT pools: %d created, %d kept (a regenerated root would orphan every certificate issued from it)", created, len(substratePools)-created)
	if err := ensureActorIDCACerts(ctx); err != nil {
		return err
	}
	if err := ensureATEAPIAuthentication(ctx); err != nil {
		return err
	}

	_, valuesPath, err := renderManifest(cfg, substrateValuesTemplate)
	if err != nil {
		return err
	}
	values, err := helmValuesFile(valuesPath)
	if err != nil {
		return err
	}
	chartRef := substrateChartsRepo + "/" + substrateRelease
	crdsRef := substrateChartsRepo + "/" + substrateCRDsRelease
	// The same host-cache -> node rule as the platform (preload.go);
	// best-effort, the node pulls anything missed under the install's wait.
	if rendered, err := helmTemplate(substrateNamespace, substrateRelease, chartRef, substrateVersion, values, nil); err == nil {
		images := append(scrapeImages(rendered), ateomGVisorImage)
		if res := sideloadImages(cfg, hostPullImages(images)); res.n > 0 {
			note("side-loaded %d Substrate images (%s)", res.n, res.d)
		}
	} else {
		note("cannot render substrate %s (%v); the node pulls its images itself", substrateVersion, excerpt(err.Error(), 300))
	}

	step("Installing %s and %s %s into %s (this waits for ate-api-server, atelet and atenet)", substrateCRDsRelease, substrateRelease, substrateVersion, substrateNamespace)
	if err := helmUpgradeInstall(substrateNamespace, substrateCRDsRelease, crdsRef, substrateVersion, map[string]any{}, substrateInstallTimeout, helmInstallOptions{}); err != nil {
		return err
	}
	// TakeOwnership: the chart renders the podcertificate-controller-system
	// Namespace the pools above needed in place first.
	if err := helmUpgradeInstall(substrateNamespace, substrateRelease, chartRef, substrateVersion, values, substrateInstallTimeout, helmInstallOptions{TakeOwnership: true}); err != nil {
		return err
	}
	// The kstatus wait returned with every workload Ready; the SandboxConfig
	// is the one object a WorkerPool cannot do without.
	gvr, err := gvrFor(sandboxConfigResource)
	if err != nil {
		return err
	}
	if exists, err := objectExists(ctx, gvr, "", defaultSandboxConfig); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("substrate %s installed, but SandboxConfig %s is missing: WorkerPools name it (check `kubectl get sandboxconfigs`)", substrateVersion, defaultSandboxConfig)
	}
	note("Substrate is up: SandboxConfig %s present; WorkerPools and Harnesses come with kagent", defaultSandboxConfig)
	return nil
}

// preflightPodCertificateAPI refuses a cluster whose apiserver does not
// serve certificates.k8s.io/v1beta1 — one created before the lab's kind
// config turned the gates on. Feature gates are fixed at `kind create`, so
// the only fix is a new cluster; said here, before anything is installed.
func preflightPodCertificateAPI(ctx context.Context) error {
	if _, err := rawGet(ctx, podCertificateAPIPath); err != nil {
		return fmt.Errorf("the apiserver does not serve certificates.k8s.io/v1beta1 (%v):\n"+
			"Substrate needs the PodCertificateRequest and ClusterTrustBundle gates the lab's kind config turns on, and this\n"+
			"cluster predates them — feature gates are fixed at kind create, so run `agentlab down && agentlab up`", err)
	}
	return nil
}

// ensureSubstratePools creates the pool Secrets that do not exist yet and
// leaves existing ones alone: rotating a root would orphan every certificate
// and token issued from it while the pods holding them keep running. Create,
// not apply, for the same reason (kube.go createTyped). Returns how many
// were created.
func ensureSubstratePools(ctx context.Context) (created int, err error) {
	for _, pool := range substratePools {
		exists, err := objectExists(ctx, gvrSecrets, pool.namespace, pool.name)
		if err != nil {
			return created, err
		}
		if exists {
			continue
		}
		var data map[string][]byte
		secretType := corev1.SecretTypeOpaque
		if pool.jwt {
			data, err = generateJWTPoolSecretData(substratePoolID)
		} else {
			data, err = generateCAPoolSecretData(substratePoolID)
			secretType = corev1.SecretTypeTLS
		}
		if err != nil {
			return created, fmt.Errorf("generating pool %s/%s: %w", pool.namespace, pool.name, err)
		}
		if err := createTyped(ctx, &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: pool.name, Namespace: pool.namespace},
			Type:       secretType,
			Data:       data,
		}); err != nil {
			return created, fmt.Errorf("creating pool %s/%s: %w", pool.namespace, pool.name, err)
		}
		created++
	}
	return created, nil
}

// ensureActorIDCACerts derives the trust anchor Secret from the actor-id CA
// pool: its signing root as PEM under `ca.crt`. Applied, not created — it is
// a pure function of the pool, so re-deriving it is always right.
func ensureActorIDCACerts(ctx context.Context) error {
	pool, err := secretDataKey(ctx, substrateNamespace, actorIDCAPool, "pool")
	if err != nil {
		return err
	}
	root, err := caPoolRootPEM(pool)
	if err != nil {
		return fmt.Errorf("pool %s/%s: %w", substrateNamespace, actorIDCAPool, err)
	}
	return ensureSecret(substrateNamespace, actorIDCACertsSecret, corev1.SecretTypeOpaque, map[string][]byte{caCertKey: root})
}

// secretDataKey reads one data key of a Secret, decoded.
func secretDataKey(ctx context.Context, ns, name, key string) ([]byte, error) {
	secret, err := getObject(ctx, gvrSecrets, ns, name)
	if err != nil {
		return nil, err
	}
	encoded, found, _ := unstructured.NestedString(secret.Object, "data", key)
	if !found {
		return nil, fmt.Errorf("secret %s/%s has no data key %s", ns, name, key)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s key %s: %w", ns, name, key, err)
	}
	return raw, nil
}

// ensureATEAPIAuthentication writes ate-api-server's authentication config:
// actor identities are Kubernetes ServiceAccount tokens for the audience
// api.ate-system.svc, verified against the cluster's own issuer.
func ensureATEAPIAuthentication(ctx context.Context) error {
	issuer, err := serviceAccountIssuer(ctx)
	if err != nil {
		return err
	}
	_, err = applyTyped(ctx, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: ateAPIAuthConfigMap, Namespace: substrateNamespace},
		Data:       map[string]string{"authentication.yaml": ateAPIAuthentication(issuer)},
	})
	return err
}

// serviceAccountIssuer is the `iss` of the apiserver's ServiceAccount tokens,
// read off its OpenID discovery document (`/.well-known/openid-configuration`)
// — kind's is https://kubernetes.default.svc.cluster.local. It has to be the
// apiserver's own spelling: ate-api-server matches the token's iss claim
// against it verbatim, and upstream's shorter in-cluster default fails on
// kind for exactly that reason.
func serviceAccountIssuer(ctx context.Context) (string, error) {
	raw, err := rawGet(ctx, "/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("reading the apiserver's OpenID discovery document: %w", err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("the apiserver's OpenID discovery document names no issuer (%v): %s", err, excerpt(string(raw), 200))
	}
	return doc.Issuer, nil
}

// inClusterIssuer is the ServiceAccount issuer of a cluster that publishes
// no other; kind spells it with the cluster domain.
const inClusterIssuer = "https://kubernetes.default.svc"

// ateAPIAuthentication renders ate-api-server's authentication.yaml the way
// substrate's ate-setup does (buildAuthenticationConfig): one JWT provider,
// the cluster's ServiceAccount issuer, the ate-api audience. An in-cluster
// issuer's discovery document is not on the public internet, so the server
// is pointed at its own projected ServiceAccount CA and token to fetch it.
func ateAPIAuthentication(issuer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "actorIdentityJWTProvider: kubernetes\njwtProviders:\n- name: kubernetes\n  issuer: %s\n  audiences: [%s]\n", issuer, ateAPIAudience)
	if issuer == inClusterIssuer || issuer == inClusterIssuer+".cluster.local" {
		b.WriteString("  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\n" +
			"  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\n")
	}
	return b.String()
}

// substrateDown removes Substrate from the cluster — after the platform,
// whose teardown deletes kagent's WorkerPool and Harnesses through finalizers
// ate-controller has to be running for. The releases go first (substrate
// waited, so its own objects' finalizers run while its controllers exist),
// then the namespaces with the pools in them: a next `up` generates fresh
// ones, and nothing issued from the old ones survives the teardown either.
// Presence-driven, not config-driven, so a knob flipped off still cleans up.
func substrateDown(ctx context.Context) error {
	if helmReleaseExists(substrateNamespace, substrateRelease) {
		step("Uninstalling Substrate (its controllers, then the CRDs)")
		if err := helmUninstall(substrateNamespace, substrateRelease, true, substrateUninstallTimeout); err != nil {
			return err
		}
	}
	if helmReleaseExists(substrateNamespace, substrateCRDsRelease) {
		if err := helmUninstall(substrateNamespace, substrateCRDsRelease, false, substrateUninstallTimeout); err != nil {
			return err
		}
	}
	for _, ns := range []string{substrateNamespace, podCertControllerNamespace} {
		if err := deleteNamespace(ctx, ns); err != nil {
			return err
		}
	}
	return nil
}
