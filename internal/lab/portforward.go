package lab

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// portForwardWait bounds the port-forward's handshake: the apiserver's
// upgrade to SPDY and the kubelet's first stream.
const portForwardWait = 30 * time.Second

// portForwardPod is `kubectl port-forward pod/<name> :<remotePort>` on the
// loopback through the embedded client: the pod's port reachable on a local
// port the kernel picks, for a proof that drives a pod's HTTP surface the lab
// exposes nowhere else (the klaus-gateway component's Slack endpoints,
// klausgatewaytest_component.go). Returns the local port and a stop that
// ends the forward and waits for it; the forward also ends with ctx.
func portForwardPod(ctx context.Context, ns, pod string, remotePort int) (int, func(), error) {
	k, err := labKube()
	if err != nil {
		return 0, nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(k.cfg)
	if err != nil {
		return 0, nil, fmt.Errorf("port-forward to pod %s/%s: %w", ns, pod, err)
	}
	url := k.clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready := make(chan struct{}), make(chan struct{})
	var errOut bytes.Buffer
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", remotePort)}, stop, ready, io.Discard, &errOut)
	if err != nil {
		return 0, nil, fmt.Errorf("port-forward to pod %s/%s: %w", ns, pod, err)
	}
	done := make(chan error, 1)
	go func() { done <- fw.ForwardPorts() }()
	stopFn := sync.OnceFunc(func() {
		close(stop)
		<-done
	})
	select {
	case <-ready:
	case err := <-done:
		return 0, nil, fmt.Errorf("port-forward to pod %s/%s:%d ended before it was ready: %v %s", ns, pod, remotePort, err, excerpt(errOut.String(), 300))
	case <-time.After(portForwardWait):
		stopFn()
		return 0, nil, fmt.Errorf("port-forward to pod %s/%s:%d not ready within %s %s", ns, pod, remotePort, portForwardWait, excerpt(errOut.String(), 300))
	case <-ctx.Done():
		stopFn()
		return 0, nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		stopFn()
		return 0, nil, fmt.Errorf("port-forward to pod %s/%s:%d: no local port (%v)", ns, pod, remotePort, err)
	}
	return int(ports[0].Local), stopFn, nil
}
