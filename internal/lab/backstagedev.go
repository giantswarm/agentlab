package lab

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// Your own Backstage in the lab, from a checkout of giantswarm/backstage on
// this machine, two ways (docs/dev-environment.md):
//
//   - BackstageDev runs the checkout's `yarn start` against the lab: hot
//     reload, signed in through the lab Dex, talking to the lab's muster,
//     kagent and apiserver. Its app-config is the chart's own as the lab
//     runs it, with the in-cluster addresses swapped for the ones this
//     machine reaches, so the portal is the in-cluster one minus the build.
//   - BackstageBuild builds the checkout's image the way its CI does, records
//     it as platform.devImages.backstage and rolls it into the lab: the
//     artifact that ships, in the deployment shape it ships in.
const (
	backstageDevComponent = "backstage"
	// backstageDevDir holds what BackstageDev generates, under state/: the
	// checkout is never written to.
	backstageDevDir      = "backstage-dev"
	labCatalogConfigMap  = "agentlab-backstage-catalog"
	agentPlatformOIDCKey = "dex-client-secret"
	// yarnBin is the checkout's package manager; configFlag passes
	// `yarn start` one app-config file.
	yarnBin    = "yarn"
	configFlag = "--config"
	// urlKey and dbClientKey are app-config keys of the generated overlay.
	urlKey      = "url"
	dbClientKey = "client"
)

// backstageFrontendURL is where `yarn start` serves the frontend, on the port
// backstage.localDevPort validation holds free (config.BackstageFrontendPort).
var backstageFrontendURL = fmt.Sprintf("http://localhost:%d", config.BackstageFrontendPort)

// checkBackstageCheckout says whether dir looks like a giantswarm/backstage
// checkout: the backend package with its Dockerfile.
func checkBackstageCheckout(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for _, f := range []string{"package.json", "app-config.yaml", "packages/backend/Dockerfile"} {
		if _, err := os.Stat(filepath.Join(abs, f)); err != nil {
			return "", fmt.Errorf("%s is not a Backstage checkout (no %s): pass --checkout <path to giantswarm/backstage>", abs, f)
		}
	}
	return abs, nil
}

// BackstageDev runs the checkout with hot reload against the lab. A port
// other than the recorded backstage.localDevPort is recorded and Dex
// re-applied first, so the backend's sign-in callback is registered.
func BackstageDev(cfg *config.Config, checkout string, port int) error {
	dir, err := checkBackstageCheckout(checkout)
	if err != nil {
		return err
	}
	if !cfg.Backstage.Enabled || !cfg.Platform.Enabled {
		return fmt.Errorf("the lab runs no Backstage (backstage.enabled and platform.enabled in %s): there is no platform app-config to start from", config.File)
	}
	// Fail before anything is recorded or applied: a checkout without its
	// dependencies cannot start, and the config save and Dex apply below
	// should not happen for a run that dies at `yarn start`.
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return fmt.Errorf("%s has no node_modules: run `yarn install` there first", dir)
	}
	if port == 0 {
		port = cfg.Backstage.LocalDevPort
	}
	if port == 0 {
		port = config.DefaultBackstageLocalDevPort
	}
	if port != cfg.Backstage.LocalDevPort {
		step("Recording http://localhost:%d as the Backstage sign-in callback (backstage.localDevPort)", port)
		cfg.Backstage.LocalDevPort = port
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	// Always: a recorded localDevPort does not prove Dex was rendered with it
	// (a hand-edited agentlab.yaml never was). The apply is checksummed, so a
	// Dex already carrying the callback is a pure no-op and a changed render
	// rolls the pod exactly once.
	if err := ApplyDex(cfg); err != nil {
		return err
	}

	step("Writing the local app-config from the lab into %s/", filepath.Join(StateDir, backstageDevDir))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	files, env, err := backstageDevInputs(ctx, cfg)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(filepath.Join(StateDir, backstageDevDir))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	args := []string{"start", configFlag, filepath.Join(dir, "app-config.yaml")}
	for _, f := range files {
		path := filepath.Join(out, f.name)
		if err := os.WriteFile(path, f.data, 0o600); err != nil {
			return err
		}
		if f.config {
			args = append(args, configFlag, path)
		}
	}

	step("Starting %s with hot reload: %s (backend http://localhost:%d); sign in as a lab user, Ctrl-C stops it", dir, backstageFrontendURL, port)
	cmd := exec.Command(yarnBin, args...) // #nosec G204 -- the checkout's own yarn, lab-controlled args
	cmd.Dir = dir
	// A minimal environment, not os.Environ(): the chart's app-config
	// interpolates ${VAR} from the process environment, and cluster config is
	// data — it must not read this machine's environment beyond what node and
	// yarn need to run.
	cmd.Env = append(backstageDevBaseEnv(os.Environ()), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// backstageDevBaseEnv narrows the given environment to what node and yarn
// need: the named variables, and the yarn and npm configuration prefixes.
// Everything else stays out of ${VAR} interpolation's reach.
func backstageDevBaseEnv(environ []string) []string {
	keep := []string{"PATH", "HOME", "USER", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TERM", "NODE_ENV", "NODE_OPTIONS"}
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keep, name) || strings.HasPrefix(name, "YARN_") || strings.HasPrefix(name, "npm_config_") {
			env = append(env, kv)
		}
	}
	return env
}

// backstageIncludeKeys are @backstage/config-loader's include directives: in
// a --config file they read files and environment variables of the machine
// the loader runs on. In-cluster the same bytes only ever resolve inside the
// pod; on this machine they must never be evaluated.
var backstageIncludeKeys = []string{"$file", "$include", "$env"}

// checkNoIncludeKeys refuses a ConfigMap value that is not clean YAML or that
// carries an include key in any of its documents, however deep — a refusal,
// not a strip, so a chart that starts using one is noticed loudly. `${VAR}`
// string interpolation is untouched: only the include KEYS are refused.
func checkNoIncludeKeys(configMap, key string, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("configmap %s key %s does not parse as YAML (%v): refusing to pass it to the local Backstage", configMap, key, err)
		}
		if bad := findIncludeKey(doc); bad != "" {
			return fmt.Errorf("configmap %s key %s carries the include key %q, which the local Backstage would evaluate against this machine's files and environment: refusing it", configMap, key, bad)
		}
	}
}

