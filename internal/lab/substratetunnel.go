package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// The Substrate tunnel-close proof: a gRPC call through atenet-router to an
// actor that speaks HTTP/1.1 only fails at once instead of waiting out its
// client's timeout.
//
// atenet-router reaches an actor through a CONNECT tunnel to the worker's
// atunnel, which relays it to the actor's port 80. Plain HTTP the router
// downgrades to HTTP/1.1 for such an actor; a gRPC-shaped request it carries
// as HTTP/2, so the actor answers the HTTP/2 preface with an HTTP/1.1 error
// and closes, its input unread. The relay must end the tunnel with the
// actor's connection: one that waits for the router to stop sending keeps it
// open, and the router waits for the actor's SETTINGS frame until its
// client gives up. Every actor the agents run on speaks h2c, so the proof
// brings its own: an ActorTemplate on the lab's alpine (the image the
// workspaces proof's actors run, pinned by digest) whose busybox nc serves
// HTTP/1.1 on port 80, on the platform Harness's worker pool, removed by the
// same run. It reaches the router's HTTP port through a port-forward and
// names the actor in the ate-target-actor header, as Substrate's own ingress
// e2e does. --substrate pins the substrate components to a release for the
// run, the way substrate-step-test steps, and returns the lab to the chart's
// Substrate afterwards. docs/platform.md "The Substrate tunnel-close proof".

// What the proof creates; all of it is removed by the same run.
const (
	substrateTunnelTemplate = "agentlab-substrate-tunnel"
	substrateTunnelActor    = "agentlab-substrate-tunnel"
	// substrateTunnelValues is the overlay that pins --substrate, beside the
	// lab's rendered values.
	substrateTunnelValues = "substrate-tunnel-values.yaml"
	// atenetRouterDeployment is Substrate's ingress, its HTTP port the one
	// actors are reached on in the clear.
	atenetRouterDeployment = "atenet-router"
	atenetRouterHTTPPort   = 80
	// targetActorHeader names the actor a request through atenet-router is
	// for: <atespace>/<name>.
	targetActorHeader = "ate-target-actor"
	// The actor's health path and answer, and the path of the gRPC-shaped
	// call.
	substrateTunnelHealthPath = "/readyz"
	substrateTunnelHealthBody = "ok"
	substrateTunnelGRPCPath   = "/agentlab.Tunnel/Close"
	// http2Proto is an HTTP/2 answer's protocol, as net/http reports it.
	http2Proto = "HTTP/2.0"
)

// The waits of the proof.
const (
	// SubstrateTunnelClientTimeout is the client's timeout on the gRPC-shaped
	// call: the router's wait for the actor's SETTINGS frame ends only with it
	// on a relay that keeps the tunnel open.
	SubstrateTunnelClientTimeout = 30 * time.Second
	// SubstrateTunnelPromptly is how soon the call must fail to have failed
	// at once: the tunnel ended with the actor's connection.
	SubstrateTunnelPromptly = 5 * time.Second
	// SubstrateTunnelReadyTimeout bounds the golden boot, the actor's resume
	// and its route reaching the router.
	SubstrateTunnelReadyTimeout = 5 * time.Minute
	substrateTunnelRoutePoll    = 2 * time.Second
)

// substrateTunnelActorScript is the actor: busybox nc on port 80, a fresh
// handler per connection. A request line of HTTP/1.x is answered 200 on
// substrateTunnelHealthPath, 404 elsewhere. On the HTTP/2 preface (PRI *
// HTTP/2.0) it closes at once without an answer, the rest of the preface and
// the SETTINGS frame unread, which resets the connection: the case of an
// HTTP/1.1-only server whose answer does not reach the router. An answer that
// does reach it fails the call there whatever the relay does; a reset leaves
// the router waiting on the tunnel unless the relay ends it.
const substrateTunnelActorScript = `cat > /tmp/serve <<'EOF'
read -r method path proto
[ "$method" = PRI ] && exit 0
while read -r line; do [ "${#line}" -le 1 ] && break; done
if [ "$path" = ` + substrateTunnelHealthPath + ` ]; then
  printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 3\r\nConnection: close\r\n\r\n` + substrateTunnelHealthBody + `\n'
else
  printf 'HTTP/1.1 404 Not Found\r\nConnection: close\r\nContent-Length: 0\r\n\r\n'
fi
EOF
exec nc -lk -p 80 -e sh /tmp/serve
`

