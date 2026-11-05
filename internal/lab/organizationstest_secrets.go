package lab

import (
	"context"
	"fmt"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// The management-cluster profile's Secret boundaries (giantswarm/agent-platform#1269):
// each component that reads Secrets reads them only where it is granted, and
// a workload cluster's kubeconfig in an org-* namespace is refused to all.
const (
	connectivityRelease       = "agent-platform-connectivity"
	connectivityHooksAccount  = connectivityRelease + "-hooks"
	googleCredentialProvider  = "google-access-token-credential-provider"
	ateController             = "ate-controller"
	egressMITMPool            = "egress-mitm-ca-pool"
	workloadKubeconfigSecret  = "agentlab-kubeconfig" // #nosec G101 -- a Secret's NAME, not a credential
	secretReadTokenSeconds    = int64(600)
	hookRBACCleanupWaitPeriod = 30 * time.Second
)

// secretRead is one read a ServiceAccount makes with its own token: a get of
// a named Secret, or a list of Secrets across the cluster when name is empty.
type secretRead struct {
	namespace, name string
	allowed         bool
}

func (r secretRead) String() string {
	if r.name == "" {
		return "list secrets -A"
	}
	return "get secret " + r.namespace + "/" + r.name
}

// readVerdict judges a read's result. A get the authorizer lets through
// answers the Secret or NotFound (authorization comes before the lookup);
// a refusal is 403 and nothing else.
func readVerdict(r secretRead, err error) (string, error) {
	switch {
	case r.allowed && (err == nil || apierrors.IsNotFound(err)):
		return r.String() + " allowed", nil
	case r.allowed:
		return "", fmt.Errorf("%s: want allowed, got %w", r, err)
	case apierrors.IsForbidden(err):
		return r.String() + " Forbidden", nil
	case err == nil:
		return "", fmt.Errorf("%s: want Forbidden, it was answered", r)
	default:
		return "", fmt.Errorf("%s: want Forbidden, got %w", r, err)
	}
}

// secretReadsAs makes each read as the ServiceAccount, with a token the
// TokenRequest API mints for it and a config that carries nothing else.
func secretReadsAs(ctx context.Context, k *kubeClients, namespace, account string, reads []secretRead) (string, error) {
	tr, err := k.clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, account, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(secretReadTokenSeconds)},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("a token for %s/%s: %w", namespace, account, err)
	}
	cfg, err := tokenConfig(tr.Status.Token)
	if err != nil {
		return "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(reads))
	for _, r := range reads {
		if r.name == "" {
			_, err = cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{Limit: 1})
		} else {
			_, err = cs.CoreV1().Secrets(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
		}
		v, verr := readVerdict(r, err)
		if verr != nil {
			return "", fmt.Errorf("%s/%s %w", namespace, account, verr)
		}
		out = append(out, v)
	}
	return fmt.Sprintf("%s/%s: %s", namespace, account, strings.Join(out, ", ")), nil
}

// ensureWorkloadKubeconfig puts a dummy kubeconfig Secret in the lab's org-*
// namespace, the shape a management cluster keeps a workload cluster's
// admin kubeconfig in.
func ensureWorkloadKubeconfig(ctx context.Context, k *kubeClients) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: workloadKubeconfigSecret, Namespace: orgNamespace, Labels: map[string]string{managedByLabel: managedByAgentlabValue}},
		StringData: map[string]string{"value": "a dummy workload cluster kubeconfig"},
	}
	_, err := k.clientset.CoreV1().Secrets(orgNamespace).Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// secretBoundariesHold reads as ate-controller and both credential
