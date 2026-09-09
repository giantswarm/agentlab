package lab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	valuesloader "helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	ri "helm.sh/helm/v4/pkg/release"
	releasecommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/klog/v2"

	"github.com/giantswarm/agentlab/pkg/project"
)

// The embedded Helm. Every Helm operation of the lab — the platform's
// upgrade-or-install, the observability chart's, the offline renders the
// image preload scrapes, the release probes and the uninstalls of
// `platform-down` — runs in this process through Helm 4's SDK
// (helm.sh/helm/v4/pkg/action): no `helm` binary is required and there is no
// Helm version to match. What the SDK writes is what the Helm CLI writes —
// the release as Secrets in the release namespace under the same name, the
// chart cache and the registry credentials under Helm's own paths — so
// `helm -n agent-platform status agent-platform` (and `history`, `get
// values`, `upgrade`) on a shell reads and continues the release this binary
// installed: the CLI stays the day-2 tool, with KUBECONFIG=state/kubeconfig.
//
// The wait is Helm 4's kstatus watcher (kube.StatusWatcherStrategy, `--wait`
// in the CLI): it watches every object of the release, custom resources
// included, until each is Current — for the platform chart that is the
// FluxInstance and every component HelmRelease Ready when the install
// returns, which the platform relies on (platform.go).
//
// The cluster is always the lab's: every operation binds to the lab-owned
// kubeconfig through labRESTClientGetter (restclient.go), never to the
// shell's KUBECONFIG or current-context — as the Kubernetes client (kube.go).

// helmToolName names Helm among the discovery report's embedded tools; its
// version is the module this binary was built with.
const helmToolName = "helm"

func helmToolVersion() string {
	return project.ModuleVersion("helm.sh/helm/v4")
}

// helmDriver is the release storage: Secrets in the release namespace, the
// CLI's default (HELM_DRIVER unset). Pinned rather than read from the
// environment so the release is always where the CLI looks by default.
const helmDriver = "secret"

// helmFieldManager is the field manager Helm records on every object it
// applies server-side — the CLI's "helm". The SDK's own default is the name
// of the running binary (filepath.Base of os.Args[0]), and server-side apply
// refuses to change a field another manager owns while a chart's zero-valued
// fields (hostNetwork: false, initialDelaySeconds: 0) count as changed on
// every re-apply: a release installed by a binary of another name
// (agentlab-linux-amd64 as downloaded), or by the Helm CLI, could not be
// upgraded — "Apply failed with N conflicts". One name for the lab and for a
// `helm upgrade` from a shell keeps the release upgradable by both.
const helmFieldManager = "helm"

// helmDebugEnv streams the SDK's log to stderr as it happens (the CLI's
// --debug); without it the log is kept and printed only when an operation
// fails. The CLI's own variable, so one habit covers both.
const helmDebugEnv = "HELM_DEBUG"

// helmSettings is Helm's environment for what is not the cluster: the
// registry credentials file (a `helm registry login` on this machine is
// honoured), the chart content cache, the repository files, the downloader
// plugins, HELM_DEBUG — the CLI's defaults and variables, so the caches are
// shared with a helm on the same machine. Its cluster-side fields are unused:
// the cluster comes from labRESTClientGetter.
func helmSettings() *cli.EnvSettings {
	return cli.New()
}

// helmLog is what the SDK says during one operation. The lab is quiet on
// success (the step lines are the progress report): the log is kept and
// printed to stderr only when the operation fails — as the captured output
// of a failed subprocess is — or streamed live under HELM_DEBUG.
type helmLog struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	live bool
}

func newHelmLog() *helmLog {
	return &helmLog{live: helmSettings().Debug}
}

// Write is the sink of the slog handler and of the registry client's
// "Pulled:"/"Digest:" lines.
func (l *helmLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live {
		return os.Stderr.Write(p)
	}
	return l.buf.Write(p)
}

// handler is the slog handler the action configuration and its kube client
// log through: Info and above kept for the failure report, Debug too when
// streaming live — the CLI's --debug verbosity.
func (l *helmLog) handler() slog.Handler {
	level := slog.LevelInfo
	if l.live {
		level = slog.LevelDebug
	}
	return slog.NewTextHandler(l, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
}

// dump prints what was kept, once, after a failure.
func (l *helmLog) dump() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() > 0 {
		_, _ = os.Stderr.Write(l.buf.Bytes())
		l.buf.Reset()
	}
}