// SubstrateTunnelOptions tunes the proof.
type SubstrateTunnelOptions struct {
	// Substrate pins the substrate components to an exact release for the
	// run (1.11.3, 1.11.4-rc.1); empty runs on the lab's Substrate.
	Substrate string
	// ClientTimeout is the gRPC-shaped call's client timeout, Promptly how
	// soon it must fail.
	ClientTimeout, Promptly time.Duration
	// ReadyTimeout bounds the golden boot, the resume and the route.
	ReadyTimeout time.Duration
	// Stay leaves the lab on Substrate instead of returning it to the
	// chart's.
	Stay bool
}

// SubstrateTunnelTest is `agentlab substrate-tunnel-test`. The verdict is one
// PASS line per check and a FAIL line with the cause, under the summary of
// the stages.
func SubstrateTunnelTest(cfg *config.Config, opts SubstrateTunnelOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents || cfg.LegacyChart() {
		return fmt.Errorf("platform.agents is off in %s (or the chart is a 3.x one): the agents of the 4.x line run on Agent Substrate — enable them and run `agentlab platform` first", config.File)
	}
	if opts.ClientTimeout <= 0 {
		opts.ClientTimeout = SubstrateTunnelClientTimeout
	}
	if opts.Promptly <= 0 {
		opts.Promptly = SubstrateTunnelPromptly
	}
	if opts.Promptly >= opts.ClientTimeout {
		return fmt.Errorf("--promptly %s must be shorter than --client-timeout %s: a call that fails only with its client failed late", opts.Promptly, opts.ClientTimeout)
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = SubstrateTunnelReadyTimeout
	}
	run := &substrateTunnelRun{cfg: cfg, opts: opts, stages: newProofStages()}
	if opts.Substrate != "" {
		if cfg.Platform.ChartBranch != "" && !cfg.Platform.ChartPinned {
			return fmt.Errorf("the lab follows the dev channel of %s, which a platform run in the middle of the proof would move: `agentlab platform --pin` first", cfg.Platform.ChartBranch)
		}
		to, err := semver.StrictNewVersion(strings.TrimPrefix(opts.Substrate, "v"))
		if err != nil {
			return fmt.Errorf("--substrate %q: an exact Substrate release (1.11.4, 1.11.4-rc.1): %v", opts.Substrate, err)
		}
		run.to = to
		if run.pinned, err = substrateStepConfig(cfg, to.String(), filepath.Join(StateDir, substrateTunnelValues), nil); err != nil {
			return err
		}
	}
	err := run.prove()
	if run.pinTaken && !opts.Stay {
		if backErr := returnToChartSubstrate(cfg, run.before.version, substrateTunnelValues); backErr != nil {
			err = errors.Join(err, backErr)
		}
	}
	err = run.stages.close(err)
	run.verdict(err)
	return err
}

// substrateTunnelRun is one run's state.
type substrateTunnelRun struct {
	cfg, pinned *config.Config
	to          *semver.Version
	opts        SubstrateTunnelOptions
	stages      *proofStages

	before, running substrateRunning
	workers         []string // the images the worker pods run
	pinTaken        bool
	lines           []string
}

func (r *substrateTunnelRun) pass(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	note("%s", line)
	r.lines = append(r.lines, line)
}

// verdict prints the PASS lines and the FAIL line with the cause.
func (r *substrateTunnelRun) verdict(err error) {
	fmt.Println()
	for _, line := range r.lines {
		fmt.Printf("PASS: %s\n", line)
	}
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	}
}