// findIncludeKey walks a decoded YAML document and returns the first
// backstageIncludeKeys member used as a mapping key, "" when there is none.
func findIncludeKey(node any) string {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if slices.Contains(backstageIncludeKeys, k) {
				return k
			}
			if bad := findIncludeKey(v[k]); bad != "" {
				return bad
			}
		}
	case []any:
		for _, child := range v {
			if bad := findIncludeKey(child); bad != "" {
				return bad
			}
		}
	}
	return ""
}

// devFile is one generated file; config ones are passed to `yarn start`.
type devFile struct {
	name   string
	data   []byte
	config bool
}

// backstageDevInputs reads what the in-cluster portal runs on — the chart's
// app-config, the lab's catalog users, the client and session secrets — and
// returns the files and environment a local run needs.
func backstageDevInputs(ctx context.Context, cfg *config.Config) ([]devFile, []string, error) {
	k, err := labKube()
	if err != nil {
		return nil, nil, err
	}
	cms := k.clientset.CoreV1().ConfigMaps(platformNamespace)
	chart, err := cms.Get(ctx, chartAppConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("reading the chart's Backstage app-config (is the lab up? `agentlab up`): %w", err)
	}
	catalog, err := cms.Get(ctx, labCatalogConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("reading the lab's catalog users: %w", err)
	}
	secrets, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, platformSecretsName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s/%s: %w", platformNamespace, platformSecretsName, err)
	}

	var files []devFile
	for _, key := range slices.Sorted(maps.Keys(chart.Data)) {
		// Cluster config is data: the checkout's config loader evaluates
		// $file/$include/$env in every --config file against this machine's
		// files and environment, so a ConfigMap a lab workload could patch
		// must never carry one (checkNoIncludeKeys).
		if err := checkNoIncludeKeys(chartAppConfigMap, key, []byte(chart.Data[key])); err != nil {
			return nil, nil, err
		}
		files = append(files, devFile{name: key, data: []byte(chart.Data[key]), config: true})
	}
	users, err := filepath.Abs(filepath.Join(StateDir, backstageDevDir, "users.yaml"))
	if err != nil {
		return nil, nil, err
	}
	files = append(files, devFile{name: "users.yaml", data: []byte(catalog.Data["users.yaml"])})
	// The cluster CA pins the apiserver in the generated app-config. A
	// kubeconfig may carry it inline (CAData) or as a file reference
	// (certificate-authority), so read the file where the bytes are not
	// inline; without either the kubernetes plugin would fail TLS silently.
	caData := k.cfg.CAData
	if len(caData) == 0 && k.cfg.CAFile != "" {
		caData, err = os.ReadFile(k.cfg.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading the cluster CA %s the kubeconfig names: %w", k.cfg.CAFile, err)
		}
	}
	if len(caData) == 0 {
		return nil, nil, fmt.Errorf("%s carries no cluster CA: the generated app-config cannot pin the apiserver", labKubeconfig())
	}
	overlay, err := backstageDevOverlay(cfg, k.cfg.Host, caData, users)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, devFile{name: "app-config.local-dev.yaml", data: overlay, config: true})

	env := []string{
		"AGENT_PLATFORM_OIDC_CLIENT_SECRET=" + string(secrets.Data[agentPlatformOIDCKey]),
		"AUTH_SESSION_SECRET=" + string(secrets.Data[backstageSessionSecretKey]),
		"NODE_EXTRA_CA_CERTS=" + absCAPath(),
	}
	return files, env, nil
}

