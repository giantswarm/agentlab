package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The dev-image swap (platform.devImages) as one `agentlab platform` run
// applies it. Two mechanisms, one values change each:
//
//   - A DEPLOYMENT target (muster, backstage, the kagent controller, the MCP
//     servers) is side-loaded into the node and rendered as a kustomize image
//     override on that component's HelmRelease (postrenderers.go). The name
//     the override replaces is read off the component chart's render — the
//     same render the preload scrapes its images from — because it differs
//     between lines: the kagent controller is
//     gsoci.azurecr.io/giantswarm/kagent-controller in the 3.x wrapper chart
//     and ghcr.io/giantswarm/kagent/controller on the kagent line. An
//     override whose name is in no rendered Deployment matches nothing, and
//     kustomize drops it without a word; the swap refuses to install such an
//     override and names the Deployment it looked for.
//
//   - The HARNESS target is the platform Harness's workload image, the Go ADK
//     runtime every agent runs on under kagent API v2. Nothing on the node's
//     containerd pulls it: the connectivity chart renders it BY DIGEST into
//     the `Harness` object (the CRD accepts no tag), and Substrate's atelet
//     fetches it from a registry into its own layer cache (the actors'
//     overlay lowerdirs) — a side-load is invisible to it. So the lab runs a
//     registry: a `registry` container on the kind docker network, published
//     on the host's loopback (platform.devRegistryPort), the pattern kind
//     documents for local registries and the one atelet's
//     --localhost-registry-replacement exists for. The build is pushed there,
//     the Harness pins `localhost:<port>/<repo>@sha256:<digest>` (the digest
//     the registry computed for the pushed manifest), and atelet rewrites the
//     localhost registry to the container's endpoint on the kind network,
//     plain HTTP. The digest reaches the Harness through the connectivity
//     values (kagent.harness.image, the values template), and the atelet flag
//     through the substrate values — the latter with the agents rather than
//     with the target, because it is inert for every other ref and the
//     Harness (connectivity's object) and atelet (substrate's) reconcile
//     independently: a flag that only arrived with the swap could lose the
//     race to the Harness's new digest and fail the first golden boot. Both
//     are part of the release, so a plain `helm upgrade` applies them and
//     removing the entry restores the chart's digest on the next run. A
//     changed Harness image recompiles every template the Harness admits (a
//     new golden snapshot per revision); the boot reports which and waits
//     for them.

// devRegistryImage is the registry the lab runs for the `harness` dev image:
// the CNCF Distribution registry, Docker Hub's official image, pinned.
const devRegistryImage = "registry:3.0.0"

// devRegistryContainerPort is the port the registry listens on inside its
// container; the kind-network endpoint atelet dials.
const devRegistryContainerPort = 5000

// devRegistryWaitAttempts bounds the wait for a freshly created registry to
// answer /v2/ (one second apart).
const devRegistryWaitAttempts = 30

// platformHarness is the one Harness the connectivity chart renders — the Go
// ADK runtime, named `kagent` — whose image the `harness` target replaces.
const platformHarness = "kagent"

// harnessRecompileTimeout bounds the wait for the admitted templates to
// recompile on the new Harness image: a golden boot per template on a worker
// of the pool, first pulls included.
const harnessRecompileTimeout = 5 * time.Minute

// devImages is the swap of one run: what the values need that the config
// alone cannot say.
type devImages struct {
	// names is the chart image name each configured Deployment target
	// replaces, from the component render.
	names map[string]string
	// harness is the `harness` dev image as the platform Harness pins it —
	// the lab registry's ref by digest; "" without the target.
	harness string
}

// prepareDevImages resolves and stages the configured dev images before the
// install: the Deployment targets' image names from the component renders
// (refusing an override that matches nothing), their side-load into the node
// with the node's own image list as the check, and the `harness` image pushed
// to the lab registry and pinned by digest. renders is the component renders
// keyed by release name (platformImages).
func prepareDevImages(cfg *config.Config, renders map[string]string) (*devImages, error) {
	names, err := resolveDevImageNames(cfg, renders)
	if err != nil {
		return nil, err
	}
	dev := &devImages{names: names}
	// The Deployment targets are builds of this host: never pullable, always
	// side-loaded, so their pods find them under imagePullPolicy IfNotPresent
	// — which is why each ref is then verified in the node's own image list
	// before the install (ensureNodeImages): a missing one would surface
	// five minutes later as an ImagePullBackOff, helm-controller's upgrade
	// timeout and a rollback to the chart's image.
	if refs := devImageRefs(cfg); len(refs) > 0 {
		if res := sideloadImages(cfg, hostPullImages(refs)); res.n > 0 {
			note("side-loaded %d dev images (%s)", res.n, res.d)
		}
		if err := ensureNodeImages(cfg, refs); err != nil {
			return nil, err
		}
	}
	if ref := harnessDevImage(cfg); ref != "" {
		step("Pushing the harness dev image %s to the lab registry", ref)
		if err := ensureDevRegistry(cfg); err != nil {
			return nil, err
		}
		pinned, err := pushDevImage(cfg, ref)
		if err != nil {
			return nil, err
		}
		dev.harness = pinned
		note("the platform Harness %s pins %s (atelet reaches the registry as %s)", platformHarness, pinned, devRegistryEndpoint(cfg))
	}
	return dev, nil
}