// prove runs the checks up to the first failure.
func (r *substrateTunnelRun) prove() error {
	r.stages.begin("substrate", "Step 1: the Substrate the proof runs on")
	before, err := readSubstrateRelease(r.cfg)
	if err != nil {
		return err
	}
	r.before, r.running = before, before
	if r.workers, err = workerPodImages(); err != nil {
		return err
	}
	if err := r.pin(); err != nil {
		return err
	}
	r.pass("on %s; the worker pods, where atunnel relays the tunnel, run %s", r.running, strings.Join(r.workers, ", "))

	ctx := context.Background()
	api, err := dialAteAPI(ctx)
	if err != nil {
		return err
	}
	defer api.Close()
	removeSubstrateTunnelActor(ctx, api, r.opts.ReadyTimeout)
	defer removeSubstrateTunnelActor(context.Background(), api, r.opts.ReadyTimeout)

	r.stages.begin("actor", "Step 2: an HTTP/1.1-only actor (busybox nc on port 80) on the platform Harness's worker pool")
	if err := r.startActor(ctx, api); err != nil {
		return err
	}

	router, stop, err := forwardAtenetRouter(ctx)
	if err != nil {
		return err
	}
	defer stop()
	target := kagentNamespace + "/" + substrateTunnelActor

	r.stages.begin("route", "Step 3: HTTP/1.1 and HTTP/2 (h2c) through atenet-router reach the actor, downgraded to HTTP/1.1")
	if err := r.route(ctx, router, target); err != nil {
		return err
	}

	r.stages.begin("tunnel-close", "Step 4: a gRPC call (h2c, application/grpc) through atenet-router to the actor fails within %s, not with its client's %s timeout", r.opts.Promptly, r.opts.ClientTimeout)
	return r.tunnelClose(ctx, router, target)
}

// pin moves the lab onto --substrate for the run when it runs another
// release.
func (r *substrateTunnelRun) pin() error {
	if r.to == nil {
		return nil
	}
	if r.before.version == r.to.String() && r.workersOn(r.to.String()) {
		note("the lab runs Substrate %s already, its worker pods included: no pin", r.to)
		return nil
	}
	values, err := substrateStepOverlay(r.to.String())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	overlay := filepath.Join(StateDir, substrateTunnelValues)
	if err := os.WriteFile(overlay, values, 0o600); err != nil {
		return err
	}
	note("the overlay %s pins %s and %s to %s (the lab runs %s)", overlay, substrateCRDsRelease, substrateRelease, r.to, r.before.version)
	r.pinTaken = true
	if err := clearRustfsServiceAccount(r.to.String()); err != nil {
		return err
	}
	if err := PlatformUp(r.pinned, Offers{}); err != nil {
		return fmt.Errorf("the platform upgrade to Substrate %s: %w", r.to, err)
	}
	running, err := readSubstrateRelease(r.pinned)
	if err != nil {
		return err
	}
	if running.version != r.to.String() {
		return fmt.Errorf("the platform upgrade finished, but the atelet runs %s, not %s: %s", running.version, r.to, running)
	}
	if err := waitWorkersOn(running.worker, substrateStepWorkersTimeout); err != nil {
		return err
	}
	r.running = running
	r.workers, err = workerPodImages()
	return err
}

// workersOn tells whether every worker pod runs the release: atunnel, the
// relay under proof, runs in the worker image.
func (r *substrateTunnelRun) workersOn(release string) bool {
	for _, image := range r.workers {
		if tag, err := imageTag(image); err != nil || tag != release {
			return false
		}
	}
	return len(r.workers) > 0
}