// klogOnce routes client-go's own logger (klog: client-side throttling and
// API deprecation warnings) the same way, once per process: to stderr under
// HELM_DEBUG, otherwise nowhere — it is global, so it cannot follow one
// operation's log.
var klogOnce sync.Once

func quietKlog(live bool) {
	klogOnce.Do(func() {
		klog.LogToStderr(false)
		if live {
			klog.SetOutput(os.Stderr)
			return
		}
		klog.SetOutput(io.Discard)
	})
}

// helmOp is one Helm operation bound to the lab cluster and a namespace: the
// action configuration (release storage, kube client), Helm's environment
// for the chart sources, the registry client for oci:// charts, and the log.
type helmOp struct {
	cfg      *action.Configuration
	settings *cli.EnvSettings
	log      *helmLog
}

// newHelmOp is the CLI's start-up for one command: the action configuration
// initialised against the lab cluster with the secret driver, and the default
// registry client (the CLI's registry cache and credentials file).
func newHelmOp(namespace string) (*helmOp, error) {
	log := newHelmLog()
	quietKlog(log.live)
	settings := helmSettings()
	kube.ManagedFieldsManager = helmFieldManager
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(log.handler()))
	if err := cfg.Init(labRESTClientGetter(namespace), namespace, helmDriver); err != nil {
		return nil, fmt.Errorf("initialising the embedded Helm: %w", err)
	}
	rc, err := registry.NewClient(
		registry.ClientOptDebug(settings.Debug),
		registry.ClientOptEnableCache(true),
		registry.ClientOptWriter(log),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("creating the OCI registry client: %w", err)
	}
	cfg.RegistryClient = rc
	return &helmOp{cfg: cfg, settings: settings, log: log}, nil
}

// fail wraps an operation's error the way a failed subprocess read — the
// invocation the CLI would have been, then Helm's message — after printing
// the kept log.
func (h *helmOp) fail(invocation string, err error) error {
	h.log.dump()
	return fmt.Errorf("helm %s: %w", invocation, err)
}

// loadChart locates and loads a chart the way the CLI's chart argument
// works: a directory is loaded as it is; an oci:// reference is resolved
// against the registry's tags — the exact version, or the highest tag a
// semver range matches (what source-controller does for an OCIRepository) —
// and pulled into Helm's content cache once per digest. opts carries the
// version and the registry client of the action the chart is for.
func (h *helmOp) loadChart(opts *action.ChartPathOptions, ref, version string) (chart.Charter, error) {
	opts.Version = version
	path, err := opts.LocateChart(ref, h.settings)
	if err != nil {
		return nil, err
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, err
	}
	ac, err := chart.NewAccessor(ch)
	if err != nil {
		return nil, err
	}
	if req := ac.MetaDependencies(); len(req) > 0 {
		// A packaged chart carries its dependencies; a local chart directory
		// (platform.chartPath) has to have them built — the lab reads the
		// directory, it never writes into it.
		if err := action.CheckDependencies(ch, req); err != nil {
			return nil, fmt.Errorf("chart dependencies: %w (a chart directory needs its charts/ built: `helm dependency build`)", err)
		}
	}
	return ch, nil
}

// helmValuesFile reads one values file the way `-f` does: Helm's values
// loader, so a `null` survives as the key's deletion marker.
func helmValuesFile(path string) (map[string]any, error) {
	return helmValuesFiles(path)
}

// helmValuesFiles reads values files the way repeated `-f` flags do: maps
// merge, lists replace, the later file wins.
func helmValuesFiles(paths ...string) (map[string]any, error) {
	opts := values.Options{ValueFiles: paths}
	vals, err := opts.MergeValues(getter.All(helmSettings()))
	if err != nil {
		return nil, fmt.Errorf("reading the values %s: %w", strings.Join(paths, ", "), err)
	}
	return vals, nil
}

// helmValues parses inlined values (a HelmRelease's spec.values) with the
// same loader; nil or empty input is an empty values set.
func helmValues(raw []byte) (map[string]any, error) {
	vals, err := valuesloader.LoadValues(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parsing the values: %w", err)
	}
	return vals, nil
}

