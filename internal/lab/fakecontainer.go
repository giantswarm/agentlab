package lab

import (
	"cmp"
	"context"
	"debug/elf"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/giantswarm/agentlab/internal/config"
)

// A proof's fake of an external API (Slack's Web API, GitHub's REST API) that
// a component's pods call. A pod reaches a service on this host only through
// the host's firewall — a default-deny one drops that traffic — so the fake
// runs as a container on the kind network, the proof's own binary in the
// lab's probe image: pods reach it container to container through a
// selector-less Service (pointFakeService), this host through a port
// published on loopback, and the proof reads what the fake recorded back
// over HTTP.

// The container's own port, where the binary is mounted, and how long its
// health may take.
const (
	fakeContainerPort  = 8080
	fakeBinaryPath     = "/agentlab"
	fakeContainerStart = 30 * time.Second
	// fakeServicePort is the port a proof's fake Service serves on: plain
	// HTTP, as the pods call the API it fakes.
	fakeServicePort = 80
)

// fakeContainer is a fake running as a container on the kind network: pods
// dial podIP on fakeContainerPort, this host hostURL.
type fakeContainer struct {
	name    string
	hostURL string
	podIP   string
	client  *http.Client
}

func (c *fakeContainer) close() { _ = command(dockerBin, "rm", "-f", c.name).Run() }

// fakeContainerSpec is one fake: what it is (for messages), the container
// name's suffix, the agentlab command it runs and its arguments, the flag
// that names another binary ("" for a command without one), and the path its
// health answers 200 on; the image it runs in ("" is the lab's probe image),
// NAME=value pairs of its environment, handed to docker by name so no value
// is on a command line, and bind mounts (<host path>:<container path>[:ro]);
// tls says the fake serves TLS with a leaf of the lab CA, so its health is
// read as the lab's clients read it.
type fakeContainerSpec struct {
	what, suffix, command, binaryFlag, healthPath, image string
	args, env, mounts                                    []string
	tls                                                  bool
}

// startFakeContainer runs `<binary> <command> --listen 0.0.0.0:8080 <args>`
// as the image's entrypoint (the lab's probe image unless the spec names
// one) on the kind network, with its port published on loopback, as the
// caller's uid, and waits for its health; a leftover of an aborted run is
// replaced.
func startFakeContainer(cfg *config.Config, binary string, spec fakeContainerSpec) (*fakeContainer, error) {
	if err := linuxStaticBinary(binary, spec.what, spec.binaryFlag); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if spec.tls {
		var err error
		if client, err = labHTTPClient(10 * time.Second); err != nil {
			return nil, err
		}
	}
	name := cfg.ClusterName + "-" + spec.suffix
	_ = command(dockerBin, "rm", "-f", name).Run()
	flags := []string{"-d", "--rm",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-p", "127.0.0.1::" + strconv.Itoa(fakeContainerPort),
		"-v", binary + ":" + fakeBinaryPath + ":ro",
		"--entrypoint", fakeBinaryPath}
	for _, mount := range spec.mounts {
		flags = append(flags, "-v", mount)
	}
	for _, pair := range spec.env {
		key, _, _ := strings.Cut(pair, "=")
		flags = append(flags, "-e", key)
	}
	args := dockerRun(name, kindDockerNetwork, flags...)
	args = append(args, cmp.Or(spec.image, probeImage), spec.command, "--listen", "0.0.0.0:"+strconv.Itoa(fakeContainerPort))
	args = append(args, spec.args...)
	run := command(dockerBin, args...)
	run.Env = append(os.Environ(), spec.env...)
	if out, err := run.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("starting the container of %s, %s: %w: %s", spec.what, name, err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	c := &fakeContainer{name: name, client: client}
	ip, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, name)
	published, perr := outputQuiet(dockerBin, "port", name, strconv.Itoa(fakeContainerPort)+"/tcp")
	c.podIP = firstIPv4(ip)
	hostPort := publishedLoopback(published)
	if err != nil || perr != nil || c.podIP == "" || hostPort == "" {
		logs, _ := outputQuiet(dockerBin, "logs", name)
		c.close()
		return nil, fmt.Errorf("the container of %s, %s, has no address on network %s (%q) or no published port (%q): %v %v; its log: %s",
			spec.what, name, kindDockerNetwork, strings.TrimSpace(ip), strings.TrimSpace(published), err, perr, excerpt(logs, 300))
	}
	c.hostURL = "http://" + hostPort
	if spec.tls {
		c.hostURL = "https://" + hostPort
	}
	if !waitFor(int(fakeContainerStart/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		resp, err := c.client.Get(c.hostURL + spec.healthPath)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		logs, _ := outputQuiet(dockerBin, "logs", name)
		c.close()
		return nil, fmt.Errorf("the container of %s, %s, did not answer %s%s within %s; its log: %s", spec.what, name, c.hostURL, spec.healthPath, fakeContainerStart, excerpt(logs, 300))
	}
	return c, nil
}

// publishedLoopback is the loopback address `docker port` names for the
// published port.
func publishedLoopback(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if addr := strings.TrimSpace(line); strings.HasPrefix(addr, "127.0.0.1:") {
			return addr
		}
	}
	return ""
}