// workerPodImages is every distinct image the worker pods in the kagent
// namespace run, sorted. The WorkerPool's spec.workerImage stays at the
// chart's floor while the controller runs the pods on its own release
// (condition WorkerImageOverridden), so the pods are read, not the spec.
func workerPodImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pods, err := listObjects(ctx, gvrPods, kagentNamespace, substrateWorkerLabel)
	if err != nil {
		return nil, fmt.Errorf("the worker pods in %s: %w", kagentNamespace, err)
	}
	seen := map[string]bool{}
	for _, pod := range pods {
		if pod.GetDeletionTimestamp() != nil {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", podContainersKey)
		for _, c := range containers {
			container, _ := c.(map[string]any)
			if image, _ := container[imageKey].(string); image != "" {
				seen[image] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no worker pod in %s", kagentNamespace)
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// startActor creates the template, waits for its golden, creates the actor
// and resumes it until it runs.
func (r *substrateTunnelRun) startActor(ctx context.Context, api *ateAPI) error {
	rt, err := readWorkspacesActorRuntime(ctx)
	if err != nil {
		return err
	}
	if _, err := api.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: substrateTunnelTemplateFrom(rt)}); err != nil {
		return fmt.Errorf("CreateActorTemplate %s/%s: %w", kagentNamespace, substrateTunnelTemplate, err)
	}
	golden, err := waitSubstrateTunnelGolden(ctx, api, r.opts.ReadyTimeout)
	if err != nil {
		return err
	}
	actor := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: substrateTunnelActor},
		ActorTemplate: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: substrateTunnelTemplate},
	}
	if _, err := api.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: actor}); err != nil {
		return fmt.Errorf("CreateActor %s/%s: %w", kagentNamespace, substrateTunnelActor, err)
	}
	if err := resumeWorkspacesActor(ctx, api, substrateTunnelActor, r.opts.ReadyTimeout); err != nil {
		return err
	}
	r.pass("actor %s/%s running: ActorTemplate %s on %s (golden %s), on the worker pool %s", kagentNamespace, substrateTunnelActor, substrateTunnelTemplate, workspacesActorImage, golden, rt.workerPool)
	return nil
}

// substrateTunnelTemplateFrom is the proof's template: on the runtime's worker
// pool, in the chart's gVisor sandbox, with its snapshot storage, one
// container running substrateTunnelActorScript.
func substrateTunnelTemplateFrom(rt workspacesActorRuntime) *ateapi.ActorTemplate {
	return &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: substrateTunnelTemplate},
		WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{workspacesKagentWorkerPoolLabel: rt.workerPool}},
		SandboxConfig:  &ateapi.SandboxConfig{SandboxClass: ateapi.SandboxClass_SANDBOX_CLASS_GVISOR, ConfigName: workspacesSandboxConfig},
		SnapshotConfig: &ateapi.SnapshotConfig{
			StorageLocation: rt.snapshotLocation,
			OnPause:         ateapi.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit:        ateapi.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
		},
		Containers: []*ateapi.Container{{
			Name:    "http1",
			Image:   workspacesActorImage,
			Command: []string{"sh", "-c", substrateTunnelActorScript},
		}},
	}
}

// waitSubstrateTunnelGolden waits for the template's golden snapshot and
// returns its tag.
func waitSubstrateTunnelGolden(ctx context.Context, api *ateAPI, timeout time.Duration) (string, error) {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: substrateTunnelTemplate}
	var tag string
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		t, err := api.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return false, nil
		}
		golden := t.GetStatus().GetGoldenSnapshotStatus()
		if msg := golden.GetErrorMessage(); msg != "" {
			return false, errors.New(msg)
		}
		tag = golden.GetGoldenTag().GetName()
		return tag != "", nil
	})
	if err != nil {
		return "", fmt.Errorf("the golden snapshot of ActorTemplate %s/%s: %w", kagentNamespace, substrateTunnelTemplate, err)
	}
	return tag, nil
}

// removeSubstrateTunnelActor removes the proof's actor and template, whatever
// state an interrupted run left them in. Best effort.
func removeSubstrateTunnelActor(ctx context.Context, api *ateAPI, timeout time.Duration) {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: substrateTunnelActor}
	if _, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref}); status.Code(err) != codes.NotFound {
		_, _ = api.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref})
		_ = waitWorkspacesActorState(ctx, api, substrateTunnelActor, ateapi.ActorState_ACTOR_STATE_SUSPENDED, timeout)
		_, _ = api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref, AnyState: true})
		_ = waitWorkspacesActorGone(ctx, api, substrateTunnelActor, timeout)
	}
	_, _ = api.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: substrateTunnelTemplate}})
}

