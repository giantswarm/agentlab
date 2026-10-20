package lab

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart form of the central proof runs beekeeper serve in the lab
// cluster from the beekeeper chart, as an installation runs it: the CRDs
// from the chart, the Environment and the lane from its values, the
// mailboxes in a CloudNativePG database whose app Secret the chart reads,
// the lab Dex reached through the dex-localhost sidecar and trusted through
// the lab CA, and muster reaching serve at its Service with the person's
// token forwarded.

const (
	beekeeperChartNamespace = "beekeeper"
	beekeeperChartRelease   = "beekeeper"
	// beekeeperChartPG is the CloudNativePG Cluster of the mailboxes; CNPG
	// writes its initdb database's connection into <name>-app, key uri.
	beekeeperChartPG = "beekeeper-pg"
	// beekeeperChartCA is the ConfigMap of the lab CA serve trusts the lab
	// Dex by.
	beekeeperChartCA      = "beekeeper-idp-ca"
	beekeeperChartTimeout = 5 * time.Minute
)

// startBeekeeperChart installs chart (a directory, or an oci:// reference at
// version) with serve on group as the organization and the team, and returns
// serve's in-cluster MCP URL and the func that removes everything it created.
func startBeekeeperChart(cfg *config.Config, chart, version, group string) (string, func(), error) {
	var undo []func()
	stop := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	fail := func(err error) (string, func(), error) {
		stop()
		return "", nil, err
	}

	// The CRDs come with the chart and Helm never deletes them: those the
	// lab did not have go once the release is gone.
	var newCRDs []string
	for _, name := range beekeeperCRDs {
		crd := name + ".beekeeper.giantswarm.io"
		if _, err := getObject(context.Background(), gvrCRDs, "", crd); err != nil {
			newCRDs = append(newCRDs, crd)
		}
	}
	undo = append(undo, func() {
		for _, crd := range newCRDs {
			_ = deleteObject(context.Background(), gvrCRDs, "", crd, 30*time.Second)
		}
	})

	ca, err := os.ReadFile(trustBundleFile(cfg)) //nolint:gosec // the lab's own CA bundle
	if err != nil {
		return fail(err)
	}
	var caData strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(ca)), "\n") {
		caData.WriteString("    " + line + "\n")
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels: {%[2]s: %[3]s}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[4]s
  namespace: %[1]s
  labels: {%[2]s: %[3]s}
data:
  ca.crt: |
%[5]s---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: %[6]s
  namespace: %[1]s
  labels: {%[2]s: %[3]s}
spec:
  instances: 1
  storage: {size: 1Gi}
  bootstrap:
    initdb: {database: beekeeper, owner: beekeeper}
`, beekeeperChartNamespace, managedByLabel, managedByAgentlabValue, beekeeperChartCA, caData.String(), beekeeperChartPG)
	undo = append(undo, func() {
		if gvr, err := gvrFor("namespaces"); err == nil {
			_ = deleteObject(context.Background(), gvr, "", beekeeperChartNamespace, 2*time.Minute)
		}
	})
	if _, err := applyManifests(context.Background(), []byte(manifest)); err != nil {
		return fail(err)
	}

	step("Waiting for the mailboxes' CloudNativePG Cluster %s/%s", beekeeperChartNamespace, beekeeperChartPG)
	gvr, err := gvrFor("clusters.postgresql.cnpg.io")
	if err != nil {
		return fail(err)
	}
	if !waitFor(60, 5*time.Second, func() bool {
		obj, err := getObject(context.Background(), gvr, beekeeperChartNamespace, beekeeperChartPG)
		return err == nil && conditionStatus(obj, "Ready") == "True"
	}) {
		return fail(fmt.Errorf("the CloudNativePG Cluster %s/%s is not Ready within 5 minutes", beekeeperChartNamespace, beekeeperChartPG))
	}

	tag := strings.TrimPrefix(version, "v")
	image := beekeeperImage + ":" + tag
	sideloadImages(cfg, hostPullImages([]string{image, dexLocalhostImage}))
	values := fmt.Sprintf(`image: {tag: %[1]q}
serve:
  issuer: %[2]q
  clientIDs: [%[3]q, %[4]q]
  organization: %[5]q
  teams: {%[5]q: %[6]q}
environments:
  - {name: %[7]q, team: %[6]q}
lanes:
  - {name: %[8]q, installation: %[7]q, repositories: [%[9]q]}
database: {secretName: %[10]s-app, secretKey: uri}
teamNamespaces: {create: false}
extraEnv:
  - {name: SSL_CERT_FILE, value: /etc/idp-ca/ca.crt}
extraVolumes:
  - {name: idp-ca, configMap: {name: %[11]s}}
extraVolumeMounts:
  - {name: idp-ca, mountPath: /etc/idp-ca, readOnly: true}
%[12]s`, tag, cfg.Issuer(), config.AgentPlatformClientID, config.KubernetesClientID, group, beekeeperTeam,
		centralEnvironment, centralLane, centralRepo, beekeeperChartPG, beekeeperChartCA,
		beekeeperDexSidecar(cfg))
	vals, err := helmValues([]byte(values))
	if err != nil {
		return fail(err)
	}
	chartVersion := ""
	if !isLocalChart(chart) {
		chartVersion = tag
	}
	step("Installing the beekeeper chart %s (image %s) into %s", helmChartLabel(chart, chartVersion), image, beekeeperChartNamespace)
	undo = append(undo, func() {
		_ = helmUninstall(beekeeperChartNamespace, beekeeperChartRelease, true, 2*time.Minute)
	})
	if err := helmUpgradeInstall(beekeeperChartNamespace, beekeeperChartRelease, chart, chartVersion, vals, beekeeperChartTimeout, helmInstallOptions{}); err != nil {
		return fail(err)
	}
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/mcp", beekeeperChartRelease, beekeeperChartNamespace)
	return url, stop, nil
}

// beekeeperDexSidecar is the initContainers value of serve's pod: the
// dex-localhost forwarder while the issuer is spelled localhost (postrenderers.go,
// dexLocalhostPatch), none with an externally provisioned pair, whose issuer
// pods reach through CoreDNS.
func beekeeperDexSidecar(cfg *config.Config) string {
	if cfg.Platform.TLS.Set() {
		return ""
	}
	return fmt.Sprintf(`initContainers:
  - name: %[1]s
    image: %[2]s
    restartPolicy: Always
    args: ["TCP6-LISTEN:%[3]d,fork,reuseaddr", "TCP:%[4]s"]
    startupProbe: {tcpSocket: {port: %[3]d}, periodSeconds: 1, failureThreshold: 30}
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
      seccompProfile: {type: RuntimeDefault}
`, dexLocalhostContainer, dexLocalhostImage, cfg.DexPort, dexServiceAddr)
}

// isLocalChart is whether chart names a directory rather than a registry
// reference.
func isLocalChart(chart string) bool {
	return !strings.Contains(chart, "://")
}