// linuxStaticBinary refuses a binary the probe image cannot run: one that is
// not a Linux executable (agentlab built for another OS) or that needs a
// dynamic loader (a `go build` with cgo; the image carries no glibc). flag is
// the proof's flag naming another binary, "" for a command without one.
func linuxStaticBinary(path, what, flag string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s runs %s in a Linux container, and it is not a Linux executable (%v): %s a linux/%s agentlab", what, path, err, anotherBinary(flag, "run"), runtime.GOARCH)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s runs %s in a container on the kind network, and it is dynamically linked: build it with CGO_ENABLED=0 (`make build` does), or %s a static agentlab", what, path, anotherBinary(flag, "run"))
		}
	}
	return nil
}

// anotherBinary words how the caller gets another binary in: the proof's
// flag, or running the command from one.
func anotherBinary(flag, verb string) string {
	if flag == "" {
		return verb + " it from"
	}
	return "pass " + flag + " with"
}

// fakePreflightTries is how often the preflight pod reads the health URL,
// two seconds apart: a Service made a moment ago is reachable once kube-proxy
// programmed its ClusterIP and the resolver's cache let go of the name, a
// few seconds on a node whose probe image is already there.
const fakePreflightTries = 15

// fakeServiceForPods points the selector-less Service at the fake's
// container, on servicePort, and proves a pod reaches it through the Service
// before the proof starts anything else — found here in seconds, not as a
// failure at the end of the run. healthURL is the health URL a pod reads,
// answering "ok": the Service's, or a name CoreDNS sends to it; an https one
// is read without verifying the fake's leaf, which the probe image does not
// trust (the pod reaching it is the point here). The returned func removes
// the Service.
func fakeServiceForPods(cfg *config.Config, service, what, ip string, port, servicePort int, healthURL string) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), probePodTimeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	remove, err := pointFakeService(ctx, k, service, what, ip, port, servicePort)
	if err != nil {
		return nil, err
	}
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	fetch := "wget -qO- -T 5"
	if strings.HasPrefix(healthURL, "https://") {
		fetch += " --no-check-certificate"
	}
	// The last read's output is the pod's: "ok", or what the fetch said.
	script := fmt.Sprintf("for i in $(seq %d); do out=$(%s %q 2>&1) && [ \"$out\" = ok ] && echo ok && exit 0; sleep 2; done; echo \"$out\"; exit 1",
		fakePreflightTries, fetch, healthURL)
	out, err := runProbePod(ctx, platformNamespace, service+"-preflight", probeImage,
		[]string{"sh", "-c", script}, probePodTimeout)
	if err == nil && strings.TrimSpace(out) == "ok" {
		note("a pod reaches %s through %s (%s:%d on the %s network)", what, healthURL, ip, port, kindDockerNetwork)
		return remove, nil
	}
	remove()
	return nil, fmt.Errorf("pods cannot reach %s (%s -> %s:%d, a container on the %s network): %s; probe output: %.300s",
		what, healthURL, ip, port, kindDockerNetwork, probeFailure(err, out), strings.TrimSpace(out))
}

// probeFailure words why a probe pod's fetch failed: its error, or what the
// fetch printed instead of the answer.
func probeFailure(err error, out string) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("the fetch answered %q, not ok", excerpt(strings.TrimSpace(out), 80))
}

// pointFakeService creates the selector-less Service (on servicePort) in the
// platform namespace and the EndpointSlice behind it: the fake container's
// address and port. A leftover of an aborted run is replaced; the returned
// func removes both.
func pointFakeService(ctx context.Context, k *kubeClients, service, what, hostIP string, port, servicePort int) (func(), error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("the port %d of %s is no TCP port", port, what)
	}
	if servicePort <= 0 || servicePort > 65535 {
		return nil, fmt.Errorf("the Service port %d of %s is no TCP port", servicePort, what)
	}
	p, sp := int32(port), int32(servicePort)
	services := k.clientset.CoreV1().Services(platformNamespace)
	endpointSlices := k.clientset.DiscoveryV1().EndpointSlices(platformNamespace)
	remove := func() {
		if err := removeFakeService(context.Background(), k, service); err != nil {
			note("cleanup: the Service %s of %s: %v", service, what, err)
		}
	}
	remove()
	labels := map[string]string{managedByLabel: managedByAgentlabValue}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: platformNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "http", Port: sp, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(p),
		}}},
	}
	if _, err := services.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating the Service %s/%s of %s: %w", platformNamespace, service, what, err)
	}
	name, proto := "http", corev1.ProtocolTCP
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: platformNamespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: service, managedByLabel: managedByAgentlabValue}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{hostIP}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &name, Protocol: &proto, Port: &p}},
	}
	if _, err := endpointSlices.Create(ctx, slice, metav1.CreateOptions{}); err != nil {
		remove()
		return nil, fmt.Errorf("creating the EndpointSlice of %s/%s: %w", platformNamespace, service, err)
	}
	note("Service %s/%s → %s:%d (%s)", platformNamespace, service, hostIP, port, what)
	return remove, nil
}

// removeFakeService deletes a fake's Service and EndpointSlice from the
// platform namespace; neither being there is fine.
func removeFakeService(ctx context.Context, k *kubeClients, service string) error {
	for _, err := range []error{
		k.clientset.DiscoveryV1().EndpointSlices(platformNamespace).Delete(ctx, service, metav1.DeleteOptions{}),
		k.clientset.CoreV1().Services(platformNamespace).Delete(ctx, service, metav1.DeleteOptions{}),
	} {
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