// forwardAtenetRouter port-forwards to the HTTP port of atenet-router's
// Service on a Ready pod — the container port the Service's port targets —
// and returns its local base URL.
func forwardAtenetRouter(ctx context.Context) (string, func(), error) {
	k, err := labKube()
	if err != nil {
		return "", nil, err
	}
	svc, err := k.clientset.CoreV1().Services(substrateNamespace).Get(ctx, atenetRouterDeployment, metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("the Service %s/%s: %w", substrateNamespace, atenetRouterDeployment, err)
	}
	pods, err := k.targetPods(ctx, substrateNamespace, "deploy/"+atenetRouterDeployment)
	if err != nil {
		return "", nil, fmt.Errorf("atenet-router: %w", err)
	}
	for i := range pods {
		if !podReady(&pods[i]) {
			continue
		}
		target, err := serviceTargetPort(svc, &pods[i], atenetRouterHTTPPort)
		if err != nil {
			return "", nil, err
		}
		port, stop, err := portForwardPod(ctx, substrateNamespace, pods[i].Name, target)
		if err != nil {
			return "", nil, err
		}
		note("atenet-router: Service port %d (pod %s port %d) on 127.0.0.1:%d", atenetRouterHTTPPort, pods[i].Name, target, port)
		return fmt.Sprintf("http://127.0.0.1:%d", port), stop, nil
	}
	return "", nil, fmt.Errorf("no Ready pod of deployment/%s in %s", atenetRouterDeployment, substrateNamespace)
}

// serviceTargetPort is the pod's container port the Service's port targets:
// a number as it is, a name looked up among the pod's container ports.
func serviceTargetPort(svc *corev1.Service, pod *corev1.Pod, port int32) (int, error) {
	for _, sp := range svc.Spec.Ports {
		if sp.Port != port {
			continue
		}
		if sp.TargetPort.Type == intstr.Int {
			if sp.TargetPort.IntVal == 0 {
				return int(port), nil
			}
			return int(sp.TargetPort.IntVal), nil
		}
		for _, c := range pod.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.Name == sp.TargetPort.StrVal {
					return int(cp.ContainerPort), nil
				}
			}
		}
		return 0, fmt.Errorf("the Service %s port %d targets %q, which no container of pod %s names", svc.Name, port, sp.TargetPort.StrVal, pod.Name)
	}
	return 0, fmt.Errorf("the Service %s has no port %d", svc.Name, port)
}

// h1Client speaks HTTP/1.1; h2cClient cleartext HTTP/2 by prior knowledge, as
// a gRPC client does.
func h1Client(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: timeout}
}

func h2cClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport.Protocols = protocols
	return &http.Client{Transport: transport, Timeout: timeout}
}

// routerAnswer is one request through the router: the answer's protocol,
// status, gRPC status and body, how long it took, or the error.
type routerAnswer struct {
	proto      string
	status     int
	grpcStatus string
	body       string
	took       time.Duration
	err        error
}

func (a routerAnswer) String() string {
	if a.err != nil {
		return fmt.Sprintf("no answer after %s: %v", tookString(a.took), a.err)
	}
	s := fmt.Sprintf("%s %d after %s", a.proto, a.status, tookString(a.took))
	if a.grpcStatus != "" {
		s += ", grpc-status " + a.grpcStatus
	}
	if a.body != "" {
		s += fmt.Sprintf(", body %q", excerpt(a.body, 120))
	}
	return s
}