// providers: each reads only where it is granted (the controller its one
// pool Secret, the providers kagent and the organization namespaces), and
// none reads a workload cluster's kubeconfig or lists Secrets.
func secretBoundariesHold(orgNS string) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ensureWorkloadKubeconfig(ctx, k); err != nil {
		return "", fmt.Errorf("the dummy %s/%s: %w", orgNamespace, workloadKubeconfigSecret, err)
	}
	wc := secretRead{namespace: orgNamespace, name: workloadKubeconfigSecret}
	list := secretRead{}
	providerReads := []secretRead{
		{namespace: kagentNamespace, name: anthropicSecret, allowed: true},
		{namespace: orgNS, name: organizationModelSecret, allowed: true},
		wc,
		{namespace: substrateNamespace, name: egressMITMPool},
		list,
	}
	plan := []struct {
		account string
		reads   []secretRead
	}{
		{ateController, []secretRead{
			{namespace: substrateNamespace, name: egressMITMPool, allowed: true},
			wc,
			{namespace: kagentNamespace, name: anthropicSecret},
			{namespace: orgNS, name: organizationModelSecret},
			list,
		}},
		{credentialProvider, providerReads},
		{googleCredentialProvider, providerReads},
	}
	var lines []string
	for _, p := range plan {
		line, err := secretReadsAs(ctx, k, substrateNamespace, p.account, p.reads)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "; "), nil
}

// hookRBACKinds are the kinds of the connectivity hooks' identity and its
// grants, with the resources they are served as.
var hookRBACKinds = map[string]schema.GroupVersionResource{
	"ServiceAccount":     {Version: "v1", Resource: "serviceaccounts"},
	"Role":               {Group: rbacv1.GroupName, Version: "v1", Resource: "roles"},
	"RoleBinding":        {Group: rbacv1.GroupName, Version: "v1", Resource: "rolebindings"},
	"ClusterRole":        {Group: rbacv1.GroupName, Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding": {Group: rbacv1.GroupName, Version: "v1", Resource: "clusterrolebindings"},
}

// hookRBAC are the hooks' ServiceAccount and the RBAC objects named after
// it, from the release's hook manifests, without their hook annotations.
func hookRBAC(manifests string) ([]*unstructured.Unstructured, error) {
	objs, err := decodeManifests([]byte(manifests))
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if _, ok := hookRBACKinds[o.GetKind()]; !ok || !strings.HasPrefix(o.GetName(), connectivityHooksAccount) {
			continue
		}
		o.SetAnnotations(nil)
		out = append(out, o)
	}
	return out, nil
}

// hookSecretsHold puts the connectivity hooks' identity in place as its
// hook event would (the release's own hook manifests), reads as it, and
// removes it again: it reads the pool Secret it writes and nothing of
// kagent's or an organization's, no workload cluster's kubeconfig, no list.
func hookSecretsHold(orgNS string) (string, error) {
	rel, err := helmReleaseNewest(platformNamespace, connectivityRelease)
	if err != nil {
		return "", err
	}
	if rel == nil {
		return "", fmt.Errorf("no Helm release %s/%s", platformNamespace, connectivityRelease)
	}
	var hooks []string
	for _, h := range rel.Hooks {
		if h != nil {
			hooks = append(hooks, h.Manifest)
		}
	}
	objs, err := hookRBAC(strings.Join(hooks, "\n---\n"))
	if err != nil {
		return "", err
	}
	if len(objs) == 0 {
		return "", fmt.Errorf("the hooks of %s revision %d carry no %s identity", connectivityRelease, rel.Version, connectivityHooksAccount)
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	defer func() {
		for _, o := range objs {
			if err := deleteObject(context.Background(), hookRBACKinds[o.GetKind()], o.GetNamespace(), o.GetName(), hookRBACCleanupWaitPeriod); err != nil {
				note("cleanup: %v", err)
			}
		}
	}()
	for _, o := range objs {
		if _, err := k.apply(ctx, o); err != nil {
			return "", err
		}
	}
	if err := ensureWorkloadKubeconfig(ctx, k); err != nil {
		return "", fmt.Errorf("the dummy %s/%s: %w", orgNamespace, workloadKubeconfigSecret, err)
	}
	return secretReadsAs(ctx, k, platformNamespace, connectivityHooksAccount, []secretRead{
		{namespace: substrateNamespace, name: egressMITMPool, allowed: true},
		{namespace: orgNamespace, name: workloadKubeconfigSecret},
		{namespace: kagentNamespace, name: anthropicSecret},
		{namespace: orgNS, name: organizationModelSecret},
		{},
	})
}