// templateData is the values template's mutator for this swap: the
// postRenderers with the resolved names, and the Harness pin.
func (d *devImages) templateData(cfg *config.Config) (func(*tmplData), error) {
	postRenderers, err := componentPostRenderers(cfg, d.names)
	if err != nil {
		return nil, err
	}
	return func(t *tmplData) {
		t.PostRenderers = postRenderers
		t.HarnessDevImage = d.harness
	}, nil
}

// registryFlag is atelet's flag for the lab registry as the substrate values
// carry it.
func registryFlag(cfg *config.Config) string {
	return "--localhost-registry-replacement=" + devRegistryEndpoint(cfg)
}

// checkValues asserts the merged values still carry the swap's two keys: a
// platform.valuesFiles overlay that sets kagent.harness.image itself (the
// way a lab pins a published build) wins the merge and the swap would be a
// silent no-op; one that sets substrate.atelet.extraArgs replaces the list
// (Helm merges maps, not lists) and atelet could not reach the registry.
func (d *devImages) checkValues(cfg *config.Config, values map[string]any) error {
	if d.harness == "" {
		return nil
	}
	if got := nestedString(values, "kagent", "harness", "image"); got != d.harness {
		return fmt.Errorf("platform.devImages.%s: the merged values pin kagent.harness.image to %q, not to the dev image %s — a platform.valuesFiles overlay sets it; drop that key from the overlay while the dev image is configured", config.DevImageHarness, got, d.harness)
	}
	if args, _ := nestedValue(values, "substrate", "atelet", "extraArgs").([]any); !slices.Contains(args, any(registryFlag(cfg))) {
		return fmt.Errorf("platform.devImages.%s: the merged values' substrate.atelet.extraArgs %v lack %s — a platform.valuesFiles overlay replaces the list; add the flag to the overlay's list or drop the key", config.DevImageHarness, args, registryFlag(cfg))
	}
	return nil
}

// nestedValue walks a values map along the keys and returns what is there,
// nil when any level is missing or not a map.
func nestedValue(values map[string]any, keys ...string) any {
	var cur any = values
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[k]; !ok {
			return nil
		}
	}
	return cur
}

// nestedString is nestedValue for a string: "" when it is not one.
func nestedString(values map[string]any, keys ...string) string {
	s, _ := nestedValue(values, keys...).(string)
	return s
}

// resolveDevImageNames reads, for every configured Deployment target, the
// image name the component chart renders on its Deployment/container off
// the component renders (keyed by release name — the meta chart names a
// component's HelmRelease after the component). A target whose chart
// rendered but has no such Deployment/container is an error: the override
// would match nothing and kustomize drops it silently, so the chart's shape
// changed under the lab's table. A target whose chart did not render at all
// (a registry hiccup; the preload already reported it) falls back to the
// table's name, unverified, and says so.
func resolveDevImageNames(cfg *config.Config, renders map[string]string) (map[string]string, error) {
	names := map[string]string{}
	for _, component := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		target, ok := devImageTargets[component]
		if !ok {
			continue
		}
		rendered, ok := renders[component]
		if !ok {
			note("platform.devImages.%s: the %s chart did not render here, so the override targets the table's image name %s unverified", component, component, target.image)
			names[component] = target.image
			continue
		}
		image, ok := renderedContainerImage(rendered, target.deployment, target.container)
		if !ok {
			return nil, fmt.Errorf("platform.devImages.%s: the %s chart's render has no Deployment %s with a container %s — the image override would match nothing and kustomize would drop it silently; the chart's shape changed (compare `helm template` of the component with devImageTargets)", component, component, target.deployment, target.container)
		}
		names[component] = imageName(image)
		note("platform.devImages.%s replaces %s (Deployment %s, container %s in the %s chart's render)", component, names[component], target.deployment, target.container, component)
	}
	return names, nil
}

