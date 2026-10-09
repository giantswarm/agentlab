package lab

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// Agent Substrate's control API (ate-api-server) as a client outside the
// cluster reaches it — the way Substrate's own kubectl-ate does: a
// port-forward to the `api` Service's pod, TLS verified against the live
// ClusterTrustBundle of Substrate's service-DNS signer under the Service's
// in-cluster name, and a bearer token minted for the chart's ate-client
// ServiceAccount with that name as its audience — the audience the
// connectivity release's bootstrap wrote into ate-api-server's
// authentication config beside the apiserver's own issuer. The proofs that
// drive actors directly (workspacestest.go: an ActorTemplate with an
// external volume, no kagent Agent in front) talk to it this way; the agent
// proofs go through the kagent controller (kagentapi.go) as a person would.

const (
	// ateAPIService is the Service in front of ate-api-server and, as
	// <service>.<namespace>.svc, the name on its certificate and the token
	// audience.
	ateAPIService = "api"
	ateAPIPort    = 443
	// ateClientServiceAccount is the chart's ServiceAccount for clients
	// outside the cluster (the substrate chart's ate-client.yaml).
	ateClientServiceAccount = "ate-client"
	// ateAPITokenTTL bounds the minted token: longer than any proof.
	ateAPITokenTTL = time.Hour
	// The signers of Substrate's podcertificate-controller: service-DNS
	// certificates for Services' pods, pod identities (SPIFFE) for clients;
	// each publishes its live roots as a ClusterTrustBundle.
	serviceDNSSignerName    = "servicedns.podcert.ate.dev/identity"
	podIdentitySignerName   = "podidentity.podcert.ate.dev/identity"
	liveTrustBundleSelector = "podcert.ate.dev/canarying=live"
	// clusterTrustBundlesResource is the API the bundles are read from,
	// resolved through discovery: v1beta1 on the lab's kind, v1 later.
	clusterTrustBundlesResource = "clustertrustbundles.certificates.k8s.io"
)

// ateAPIServerName is the name the client verifies ate-api-server under and
// the audience of its token: <service>.<namespace>.svc.
func ateAPIServerName() string {
	return ateAPIService + "." + substrateNamespace + ".svc"
}

// ateAPI is one connection to ate-api-server through a port-forward: the
// generated ControlClient on it, and the stop that ends both.
type ateAPI struct {
	ateapi.ControlClient
	conn *grpc.ClientConn
	stop func()
}

// Close ends the connection and the port-forward behind it.
func (a *ateAPI) Close() {
	if a.conn != nil {
		_ = a.conn.Close()
	}
	if a.stop != nil {
		a.stop()
	}
}

// dialAteAPI connects to ate-api-server as the ate-client ServiceAccount.
func dialAteAPI(ctx context.Context) (*ateAPI, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	pods, err := k.targetPods(ctx, substrateNamespace, "deploy/"+substrateAPIDeployment)
	if err != nil {
		return nil, fmt.Errorf("ate-api-server: %w", err)
	}
	var pod string
	for _, p := range pods {
		if podReady(&p) {
			pod = p.Name
			break
		}
	}
	if pod == "" {
		return nil, fmt.Errorf("no Ready pod of deployment/%s in %s", substrateAPIDeployment, substrateNamespace)
	}
	pool, err := signerTrustPool(ctx, serviceDNSSignerName)
	if err != nil {
		return nil, err
	}
	token, err := ateClientToken(ctx)
	if err != nil {
		return nil, err
	}
	port, stop, err := portForwardPod(ctx, substrateNamespace, pod, ateAPIPort)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: ateAPIServerName()})
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(bearerCredentials(token)))
	if err != nil {
		stop()
		return nil, fmt.Errorf("dialling ate-api-server through the port-forward: %w", err)
	}
	return &ateAPI{ControlClient: ateapi.NewControlClient(conn), conn: conn, stop: stop}, nil
}

// signerTrustPool is the pool of a signer's live roots, from every live
// ClusterTrustBundle whose spec.signerName is the signer — what a pod gets
// through a clusterTrustBundle projected volume.
func signerTrustPool(ctx context.Context, signer string) (*x509.CertPool, error) {
	gvr, err := gvrFor(clusterTrustBundlesResource)
	if err != nil {
		return nil, fmt.Errorf("the ClusterTrustBundle API is not served (%v): the lab's kind config turns its gate on; `agentlab down && agentlab up`", err)
	}
	bundles, err := listObjects(ctx, gvr, "", liveTrustBundleSelector)
	if err != nil {
		return nil, fmt.Errorf("listing the live ClusterTrustBundles: %w", err)
	}
	pool := x509.NewCertPool()
	found := false
	for i := range bundles {
		b := &bundles[i]
		if name, _, _ := unstructured.NestedString(b.Object, "spec", "signerName"); name != signer {
			continue
		}
		pem, _, _ := unstructured.NestedString(b.Object, "spec", "trustBundle")
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("ClusterTrustBundle %s carries no certificate", b.GetName())
		}
		found = true
	}
	if !found {
		return nil, fmt.Errorf("no live ClusterTrustBundle of the signer %s: the connectivity release's bootstrap publishes it with the agents runtime", signer)
	}
	return pool, nil
}

// ateClientToken mints a token of the ate-client ServiceAccount for
// ate-api-server's audience.
func ateClientToken(ctx context.Context) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	expiry := int64(ateAPITokenTTL / time.Second)
	tr, err := k.clientset.CoreV1().ServiceAccounts(substrateNamespace).CreateToken(ctx, ateClientServiceAccount, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences:         []string{ateAPIServerName()},
		ExpirationSeconds: &expiry,
	}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("a token of ServiceAccount %s/%s for ate-api-server: %w", substrateNamespace, ateClientServiceAccount, err)
	}
	if tr.Status.Token == "" {
		return "", fmt.Errorf("the token request for %s/%s answered no token", substrateNamespace, ateClientServiceAccount)
	}
	return tr.Status.Token, nil
}

// bearerCredentials puts `authorization: Bearer <token>` on every call, over
// TLS only.
type bearerCredentials string

func (b bearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearerCredentials) RequireTransportSecurity() bool { return true }