// routerRequest sends one request for the target actor through the router.
func routerRequest(ctx context.Context, client *http.Client, method, url, target, contentType string) routerAnswer {
	req, err := http.NewRequestWithContext(ctx, method, url, http.NoBody)
	if err != nil {
		return routerAnswer{err: err}
	}
	req.Header.Set(targetActorHeader, target)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return routerAnswer{err: err, took: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	a := routerAnswer{proto: resp.Proto, status: resp.StatusCode, body: strings.TrimSpace(string(body)), took: time.Since(start)}
	if a.grpcStatus = resp.Header.Get("Grpc-Status"); a.grpcStatus == "" {
		a.grpcStatus = resp.Trailer.Get("Grpc-Status")
	}
	return a
}

// route waits for the actor's route over HTTP/1.1, then asks the health path
// over h2c: the router downgrades plain HTTP/2 for an HTTP/1.1-only actor.
func (r *substrateTunnelRun) route(ctx context.Context, router, target string) error {
	health := router + substrateTunnelHealthPath
	h1 := h1Client(10 * time.Second)
	var last routerAnswer
	start := time.Now()
	ready := waitFor(int(r.opts.ReadyTimeout/substrateTunnelRoutePoll), substrateTunnelRoutePoll, func() bool {
		last = routerRequest(ctx, h1, http.MethodGet, health, target, "")
		return last.err == nil && last.status == http.StatusOK && last.body == substrateTunnelHealthBody
	})
	if !ready {
		return fmt.Errorf("GET %s over HTTP/1.1 for %s did not answer 200 %q within %s: %s", substrateTunnelHealthPath, target, substrateTunnelHealthBody, r.opts.ReadyTimeout, last)
	}
	r.pass("HTTP/1.1 GET %s through atenet-router: %s (route ready after %s)", substrateTunnelHealthPath, last, tookString(time.Since(start)))
	h2 := routerRequest(ctx, h2cClient(r.opts.ClientTimeout), http.MethodGet, health, target, "")
	if h2.err != nil || h2.proto != http2Proto || h2.status != http.StatusOK || h2.body != substrateTunnelHealthBody {
		return fmt.Errorf("GET %s over h2c for %s: %s, want HTTP/2.0 200 %q: non-gRPC HTTP/2 must be downgraded for an HTTP/1.1-only actor", substrateTunnelHealthPath, target, h2, substrateTunnelHealthBody)
	}
	r.pass("h2c GET %s through atenet-router: %s (downgraded to HTTP/1.1 at the actor)", substrateTunnelHealthPath, h2)
	return nil
}

// tunnelClose sends the gRPC-shaped call and judges it: HTTP 502, or gRPC
// Unavailable (14), within Promptly.
func (r *substrateTunnelRun) tunnelClose(ctx context.Context, router, target string) error {
	a := routerRequest(ctx, h2cClient(r.opts.ClientTimeout), http.MethodPost, router+substrateTunnelGRPCPath, target, "application/grpc")
	if err := judgeTunnelClose(a, r.opts.Promptly, r.running.version); err != nil {
		return err
	}
	r.pass("tunnel close on Substrate %s: the gRPC call (h2c POST %s, application/grpc) to the HTTP/1.1-only actor failed at once: %s, within %s", r.running.version, substrateTunnelGRPCPath, a, r.opts.Promptly)
	return nil
}

// judgeTunnelClose is the verdict on the gRPC-shaped call: it must fail
// loudly (HTTP 502, or 200 with grpc-status 14) and within promptly; one
// that ends only with the client's timeout is the tunnel the actor's close
// did not end.
func judgeTunnelClose(a routerAnswer, promptly time.Duration, release string) error {
	if a.err != nil {
		if errors.Is(a.err, context.DeadlineExceeded) || strings.Contains(a.err.Error(), "Client.Timeout") {
			return fmt.Errorf("the gRPC call to the HTTP/1.1-only actor waited out its client's timeout (%s) on Substrate %s: the tunnel stayed open after the actor closed its connection (atunnel's relay does not end it): %v", tookString(a.took), release, a.err)
		}
		return fmt.Errorf("the gRPC call to the HTTP/1.1-only actor on Substrate %s: %s, want HTTP 502 or gRPC Unavailable", release, a)
	}
	loud := a.status == http.StatusBadGateway || (a.status == http.StatusOK && a.grpcStatus == "14")
	if !loud {
		return fmt.Errorf("the gRPC call to the HTTP/1.1-only actor on Substrate %s answered %s, want HTTP 502 or gRPC Unavailable: gRPC must not be downgraded to HTTP/1.1", release, a)
	}
	if a.took > promptly {
		return fmt.Errorf("the gRPC call to the HTTP/1.1-only actor on Substrate %s failed loudly but late: %s, want within %s", release, a, promptly)
	}
	return nil
}