// renderedWorkload is the part of a rendered Deployment the name resolution
// reads: its name and its containers' names and images.
type renderedWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `yaml:"name"`
					Image string `yaml:"image"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// renderedContainerImage finds the image of the named container of the
// named Deployment in a multi-document render. Documents that are not
// objects (comments, empty separators) are skipped.
func renderedContainerImage(rendered, deployment, container string) (string, bool) {
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc renderedWorkload
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			continue
		}
		if doc.Kind != kindDeployment || doc.Metadata.Name != deployment {
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if c.Name == container && c.Image != "" {
				return c.Image, true
			}
		}
	}
}

// imageName is a ref without its tag or digest — the name kustomize's image
// transformer matches on. A registry port sits before the first slash, so
// only a colon after the last slash is a tag separator.
func imageName(ref string) string {
	if repo, _, ok := strings.Cut(ref, "@"); ok {
		ref = repo
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

// devRegistryName is the registry container: one per cluster, named after
// it, on the kind docker network next to the node.
func devRegistryName(cfg *config.Config) string { return cfg.ClusterName + "-registry" }

// devRegistryEndpoint is the registry as pods reach it on the kind network:
// the container's name (docker's embedded DNS resolves it, and CoreDNS
// forwards to the node's resolver) and its own port.
func devRegistryEndpoint(cfg *config.Config) string {
	return devRegistryName(cfg) + ":" + strconv.Itoa(devRegistryContainerPort)
}

// devRegistryHost is the registry as the host spells it — and as the Harness
// ref spells it, so atelet's localhost rewrite applies: docker treats a
// localhost registry as insecure without daemon configuration, and so does
// atelet.
func devRegistryHost(cfg *config.Config) string {
	return localhostName + ":" + strconv.Itoa(cfg.Platform.DevRegistryPort)
}

// devRegistryURL is the registry's API on the host loopback.
func devRegistryURL(cfg *config.Config) string {
	return "http://127.0.0.1:" + strconv.Itoa(cfg.Platform.DevRegistryPort)
}

// ensureDevRegistry has the lab registry running on the kind network and
// published on the configured host port, creating or restarting the
// container as needed, and waits for its API. An existing registry that
// publishes another port is refused rather than silently used: the Harness
// ref carries the configured port.
func ensureDevRegistry(cfg *config.Config) error {
	name := devRegistryName(cfg)
	state, err := nodeState(name)
	switch {
	case err != nil:
		hostPullImages([]string{devRegistryImage})
		args := []string{"run", "-d", "--restart=always", "--name", name, "--network", kindDockerNetwork,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.Platform.DevRegistryPort, devRegistryContainerPort), devRegistryImage}
		if err := runQuiet(dockerBin, args...); err != nil {
			return fmt.Errorf("creating the lab registry %s: %w\n(another process holding 127.0.0.1:%d? set platform.devRegistryPort to a free port)", name, err, cfg.Platform.DevRegistryPort)
		}
		note("created the lab registry %s (%s on the %s network, published on %s)", name, devRegistryImage, kindDockerNetwork, devRegistryHost(cfg))
	case state == stateRunning:
	case state == statePaused:
		if err := runQuiet(dockerBin, verbUnpause, name); err != nil {
			return fmt.Errorf("resuming the lab registry %s: %w", name, err)
		}
	default:
		if err := runQuiet(dockerBin, verbStart, name); err != nil {
			return fmt.Errorf("starting the lab registry %s (%s): %w", name, state, err)
		}
		note("started the lab registry %s (was %s)", name, state)
	}
	if port, err := devRegistryPublishedPort(name); err != nil {
		return err
	} else if port != cfg.Platform.DevRegistryPort {
		return fmt.Errorf("the lab registry %s publishes 127.0.0.1:%d, but platform.devRegistryPort is %d: `docker rm -f %s` and re-run, or set the port back", name, port, cfg.Platform.DevRegistryPort, name)
	}
	url := devRegistryURL(cfg) + "/v2/"
	client := &http.Client{Timeout: 3 * time.Second}
	if !waitFor(devRegistryWaitAttempts, time.Second, func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		return fmt.Errorf("the lab registry %s does not answer on %s (`docker logs %s`)", name, url, name)
	}
	return nil
}

// devRegistryPublishedPort reads the host port the registry container
// publishes its registry port on.
func devRegistryPublishedPort(name string) (int, error) {
	format := fmt.Sprintf(`{{(index (index .NetworkSettings.Ports "%d/tcp") 0).HostPort}}`, devRegistryContainerPort)
	out, err := outputQuiet(dockerBin, "inspect", "-f", format, name)
	if err != nil {
		return 0, fmt.Errorf("reading the lab registry's published port: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("the lab registry %s publishes no port %d/tcp (%q)", name, devRegistryContainerPort, strings.TrimSpace(out))
	}
	return port, nil
}

// removeDevRegistry removes the lab registry container with its storage;
// nothing to do when there is none. Best-effort, for `agentlab down`.
func removeDevRegistry(cfg *config.Config) {
	name := devRegistryName(cfg)
	if _, err := nodeState(name); err != nil {
		return
	}
	if err := runQuiet(dockerBin, "rm", "-f", "-v", name); err != nil {
		note("removing the lab registry %s failed (%v); `docker rm -f -v %s` removes it", name, err, name)
		return
	}
	note("removed the lab registry %s", name)
}

// pushDevImage tags the local build into the lab registry, pushes it, and
// returns the ref the Harness pins: the registry's host spelling with the
// manifest digest the registry computed for what landed — the digest atelet
// keys its cache by.
func pushDevImage(cfg *config.Config, ref string) (string, error) {
	repo, tag := devImageRepoTag(ref)
	pushRef := devRegistryHost(cfg) + "/" + repo + ":" + tag
	if err := runQuiet(dockerBin, "tag", ref, pushRef); err != nil {
		return "", fmt.Errorf("tagging %s for the lab registry: %w (is the image in the host docker cache? `docker image inspect %s`)", ref, err, ref)
	}
	args := []string{"push", pushRef}
	if dockerIsPodman() {
		args = []string{"push", "--tls-verify=false", pushRef}
	}
	if err := runQuiet(dockerBin, args...); err != nil {
		return "", fmt.Errorf("pushing %s to the lab registry: %w", pushRef, err)
	}
	digest, err := registryManifestDigest(devRegistryURL(cfg), repo, tag)
	if err != nil {
		return "", err
	}
	return devRegistryHost(cfg) + "/" + repo + "@" + digest, nil
}

// devImageRepoTag splits a local ref into the repository path the lab
// registry serves it under and a tag: the registry host of the ref (when it
// names one) is dropped, the path kept; a digest ref gets a tag derived from
// the digest.
func devImageRepoTag(ref string) (repo, tag string) {
	path := ref
	if first, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(first, ".:") || first == localhostName) {
		path = rest
	}
	if repo, digest, ok := strings.Cut(path, "@"); ok {
		hex := strings.TrimPrefix(digest, "sha256:")
		return repo, "sha256-" + hex[:min(12, len(hex))]
	}
	slash := strings.LastIndex(path, "/")
	if colon := strings.LastIndex(path, ":"); colon > slash {
		return path[:colon], path[colon+1:]
	}
	return path, "latest"
}

// manifestAccept lists the manifest media types a registry may answer with —
// an OCI or Docker image manifest, or an index — so the digest is the one of
// what `docker push` uploaded, whichever form it took.
const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// digestRe is a sha256 digest as the registry and the Harness CRD spell it.
var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// registryManifestDigest asks the registry (distribution API) for the
// digest of a tag's manifest: the Docker-Content-Digest header of a HEAD.
func registryManifestDigest(registryURL, repo, tag string) (string, error) {
	url := registryURL + "/v2/" + repo + "/manifests/" + tag
	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestAccept)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the pushed manifest's digest from the lab registry: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the pushed manifest's digest from the lab registry: %s %s", url, resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !digestRe.MatchString(digest) {
		return "", fmt.Errorf("the lab registry answered %s with digest %q, not a sha256 digest", url, digest)
	}
	return digest, nil
}

// harnessState is what the boot records about the platform Harness before
// the install, to say afterwards what the swap changed: the image it ran and
// the desired revision of every template it admits.
type harnessState struct {
	image     string
	revisions map[string]string
}

// readHarnessState reads the platform Harness's image and its admitted
// templates' desired revisions; a Harness that is not there yet (a first
// install) reads as empty.
func readHarnessState(ctx context.Context) (harnessState, error) {
	st := harnessState{revisions: map[string]string{}}
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return st, nil // the CRD is not served yet: a first install
	}
	harness, err := getObject(ctx, gvr, kagentNamespace, platformHarness)
	if err != nil {
		return st, nil
	}
	st.image, _, _ = unstructured.NestedString(harness.Object, "spec", "workload", "image")
	templates, err := admittedTemplates(ctx, harness)
	if err != nil {
		return st, err
	}
	for name, t := range templates {
		if h := t.harness(platformHarness); h != nil {
			st.revisions[name] = h.DesiredRevision
		}
	}
	return st, nil
}

// admittedTemplates lists the AgentTemplates the Harness admits — those its
// allowedAgentTemplates selector matches, in its namespace — by name.
func admittedTemplates(ctx context.Context, harness *unstructured.Unstructured) (map[string]*agentTemplate, error) {
	labels, _, _ := unstructured.NestedStringMap(harness.Object, "spec", "allowedAgentTemplates", "selector", "matchLabels")
	var terms []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		terms = append(terms, k+"="+labels[k])
	}
	gvr, err := gvrFor(agentTemplateResource)
	if err != nil {
		return nil, err
	}
	objs, err := listObjects(ctx, gvr, harness.GetNamespace(), strings.Join(terms, ","))
	if err != nil {
		return nil, err
	}
	out := map[string]*agentTemplate{}
	for i := range objs {
		t, err := agentTemplateFrom(&objs[i])
		if err != nil {
			return nil, err
		}
		out[objs[i].GetName()] = t
	}
	return out, nil
}

// reportHarnessDevImage checks, after the install, that the platform Harness
// runs the dev image and waits for the templates it admits to recompile on
// it: a Harness image change moves every admitted template's desired
// revision, and the template is back once its latest successful revision is
// that one and Ready holds. A Harness that already ran the image (a re-run)
// changed nothing, so its templates only have to be Ready. The wait is a
// note when it runs out — the proofs are where a template that never comes
// back fails.
func reportHarnessDevImage(ctx context.Context, dev *devImages, before harnessState) error {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return fmt.Errorf("the platform Harness's CRD (%s) is not served: %w", harnessResource, err)
	}
	harness, err := getObject(ctx, gvr, kagentNamespace, platformHarness)
	if err != nil {
		return err
	}
	image, _, _ := unstructured.NestedString(harness.Object, "spec", "workload", "image")
	if image != dev.harness {
		return fmt.Errorf("the platform Harness %s runs %s, not the dev image %s: the connectivity chart did not forward kagent.harness.image (check the merged values in %s)", platformHarness, image, dev.harness, StateDir+"/agent-platform-values.yaml")
	}
	changed := before.image != image
	if changed {
		step("Waiting for the templates Harness %s admits to recompile on %s", platformHarness, image)
	}
	var pending []string
	var done []string
	deadline := time.Now().Add(harnessRecompileTimeout)
	for {
		templates, err := admittedTemplates(ctx, harness)
		if err != nil {
			return err
		}
		pending, done = pending[:0], done[:0]
		for _, name := range slices.Sorted(maps.Keys(templates)) {
			h := templates[name].harness(platformHarness)
			if h == nil {
				pending = append(pending, name+" (no status for the Harness yet)")
				continue
			}
			ready, message := h.condition("Ready")
			recompiled := !changed || h.DesiredRevision != before.revisions[name]
			if recompiled && h.LatestSuccessfulRevision == h.DesiredRevision && ready == conditionTrue {
				done = append(done, fmt.Sprintf("%s %s", name, revisionChange(before.revisions[name], h.DesiredRevision)))
				continue
			}
			pending = append(pending, fmt.Sprintf("%s (Ready=%s %s)", name, ready, message))
		}
		if len(pending) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	sort.Strings(done)
	switch {
	case len(done) == 0 && len(pending) == 0:
		note("Harness %s runs %s; it admits no template yet — the next one compiles on it", platformHarness, image)
	case changed:
		note("Harness %s runs %s; %d admitted templates recompiled on it: %s", platformHarness, image, len(done), strings.Join(done, ", "))
	default:
		note("Harness %s already ran %s; %d admitted templates Ready on it", platformHarness, image, len(done))
	}
	if len(pending) > 0 {
		warn("%d templates Harness %s admits have not recompiled after %s: %s", len(pending), platformHarness, harnessRecompileTimeout, strings.Join(pending, "; "))
	}
	return nil
}

// revisionChange words a template's revision move, `abc123 -> def456`, or
// just the revision when there was none before.
func revisionChange(from, to string) string {
	if from == "" || from == to {
		return to
	}
	return from + " -> " + to
}