// backstageDevOverlay is the app-config the local run layers last: the local
// URLs, and every in-cluster address of the chart's app-config swapped for
// the one this machine reaches — the apiserver on its published port, muster
// and the kagent API through the edge.
func backstageDevOverlay(cfg *config.Config, apiserver string, caData []byte, usersFile string) ([]byte, error) {
	backend := cfg.BackstageLocalDevURL()
	overlay := map[string]any{
		"app": map[string]any{"baseUrl": backstageFrontendURL},
		"backend": map[string]any{
			"baseUrl": backend,
			// Loopback only: Backstage's default is 0.0.0.0, which would
			// serve a backend holding the lab's OIDC client and session
			// secrets to the whole network.
			"listen": map[string]any{"port": cfg.Backstage.LocalDevPort, "host": "127.0.0.1"},
			"cors":   map[string]any{"origin": backstageFrontendURL},
			"database": map[string]any{
				dbClientKey:  "better-sqlite3",
				"connection": ":memory:",
			},
		},
		"catalog": map[string]any{
			"locations": []any{map[string]any{
				"type":   "file",
				"target": usersFile,
				"rules":  []any{map[string]any{"allow": []string{"User", "Group"}}},
			}},
		},
		// #nosec G101 -- no credential: the provider's name and the public cluster CA
		familyKubernetes: map[string]any{
			"clusterLocatorMethods": []any{map[string]any{
				"type": "config",
				"clusters": []any{map[string]any{
					nameKey:             platformNamespace,
					urlKey:              apiserver,
					"caData":            base64.StdEncoding.EncodeToString(caData),
					"authProvider":      "oidc",
					"oidcTokenProvider": "oidc-agent-platform",
				}},
			}},
		},
		musterValues: map[string]any{
			"installations": []any{map[string]any{
				nameKey:        platformNamespace,
				urlKey:         cfg.MusterBaseURL() + "/mcp",
				"authProvider": "oidc-agent-platform",
			}},
		},
		"agentPlatform": map[string]any{
			"kagent": map[string]any{
				"installations": map[string]any{
					platformNamespace: map[string]any{"apiBaseUrl": cfg.AgentgatewayBaseURL()},
				},
			},
		},
	}
	raw, err := yaml.Marshal(overlay)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Generated by `agentlab portal dev` on every start.\n"), raw...), nil
}

// BackstageBuild builds the checkout's image as its CI does, records it as
// platform.devImages.backstage and, unless noApply, rolls it into the lab.
// Every build gets its own tag: kind's containerd keeps running the old bits
// under a reused one.
func BackstageBuild(cfg *config.Config, checkout string, noApply bool, offers Offers) error {
	dir, err := checkBackstageCheckout(checkout)
	if err != nil {
		return err
	}
	tag := backstageDevTag(dir, time.Now())
	for _, c := range [][]string{
		{yarnBin, "install", "--immutable"},
		// tsc is load-bearing, not a lint step: ci:build's build:backend
		// bundles against the dist-types it emits.
		{yarnBin, "tsc"},
		{yarnBin, "ci:build"},
		backstageImageBuild(tag, dockerIsPodman()),
	} {
		step("%s (in %s)", strings.Join(c, " "), dir)
		cmd := exec.Command(c[0], c[1:]...) // #nosec G204 -- fixed build steps
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}

	step("Recording platform.devImages.%s: %s", backstageDevComponent, tag)
	if cfg.Platform.DevImages == nil {
		cfg.Platform.DevImages = map[string]string{}
	}
	cfg.Platform.DevImages[backstageDevComponent] = tag
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if noApply {
		note("not applied: `agentlab platform` rolls it into the lab")
		return nil
	}
	return PlatformUp(cfg, offers)
}

// backstageImageBuild is the image build of the checkout's Dockerfile. Under
// Podman its SELinux labelling hides the Dockerfile's `RUN --mount=type=bind`
// sources (the backend bundles) from the build's non-root user — "Permission
// denied" on a world-readable file — so the build runs unlabelled there, as
// Docker's builder does.
func backstageImageBuild(tag string, podman bool) []string {
	args := []string{dockerBin, "build", ".", "-f", "packages/backend/Dockerfile", "-t", tag}
	if podman {
		args = append(args, "--security-opt", "label=disable")
	}
	return args
}

// backstageDevTag names a build: the checkout's commit, marked when the tree
// has changes, and the time, so no two builds share a tag.
func backstageDevTag(dir string, now time.Time) string {
	rev := "nogit"
	if out, err := gitOutputIn(dir, "rev-parse", "--short", "HEAD"); err == nil {
		rev = strings.TrimSpace(out)
		if status, err := gitOutputIn(dir, "status", "--porcelain", "--untracked-files=no"); err == nil && strings.TrimSpace(status) != "" {
			rev += "-dirty"
		}
	}
	return fmt.Sprintf("backstage-dev:%s-%s", rev, now.UTC().Format("20060102150405"))
}

func gitOutputIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed git queries
	out, err := cmd.Output()
	return string(out), err
}