// helmChartLabel words a chart reference for messages: the ref, with the
// version when there is one.
func helmChartLabel(ref, version string) string {
	if version == "" {
		return ref
	}
	return ref + " --version " + version
}

// helmUpgradeInstall is `helm upgrade --install <release> <chart> -n <ns> -f
// <values> --wait --timeout <timeout> --force-conflicts` (plus
// --create-namespace when asked):
// the release is installed when it does not exist or was uninstalled,
// upgraded otherwise, with the CLI's defaults — server-side apply on install
// and, on upgrade, the method the release was applied with; no
// --reuse-values (the rendered values are the whole configuration, so an
// unchanged re-run is a new revision with no drift); hooks on; the history
// capped at Helm's default — and the kstatus wait until every object of the
// release, custom resources included, is Current. Ctrl-C cancels the
// operation the way it cancels the CLI: the release is marked failed instead
// of staying pending forever.
func helmUpgradeInstall(namespace, releaseName, ref, version string, vals map[string]any, timeout time.Duration, createNamespace bool) error {
	h, err := newHelmOp(namespace)
	if err != nil {
		return err
	}
	invocation := fmt.Sprintf("upgrade --install %s %s -n %s", releaseName, helmChartLabel(ref, version), namespace)
	history := action.NewHistory(h.cfg)
	history.Max = 1
	revisions, err := history.Run(releaseName)
	notFound := errors.Is(err, driver.ErrReleaseNotFound)
	if err != nil && !notFound {
		return h.fail(invocation, err)
	}
	uninstalled, err := lastRevisionUninstalled(revisions)
	if err != nil {
		return h.fail(invocation, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if notFound || uninstalled {
		install := action.NewInstall(h.cfg)
		install.ReleaseName = releaseName
		install.Namespace = namespace
		install.CreateNamespace = createNamespace
		install.Timeout = timeout
		install.WaitStrategy = kube.StatusWatcherStrategy
		install.ForceConflicts = true
		// The last revision is an uninstalled one kept in history: the name
		// is reused, as the CLI does.
		install.Replace = uninstalled
		ch, err := h.loadChart(&install.ChartPathOptions, ref, version)
		if err != nil {
			return h.fail(invocation, err)
		}
		if _, err := install.RunWithContext(ctx, ch, vals); err != nil {
			return h.fail(invocation, fmt.Errorf("INSTALL FAILED: %w", err))
		}
		return nil
	}

	upgrade := action.NewUpgrade(h.cfg)
	upgrade.Install = true
	upgrade.Namespace = namespace
	upgrade.Timeout = timeout
	upgrade.WaitStrategy = kube.StatusWatcherStrategy
	// The CLI's --force-conflicts: fields another manager owns are taken over
	// rather than refused — the release of a lab installed before the manager
	// was pinned (owned by the binary's name then) upgrades instead of
	// failing on every zero-valued field of the chart.
	upgrade.ForceConflicts = true
	upgrade.MaxHistory = h.settings.MaxHistory
	ch, err := h.loadChart(&upgrade.ChartPathOptions, ref, version)
	if err != nil {
		return h.fail(invocation, err)
	}
	if _, err := upgrade.RunWithContext(ctx, releaseName, ch, vals); err != nil {
		return h.fail(invocation, fmt.Errorf("UPGRADE FAILED: %w", err))
	}
	return nil
}

// lastRevisionUninstalled reports whether the newest revision in a release's
// history is an uninstalled one (kept with --keep-history), which an
// upgrade-or-install treats as no release: it installs, reusing the name.
func lastRevisionUninstalled(revisions []ri.Releaser) (bool, error) {
	if len(revisions) == 0 {
		return false, nil
	}
	last, err := asV1Release(revisions[len(revisions)-1])
	if err != nil {
		return false, err
	}
	return last.Info != nil && last.Info.Status == releasecommon.StatusUninstalled, nil
}

// helmTemplate is `helm template <release> <chart> -n <ns> -f <values>
// [--api-versions ...]`: a client-only render — no cluster is contacted,
// .Capabilities is Helm's default set plus apiVersions — of the chart's
// templates and hooks, CRDs excluded, as one multi-document manifest.
func helmTemplate(namespace, releaseName, ref, version string, vals map[string]any, apiVersions []string) (string, error) {
	h, err := newHelmOp(namespace)
	if err != nil {
		return "", err
	}
	invocation := fmt.Sprintf("template %s %s -n %s", releaseName, helmChartLabel(ref, version), namespace)
	install := action.NewInstall(h.cfg)
	install.DryRunStrategy = action.DryRunClient
	install.ReleaseName = releaseName
	install.Namespace = namespace
	// Skip the name check, as `helm template` does.
	install.Replace = true
	install.APIVersions = common.VersionSet(apiVersions)
	ch, err := h.loadChart(&install.ChartPathOptions, ref, version)
	if err != nil {
		return "", h.fail(invocation, err)
	}
	rendered, err := install.RunWithContext(context.Background(), ch, vals)
	if err != nil {
		return "", h.fail(invocation, err)
	}
	rel, err := asV1Release(rendered)
	if err != nil {
		return "", h.fail(invocation, err)
	}
	var b strings.Builder
	fmt.Fprintln(&b, strings.TrimSpace(rel.Manifest))
	for _, hook := range rel.Hooks {
		fmt.Fprintf(&b, "---\n# Source: %s\n%s\n", hook.Path, hook.Manifest)
	}
	return b.String(), nil
}

// helmReleaseExists is `helm -n <ns> status <release>` reduced to its
// verdict: whether the release has a revision in the namespace, whatever its
// status. False too when the cluster cannot be reached.
func helmReleaseExists(namespace, releaseName string) bool {
	h, err := newHelmOp(namespace)
	if err != nil {
		return false
	}
	history := action.NewHistory(h.cfg)
	history.Max = 1
	revisions, err := history.Run(releaseName)
	return err == nil && len(revisions) > 0
}

// helmReleaseChart is the chart a release was installed from, by name — what
// `helm -n <ns> list --filter '^<release>$'` reports in its chart column,
// without the version. Empty when there is no such release.
func helmReleaseChart(namespace, releaseName string) (string, error) {
	h, err := newHelmOp(namespace)
	if err != nil {
		return "", err
	}
	list := action.NewList(h.cfg)
	list.Filter = "^" + regexp.QuoteMeta(releaseName) + "$"
	releases, err := list.Run()
	if err != nil {
		return "", h.fail(fmt.Sprintf("list -n %s --filter %s", namespace, list.Filter), err)
	}
	for _, r := range releases {
		rel, err := asV1Release(r)
		if err != nil {
			return "", err
		}
		if rel.Name == releaseName && rel.Chart != nil && rel.Chart.Metadata != nil {
			return rel.Chart.Metadata.Name, nil
		}
	}
	return "", nil
}

// helmUninstall is `helm -n <ns> uninstall <release> [--wait] --timeout
// <timeout>`: hooks on (the platform chart's pre-delete hooks are its ordered
// teardown), the history purged, background cascading, and with wait the
// kstatus watcher until every object of the release is gone.
func helmUninstall(namespace, releaseName string, wait bool, timeout time.Duration) error {
	h, err := newHelmOp(namespace)
	if err != nil {
		return err
	}
	uninstall := action.NewUninstall(h.cfg)
	uninstall.Timeout = timeout
	uninstall.DeletionPropagation = "background"
	uninstall.WaitStrategy = kube.HookOnlyStrategy
	if wait {
		uninstall.WaitStrategy = kube.StatusWatcherStrategy
	}
	resp, err := uninstall.Run(releaseName)
	if err != nil {
		return h.fail(fmt.Sprintf("uninstall %s -n %s", releaseName, namespace), err)
	}
	if resp != nil && resp.Info != "" {
		note("%s", resp.Info)
	}
	return nil
}

// asV1Release is the concrete release behind the SDK's release interface —
// the v1 release, the only one Helm 4 stores.
func asV1Release(r ri.Releaser) (*release.Release, error) {
	switch rel := r.(type) {
	case *release.Release:
		return rel, nil
	case release.Release:
		return &rel, nil
	default:
		return nil, fmt.Errorf("unexpected release type %T", r)
	}
}
